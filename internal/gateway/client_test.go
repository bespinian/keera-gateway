package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Reports are drawn from the usage row, so the client must be on it. The client
// table is tested in internal/connect.
func TestTheUsageEventNamesWhatSentTheRequest(t *testing.T) {
	tests := []struct {
		name   string
		agent  string
		stated string
		want   string
	}{
		{name: "a coding agent", agent: "claude-cli/1.0.83 (external, cli)", want: "claude-code"},
		{name: "an editor and its runtime", agent: "opencode/0.4.2 node/22.3.0", want: "opencode"},
		{
			name:   "a client that says what it is outranks what it looks like",
			agent:  "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0",
			stated: "keera-playground",
			want:   "keera-playground",
		},
		{name: "something nobody has heard of", agent: "brand-new-editor/0.1", want: "brand-new-editor"},
		{name: "a client that says nothing", agent: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), nil, nil)

			req, err := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"),
				strings.NewReader(`{"model":"keera-code","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+testKey)
			req.Header.Set("Content-Type", "application/json")
			// An empty string stops Go's client sending its own.
			req.Header.Set("User-Agent", tc.agent)
			if tc.stated != "" {
				req.Header.Set(connect.ClientHeader, tc.stated)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			if got := h.sink.last(t).Client; got != tc.want {
				t.Errorf("the usage event names the client %q, want %q", got, tc.want)
			}
		})
	}
}

// A refused request still records its client, to answer "which editor is
// getting all the 403s".
func TestARefusedRequestStillNamesItsClient(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	h.budgets.err = &policy.ErrBudgetExceeded{
		Scope:  policy.Scope{Type: policy.ScopeProject, ID: "project_1", Period: policy.PeriodMonth},
		Spent:  2_000_000,
		Budget: 1_000_000,
	}

	req, err := http.NewRequest(http.MethodPost, h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"keera-code","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/0.4.2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if got := h.sink.last(t).Client; got != "opencode" {
		t.Errorf("the refusal was recorded against client %q, want opencode", got)
	}
}
