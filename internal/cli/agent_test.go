package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

// newFakeAgents is newFakeControl with Keera Agents on the same server, and
// a person signed in, since Keera Agents takes no operator key.
func newFakeAgents(t *testing.T, handlers map[string]any) *fakeControl {
	t.Helper()
	f := newFakeControl(t, handlers)
	base := os.Getenv("KEERA_CONTROL_URL")
	f.handlers["GET /v1/me"] = map[string]any{"agents_url": base}
	t.Setenv("KEERA_OPERATOR_KEY", "")
	t.Setenv("KEERA_CONFIG_DIR", t.TempDir())
	if err := saveSignIn(base, signIn{Token: "person-token"}); err != nil {
		t.Fatal(err)
	}
	return f
}

var twoAgents = map[string]any{"data": []map[string]any{
	{
		"id": "agt_1", "name": "Support", "model": "keera-speed", "enabled": true, "state": "running",
		"class": map[string]any{"isolation": "vm", "cpu": "1", "memory": "2Gi", "disk": "5Gi"},
	},
	{"id": "agt_2", "name": "Reports", "model": "keera-speed", "enabled": false},
}}

func TestAgentAskFindsTheAgentByName(t *testing.T) {
	f := newFakeAgents(t, map[string]any{
		"GET /api/v1/agents":              twoAgents,
		"POST /api/v1/agents/agt_1/tasks": map[string]any{"id": "task_1", "state": "queued"},
	})
	out := capture(t, "agent", "ask", "support", "What came in overnight?")
	if got := f.request("POST", "/api/v1/agents/agt_1/tasks").body["prompt"]; got != "What came in overnight?" {
		t.Errorf("sent the prompt %v", got)
	}
	if !strings.Contains(out, "keera task show task_1 --follow") {
		t.Errorf("does not say how to follow the task:\n%s", out)
	}
}

func TestAgentSetSendsOnlyWhatIsGivenAndKeepsTheIsolation(t *testing.T) {
	f := newFakeAgents(t, map[string]any{
		"GET /api/v1/agents":         twoAgents,
		"PATCH /api/v1/agents/agt_1": map[string]any{"id": "agt_1", "name": "Support"},
	})
	capture(t, "agent", "set", "Support", "--cpu", "4", "--people", "ada@example.ch, bob@example.ch")
	body := f.request("PATCH", "/api/v1/agents/agt_1").body
	class, _ := body["class"].(map[string]any)
	if class["cpu"] != "4" || class["isolation"] != "vm" || class["memory"] != "2Gi" {
		t.Errorf("sent the class %v; want 4 CPUs and the rest as it was", class)
	}
	// Naming people means they decide.
	if body["access"] != "people" {
		t.Errorf("sent access %v, want people", body["access"])
	}
	if emails, _ := body["allowed_emails"].([]any); len(emails) != 2 {
		t.Errorf("sent the people %v", body["allowed_emails"])
	}
	for _, left := range []string{"name", "model", "instructions", "builtins", "budget_micros"} {
		if _, sent := body[left]; sent {
			t.Errorf("sent %s, which was not given", left)
		}
	}
}

func TestAgentCreateNeedsAModel(t *testing.T) {
	f := newFakeAgents(t, map[string]any{})
	quiet(t)
	err := Run(t.Context(), []string{"agent", "create", "support"})
	if err == nil || !strings.Contains(err.Error(), "--model") {
		t.Errorf("got %v, want --model asked for", err)
	}
	if f.called("POST", "/api/v1/agents") {
		t.Error("created an agent with no model")
	}
}

func TestAgentsRefuseTheOperatorKey(t *testing.T) {
	newFakeControl(t, map[string]any{})
	err := Run(t.Context(), []string{"agent", "list"})
	if err == nil || !strings.Contains(err.Error(), "keera login") {
		t.Errorf("got %v, want to be sent to keera login", err)
	}
}

func TestPickOneRefusesANameTwoShare(t *testing.T) {
	list := []scheduleEntry{{ID: "sch_1", Name: "Daily"}, {ID: "sch_2", Name: "daily"}}
	idName := func(s scheduleEntry) (string, string) { return s.ID, s.Name }
	if _, err := pickOne(list, "Daily", "schedule", "schedule list", idName); err == nil ||
		!strings.Contains(err.Error(), "by its id") {
		t.Errorf("a shared name: %v", err)
	}
	if s, err := pickOne(list, "sch_2", "schedule", "schedule list", idName); err != nil || s.ID != "sch_2" {
		t.Errorf("by id: %v, %v", s, err)
	}
	if _, err := pickOne(list, "weekly", "schedule", "schedule list", idName); err == nil ||
		!strings.Contains(err.Error(), "keera schedule list") {
		t.Errorf("no such one: %v", err)
	}
}

