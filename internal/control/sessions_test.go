package control

import (
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// sessionFixture writes one deployment's worth of traffic: two tasks on one
// key, one of which ended on a refusal, and a request that belongs to no
// conversation at all.
func sessionFixture(t *testing.T, st *store.Store) (from, to time.Time) {
	t.Helper()
	ctx, now := t.Context(), time.Now().UTC().Truncate(time.Second)
	if _, err := st.CreateOrg(ctx, "org_1", "Example Bank"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// The team and the key exist as rows, not only as ids on a usage event:
	// narrowing a report to one of them is checked against who owns it, which
	// is what keeps one tenant from reading another's traffic by naming its id.
	if _, err := st.CreateTeam(ctx, "team_1", "org_1", "Payments Platform"); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := st.CreateKey(ctx, store.KeyInfo{
		ID: "key_1", OrgID: "org_1", TeamID: "team_1",
		Alias: "a developer's laptop", Prefix: "keera_sk_ab",
	}, []byte("hash-of-a-key-that-is-32-bytes!!")); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	events := []store.Event{}
	task := store.DerivedSessionKey("aaaaaaaaaaaaaaaa")
	for i := range 3 {
		events = append(events, store.Event{
			TS:    now.Add(-3*time.Hour + time.Duration(i)*time.Minute),
			OrgID: "org_1", TeamID: "team_1", KeyID: "key_1", Alias: "keera-code",
			SessionKey: task, Status: 200, CostMicros: 1000, InputTokens: 500,
			OutputTokens: 50, Latency: 2 * time.Second, TTFT: 300 * time.Millisecond,
		})
	}
	events = append(events,
		store.Event{
			TS:    now.Add(-3*time.Hour + 3*time.Minute),
			OrgID: "org_1", TeamID: "team_1", KeyID: "key_1", Alias: "keera-code",
			SessionKey: task, Status: 402, Error: "your team has spent its month budget",
		},
		store.Event{
			TS: now.Add(-time.Hour), OrgID: "org_1", TeamID: "team_1", KeyID: "key_1",
			Alias: "keera-speed", SessionKey: store.DerivedSessionKey("bbbbbbbbbbbbbbbb"),
			Status: 200, CostMicros: 20, Latency: time.Second,
		},
		store.Event{
			TS: now.Add(-30 * time.Minute), OrgID: "org_1", KeyID: "key_1",
			Alias: "keera-embed", Status: 200, CostMicros: 5,
		})
	if err := st.WriteEvents(ctx, events); err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	return now.Add(-24 * time.Hour), now.Add(time.Minute)
}

func sessionServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	srv := New(st, nil, nil, nil, Options{OperatorKey: testOperatorKey, Currency: "CHF"},
		slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// getJSON reads one control-plane report as the operator key.
func getJSON(t *testing.T, ts *httptest.Server, path string, into any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testOperatorKey)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if into != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
	}
	return res.StatusCode
}

type sessionList struct {
	Data       []store.AgentSession     `json:"data"`
	Totals     store.AgentSessionTotals `json:"totals"`
	GapSeconds int64                    `json:"gap_seconds"`
	Currency   string                   `json:"currency"`
	NextBefore int64                    `json:"next_before"`
	KeyAliases map[string]string        `json:"key_aliases"`
}

func TestSessionsReportTasksWithTheWindowsTotalsAboveThem(t *testing.T) {
	st, _ := streamStore(t)
	from, to := sessionFixture(t, st)
	ts := sessionServer(t, st)

	var got sessionList
	path := httpx.ControlPrefix + "/v1/sessions?org_id=org_1&from=" + from.Format(time.RFC3339) +
		"&to=" + to.Format(time.RFC3339)
	if code := getJSON(t, ts, path, &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(got.Data) != 2 {
		t.Fatalf("%d sessions, want 2 - and nothing for the request that belonged "+
			"to no conversation", len(got.Data))
	}
	if got.Totals.Sessions != 2 || got.Totals.Requests != 5 {
		t.Errorf("totals = %+v, want 2 sessions over 5 requests", got.Totals)
	}
	if got.Totals.Unhappy != 1 {
		t.Errorf("%d unhappy sessions, want the one that ended on a refusal",
			got.Totals.Unhappy)
	}
	// The threshold that produced this grouping travels with it: a reader who
	// cannot see it cannot tell a deployment that cuts tasks at ten minutes
	// from one that cuts them at an hour.
	if got.GapSeconds != int64(store.DefaultSessionGap.Seconds()) {
		t.Errorf("gap = %ds, want the deployment's %s", got.GapSeconds, store.DefaultSessionGap)
	}
	if got.Currency != "CHF" {
		t.Errorf("currency = %q, want CHF", got.Currency)
	}
	// Ids alone are not something anybody can act on.
	if got.KeyAliases == nil {
		t.Error("the report carries no key aliases")
	}

	// The task that ended on the refusal says so in the list, which is what
	// saves opening every session to find the one that died.
	var refused *store.AgentSession
	for i := range got.Data {
		if got.Data[i].LastStatus == 402 {
			refused = &got.Data[i]
		}
	}
	if refused == nil {
		t.Fatal("no session reports having ended on the refusal")
	}
	if refused.Requests != 4 || !strings.Contains(refused.LastError, "budget") {
		t.Errorf("the refused task = %+v, want its four calls and the sentence its "+
			"client was given", refused)
	}
}

func TestSessionsNarrowAndRankAndPage(t *testing.T) {
	st, _ := streamStore(t)
	from, to := sessionFixture(t, st)
	ts := sessionServer(t, st)
	window := "org_id=org_1&from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)

	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"this key", "&key_id=key_1", 2},
		{"one model", "&alias=keera-speed", 1},
		{"a model nothing used", "&alias=keera-frontier", 0},
		{"only the unhappy ones", "&unhappy=1", 1},
		{"ranked by cost", "&sort=cost", 2},
		{"ranked by how many calls", "&sort=requests", 2},
	} {
		var got sessionList
		if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions?"+window+tc.query, &got); code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.name, code)
		}
		if len(got.Data) != tc.want {
			t.Errorf("%s = %d sessions, want %d", tc.name, len(got.Data), tc.want)
		}
	}

	// A cursor is offered under the default ordering, where the id order and
	// the recency order are the same order.
	var page sessionList
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions?"+window+"&limit=1", &page); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(page.Data) != 1 || page.NextBefore != page.Data[0].ID {
		t.Fatalf("a page of one = %+v with cursor %d", page.Data, page.NextBefore)
	}
	var next sessionList
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions?"+window+"&limit=1&before="+
		strconv.FormatInt(page.NextBefore, 10), &next); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(next.Data) != 1 || next.Data[0].ID >= page.Data[0].ID {
		t.Errorf("the second page = %+v, want the older session", next.Data)
	}
	// The totals are on the first page only: they do not change as somebody
	// pages backwards, and they are a second walk over the window.
	if next.Totals.Sessions != 0 {
		t.Errorf("page two carries totals again: %+v", next.Totals)
	}

	// And a ranking nobody offers is a refusal rather than a silent fallback to
	// the default, which would be a screen sorted by something other than what
	// it says.
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions?"+window+"&sort=whatever", nil); code != http.StatusBadRequest {
		t.Errorf("an unknown sort answered %d, want 400", code)
	}
}

