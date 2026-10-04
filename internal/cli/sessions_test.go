package cli

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// What `keera session list` and `keera failures` print in their first column
// is what `keera session show` takes, so a row can be pasted straight into it.
func TestSessionAndFailureRowsNameWhatKeeraSessionTakes(t *testing.T) {
	var buf bytes.Buffer
	w := newTable(&buf)
	printSessions(w, sessionsResponse{Data: []store.AgentSession{{
		ID: 4711, Key: "pU6F5haDO6VBv8V-H", Requests: 3,
	}}}, time.Hour)
	_ = w.Flush()
	if !strings.Contains(buf.String(), "4711") || strings.Contains(buf.String(), "pU6F5haDO6VBv8V-H") {
		t.Errorf("keera session list should print the request id, not the hash:\n%s", buf.String())
	}

	buf.Reset()
	w = newTable(&buf)
	printFailures(w, failuresResponse{Data: []store.Request{{ID: 815, Status: 502}}},
		store.OutcomeFailed, time.Hour)
	_ = w.Flush()
	if !strings.Contains(buf.String(), "815") {
		t.Errorf("keera failures should print the request id:\n%s", buf.String())
	}
}

// --user takes an email, as `keera key create --user` does.
func TestSessionsTakesAPersonByEmail(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/orgs": oneOrg,
		"GET /v1/users": map[string]any{"data": []map[string]any{
			{"id": "user_1", "email": "ada@example.ch"},
		}},
		"GET /v1/sessions": map[string]any{"data": []any{}},
	})
	if err := sessionCmd(context.Background(), []string{"list", "--user", "Ada@example.ch"}); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(f.request("GET", "/v1/sessions").query)
	if got := q.Get("user_id"); got != "user_1" {
		t.Errorf("user_id = %q, want the id the email belongs to", got)
	}
}

// Grouped by project, key or person, a row is called what people call it.
func TestUsageNamesTheGroups(t *testing.T) {
	res := usageResponse{
		ProjectNames: map[string]string{"project_1": "Payments"},
		KeyNames:     map[string]string{"key_1": "ci"},
		UserNames:    map[string]string{"user_1": "ada@example.ch"},
	}
	for _, c := range []struct{ by, group, want string }{
		{"project", "project_1", "Payments"},
		{"key", "key_1", "ci"},
		{"user", "user_1", "ada@example.ch"},
		{"user", "user_2", "user_2"},
		{"model", "keera-speed", "keera-speed"},
		{"project", "", "(none)"},
	} {
		if got := res.label(c.by, c.group); got != c.want {
			t.Errorf("label(%s, %q) = %q, want %q", c.by, c.group, got, c.want)
		}
	}
}

// A session id straight after 'session' opens it, as it did before 'show'.
func TestSessionTakesAnIdWithoutShow(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"GET /v1/sessions/4711": map[string]any{"session": map[string]any{"id": 4711}},
	})
	if err := sessionCmd(context.Background(), []string{"4711", "--json"}); err != nil {
		t.Fatal(err)
	}
	f.request("GET", "/v1/sessions/4711")
}
