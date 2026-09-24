package control

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The live request log end to end: a request is recorded on the write path and
// reaches a reader's open stream without anybody reloading anything.
//
// It runs against a real Postgres because the part worth testing is the part no
// fake has - the notification the batch write sends, the listener that picks it
// up, and the cursor the stream reads from. Set KEERA_TEST_DATABASE_URL to run
// it; without one it skips, exactly as the store's own tests do.

func streamStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("KEERA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set KEERA_TEST_DATABASE_URL to run the control-plane tests that need one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	st, err := store.Open(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		"TRUNCATE usage_events, spend, api_keys, models, teams, orgs RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st, ctx
}

func TestRequestStreamCarriesARequestAsItIsRecorded(t *testing.T) {
	st, ctx := streamStore(t)
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	srv := New(st, nil, nil, nil, Options{OperatorKey: testOperatorKey, Currency: "CHF"},
		slog.New(slog.DiscardHandler))
	listen, stopListening := context.WithCancel(ctx)
	defer stopListening()
	go srv.Run(listen)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+httpx.ControlPrefix+"/v1/requests/stream?org_id=org_1&since=24h", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q; a browser will not open anything else as a stream", ct)
	}

	// The listener has to be on the channel before the write, or the
	// notification it is waiting for is sent to nobody. Nothing in the response
	// says when that happened, so the write is retried until it lands - which
	// is also what the real thing does, one batch at a time.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			_ = st.WriteEvents(ctx, []store.Event{{
				TS: time.Now(), OrgID: "org_1", Alias: "keera-code", Status: 200,
				InputTokens: 1000, OutputTokens: 200,
			}})
		}
	}()

	payload := awaitEvent(t, res.Body, 30*time.Second)
	if len(payload.Data) == 0 {
		t.Fatal("the batch carried no rows")
	}
	if got := payload.Data[0].Alias; got != "keera-code" {
		t.Errorf("alias = %q, want the request that was just recorded", got)
	}
	if payload.NextAfter != payload.Data[0].ID {
		t.Errorf("next_after = %d, want the newest row's id %d - a cursor that lags "+
			"sends the same row again on the next batch",
			payload.NextAfter, payload.Data[0].ID)
	}
	// The counts travel with the rows and are counted over the window rather
	// than accumulated from what was sent, so the number beside "All" on the
	// screen cannot drift from the one a reload would show.
	if payload.Outcomes.Total == 0 || payload.Outcomes.OK == 0 {
		t.Errorf("outcomes = %+v, want the window counted", payload.Outcomes)
	}
}

func TestRequestStreamIsAdministratorOnly(t *testing.T) {
	// The rows name other people's keys and carry text the inference plane
	// wrote, so this is held to what the log itself is held to - and it is a
	// separate route, which is exactly how such a check gets forgotten.
	srv := New(nil, nil, nil, nil, Options{OperatorKey: testOperatorKey},
		slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	res, err := ts.Client().Get(ts.URL + httpx.ControlPrefix + "/v1/requests/stream?org_id=org_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d for a caller with no credential, want 401", res.StatusCode)
	}
}

// batch is one server-sent event's payload, read the way the panel reads it.
type batch struct {
	Data      []store.Request       `json:"data"`
	Outcomes  store.RequestOutcomes `json:"outcomes"`
	NextAfter int64                 `json:"next_after"`
}

// awaitEvent reads the stream until a requests event arrives, ignoring the
// keepalive comments in between.
func awaitEvent(t *testing.T, body interface{ Read([]byte) (int, error) },
	within time.Duration) batch {
	t.Helper()
	type result struct {
		b   batch
		err error
	}
	got := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		named := false
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "event: requests":
				named = true
			case named && strings.HasPrefix(line, "data: "):
				var b batch
				err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &b)
				got <- result{b, err}
				return
			}
		}
		got <- result{err: sc.Err()}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("reading the stream: %v", r.err)
		}
		return r.b
	case <-time.After(within):
		t.Fatal("no request reached the stream; a panel watching this log would " +
			"have shown nothing")
		return batch{}
	}
}