func TestOneSessionIsReachedFromAnyRequestInIt(t *testing.T) {
	st, _ := streamStore(t)
	from, to := sessionFixture(t, st)
	ts := sessionServer(t, st)

	// The rows, as the request log hands them over - which is where a reader
	// who wants a task actually starts.
	var log struct {
		Data []store.Request `json:"data"`
	}
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/requests?org_id=org_1&from="+from.Format(time.RFC3339)+
		"&to="+to.Format(time.RFC3339), &log); code != http.StatusOK {
		t.Fatalf("the request log answered %d", code)
	}
	var refusedID, looseID int64
	for _, r := range log.Data {
		if r.Status == 402 {
			refusedID = r.ID
		}
		if r.SessionKey == "" {
			looseID = r.ID
		}
	}
	if refusedID == 0 || looseID == 0 {
		t.Fatal("the request log did not carry the rows this test needs")
	}

	var got struct {
		Session    store.AgentSession `json:"session"`
		Requests   []store.Request    `json:"requests"`
		GapSeconds int64              `json:"gap_seconds"`
	}
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions/"+strconv.FormatInt(refusedID, 10)+
		"?org_id=org_1", &got); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the last request of a task has to be a way "+
			"into it", code)
	}
	if len(got.Requests) != 4 || got.Session.Requests != 4 {
		t.Fatalf("session = %d calls with %d rows, want 4 and 4",
			got.Session.Requests, len(got.Requests))
	}
	// A task is read from its beginning: this is the one log here that does not
	// read newest first.
	if !got.Requests[0].TS.Before(got.Requests[3].TS) {
		t.Errorf("the requests are not oldest-first: %v then %v",
			got.Requests[0].TS, got.Requests[3].TS)
	}
	if got.Session.ID != got.Requests[0].ID {
		t.Errorf("session id = %d, want the opening request's %d",
			got.Session.ID, got.Requests[0].ID)
	}
	if got.GapSeconds == 0 {
		t.Error("the session does not say what threshold grouped it")
	}

	// A request that belonged to no conversation belongs to no session, and an
	// id that is not a number is not one either.
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions/"+strconv.FormatInt(looseID, 10)+
		"?org_id=org_1", nil); code != http.StatusNotFound {
		t.Errorf("a request with no conversation answered %d, want 404", code)
	}
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions/999999?org_id=org_1", nil); code != http.StatusNotFound {
		t.Errorf("an id nothing was recorded under answered %d, want 404", code)
	}
	if code := getJSON(t, ts, httpx.ControlPrefix+"/v1/sessions/not-a-number?org_id=org_1", nil); code != http.StatusBadRequest {
		t.Errorf("an id that is not a number answered %d, want 400", code)
	}
}