func TestScheduleCreateSendsTheAgentsID(t *testing.T) {
	f := newFakeAgents(t, map[string]any{
		"GET /api/v1/agents":     twoAgents,
		"POST /api/v1/schedules": map[string]any{"id": "sch_1", "name": "standup", "enabled": true},
	})
	capture(t, "schedule", "create", "standup", "--agent", "reports", "--cron", "0 9 * * 1-5",
		"--prompt", "What happened yesterday?")
	body := f.request("POST", "/api/v1/schedules").body
	if body["agent_id"] != "agt_2" || body["name"] != "standup" || body["cron"] != "0 9 * * 1-5" {
		t.Errorf("sent %v", body)
	}
	if _, sent := body["timezone"]; sent {
		t.Error("sent a time zone nobody gave")
	}
}

func TestScheduleDisableOnlyTurnsItOff(t *testing.T) {
	f := newFakeAgents(t, map[string]any{
		"GET /api/v1/schedules":         map[string]any{"data": []map[string]any{{"id": "sch_1", "name": "standup"}}},
		"PATCH /api/v1/schedules/sch_1": map[string]any{"id": "sch_1", "name": "standup"},
	})
	capture(t, "schedule", "disable", "standup")
	body := f.request("PATCH", "/api/v1/schedules/sch_1").body
	if len(body) != 1 || body["enabled"] != false {
		t.Errorf("sent %v, want only enabled false", body)
	}
}

func TestTaskShowFollowsUntilItEnds(t *testing.T) {
	old := pollEvery
	pollEvery = time.Millisecond
	t.Cleanup(func() { pollEvery = old })
	newFakeAgents(t, map[string]any{
		"GET /api/v1/tasks/task_1": map[string]any{
			"id": "task_1", "state": "running", "prompt": "Count the tickets",
			"events": []map[string]any{{"id": 1, "kind": "tool_call", "text": "search\n{...}"}},
		},
		"GET /api/v1/tasks/task_1/events": map[string]any{
			"events": []map[string]any{{"id": 2, "kind": "message", "text": "There are 12."}},
			"task":   map[string]any{"id": "task_1", "state": "succeeded", "result": "12 open tickets"},
		},
	})
	out := capture(t, "task", "show", "task_1", "--follow")
	for _, want := range []string{"Count the tickets", "tool call search…", "There are 12.", "12 open tickets"} {
		if !strings.Contains(out, want) {
			t.Errorf("does not print %q:\n%s", want, out)
		}
	}
}

func TestRemoveSSHKeyTakesAFingerprint(t *testing.T) {
	f := newFakeAgents(t, map[string]any{
		"GET /api/v1/me/ssh-keys": map[string]any{"data": []map[string]any{
			{"id": "ssh_1", "name": "laptop", "fingerprint": "SHA256:abc"},
			{"id": "ssh_2", "name": "desktop", "fingerprint": "SHA256:def"},
		}},
		"DELETE /api/v1/me/ssh-keys/ssh_2": map[string]any{},
	})
	capture(t, "workspace", "remove-ssh-key", "SHA256:def")
	if !f.called("DELETE", "/api/v1/me/ssh-keys/ssh_2") {
		t.Error("did not remove the key with that fingerprint")
	}
}

func TestAuditAsksForTheWindowAndTheFilters(t *testing.T) {
	f := newFakeControl(t, map[string]any{
		"GET /v1/audit": map[string]any{"data": []map[string]any{{
			"id": 7, "created_at": "2026-11-18T10:00:00Z", "actor": "ada@example.ch",
			"action": "key.revoke", "target_type": "key", "target_id": "key_1",
			"detail": map[string]any{"name": "laptop"},
		}}},
	})
	out := capture(t, "audit", "--action", "key.revoke", "--since", "7d", "--limit", "1")
	q := f.request("GET", "/v1/audit").query
	for _, want := range []string{"action=key.revoke", "limit=1", "from="} {
		if !strings.Contains(q, want) {
			t.Errorf("the query %q has no %s", q, want)
		}
	}
	for _, want := range []string{"ada@example.ch", "key key_1", `"name":"laptop"`, "raise --limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("does not print %q:\n%s", want, out)
		}
	}
}

func TestAuditRefusesALimitTheServerWouldCut(t *testing.T) {
	f := newFakeControl(t, map[string]any{})
	if err := Run(t.Context(), []string{"audit", "--limit", "9000"}); err == nil {
		t.Error("took --limit 9000")
	}
	if f.called("GET", "/v1/audit") {
		t.Error("asked anyway")
	}
}