// Administrator-only, exactly like the request log these are built from: the
// rows name other people's keys and carry text the inference plane wrote.
func TestAMemberCannotReadTheSessionLog(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{}, slog.New(slog.DiscardHandler))
	member := &authn.Principal{Role: authn.RoleMember, OrgID: "org_1"}

	for name, route := range map[string]handler{
		"the list":    s.sessions,
		"one of them": s.session,
	} {
		w := httptest.NewRecorder()
		route(w, httptest.NewRequest(http.MethodGet, httpx.ControlPrefix+"/v1/sessions", nil), member)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s answered a member %d, want 403", name, w.Code)
		}
	}
}

// A month of agent work is divided by tasks in a spreadsheet, not in a browser.
func TestSessionsCSVCarriesTheCostPerTask(t *testing.T) {
	s := New(nil, nil, nil, nil, Options{Currency: "CHF"}, slog.New(slog.DiscardHandler))
	w := httptest.NewRecorder()
	start := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)

	s.sessionsCSV(w, []store.AgentSession{{
		ID: 41, Key: store.DerivedSessionKey("aaaaaaaaaaaaaaaa"),
		StartedAt: start, EndedAt: start.Add(11*time.Minute + 30*time.Second),
		Requests: 41, OK: 40, Refused: 1, Models: []string{"keera-code", "keera-frontier"},
		InputTokens: 900, OutputTokens: 100, CostMicros: 1_200_000,
		KeyID: "key_1", TeamID: "team_1", LastStatus: 402, LastError: "budget spent",
	}}, groupLabels{
		teams: map[string]string{"team_1": "Payments Platform"},
		keys:  map[string]string{"key_1": "a developer's laptop"},
	})

	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want a header and one task", len(rows))
	}
	row := strings.Join(rows[1], "|")
	for _, want := range []string{
		"11.5", // the minutes it ran
		"41",   // the calls it took
		"keera-code keera-frontier",
		"a developer's laptop", // and not the id of the key
		"Payments Platform",
		"1.20", // what it cost
		"402",  // and how it ended
		"budget spent",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the row %q does not carry %q", row, want)
		}
	}
}
