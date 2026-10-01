package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// subKey is a subscription key, next to the standard testKey.
const subKey = "keera_sk_subscription"

// claudeToken is a Claude sign-in as Claude Code sends it.
const claudeToken = "sk-ant-oat01-secret"

// subscriptionHarness is providerHarness with keera-frontier paid by each
// caller's Claude plan, and a subscription key next to the standard one. The
// backend answers with the plan headers Anthropic sends a signed-in caller.
func subscriptionHarness(t *testing.T, res *policy.Resolved) (*harness, chan seenRequest) {
	t.Helper()
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1783180800")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.63")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Organization-Id", "not-for-the-client")
		_, _ = io.WriteString(w, anthropicAnswer)
	}
	h, seen := providerHarness(t, "anthropic", "claude-opus-5", backend, nil)
	m := h.src.models["keera-frontier"]
	m.Subscription, m.APIKey = true, ""
	h.src.models["keera-frontier"] = m
	if res == nil {
		res = policy.Resolve(policy.Key{ID: "key_sub", OrgID: testOrg, UserID: "usr_1",
			Kind: policy.KeySubscription}, nil, nil, nil)
	}
	h.src.resolved[subKey] = res
	return h, seen
}

// claudeCode sends what Claude Code signed in to a Claude plan sends. Each
// header in extra is set too, and an empty value removes it.
func claudeCode(t *testing.T, h *harness, path, body string, extra map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+claudeToken)
	req.Header.Set(KeyHeader, subKey)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	req.Header.Set("User-Agent", "claude-cli/2.1.300 (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("X-Claude-Code-Session-Id", "session-1")
	for name, value := range extra {
		if value == "" {
			req.Header.Del(name)
		} else {
			req.Header.Set(name, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

const planMessage = `{"model":"keera-frontier","max_tokens":1024,` +
	`"system":[{"type":"text","text":"You are Claude Code"}],` +
	`"messages":[{"role":"user","content":"hi"}]}`

func TestASubscriptionRequestForwardsTheCallersSignIn(t *testing.T) {
	h, seen := subscriptionHarness(t, nil)

	resp := claudeCode(t, h, "/v1/messages", planMessage, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	got := nextSeen(t, seen)
	for name, want := range map[string]string{
		"Authorization":            "Bearer " + claudeToken,
		"Anthropic-Beta":           "oauth-2025-04-20,interleaved-thinking-2025-05-14",
		"Anthropic-Version":        anthropicVersion,
		"User-Agent":               "claude-cli/2.1.300 (external, cli)",
		"X-App":                    "cli",
		"X-Claude-Code-Session-Id": "session-1",
	} {
		if got.header.Get(name) != want {
			t.Errorf("%s forwarded as %q, want %q", name, got.header.Get(name), want)
		}
	}
	// The Keera key is the gateway's business, and there is no key of the
	// organisation's to send.
	for _, name := range []string{KeyHeader, "X-Api-Key"} {
		if v := got.header.Get(name); v != "" {
			t.Errorf("%s = %q was forwarded to Anthropic", name, v)
		}
	}

	// Claude Code reads the plan's limits off the answer.
	if v := resp.Header.Get("Anthropic-Ratelimit-Unified-5h-Utilization"); v != "0.42" {
		t.Errorf("plan header reached the client as %q, want 0.42", v)
	}
	if v := resp.Header.Get("Anthropic-Organization-Id"); v != "" {
		t.Errorf("an upstream header that is not the plan's reached the client: %q", v)
	}

	// The plan paid: no spend, no budget charged, and the API price kept to
	// compare against (100 + 900/10 + 50*4).
	ev := h.sink.last(t)
	if ev.CostMicros != 0 || h.budgets.total() != 0 {
		t.Errorf("cost = %d, charged %d; want nothing, the plan paid", ev.CostMicros, h.budgets.total())
	}
	if ev.ListCostMicros != 390 {
		t.Errorf("list cost = %d, want 390", ev.ListCostMicros)
	}
	if ev.InputTokens != 1000 || ev.OutputTokens != 50 {
		t.Errorf("tokens = %d in, %d out; want 1000, 50", ev.InputTokens, ev.OutputTokens)
	}
	if ev.Plan == nil || ev.Plan.FiveHour == nil || *ev.Plan.FiveHour != 0.42 ||
		ev.Plan.SevenDay == nil || *ev.Plan.SevenDay != 0.63 || ev.Plan.Status != "allowed" {
		t.Errorf("plan usage = %+v, want 5h 0.42, 7d 0.63, allowed", ev.Plan)
	}
	if ev.KeyID != "key_sub" || ev.UserID != "usr_1" {
		t.Errorf("row is for key %q, person %q; want key_sub, usr_1", ev.KeyID, ev.UserID)
	}
}

func TestASubscriptionRequestKeepsClaudeCodesSystemPromptFirst(t *testing.T) {
	// Claude Code's first system block identifies it, and Anthropic removes
	// it only when it comes first, so the guardrail's prompt goes after it.
	prompt := "obey the org"
	res := policy.Resolve(policy.Key{ID: "key_sub", OrgID: testOrg, Kind: policy.KeySubscription},
		&policy.Limits{SystemPrompt: &prompt}, nil, nil)
	h, seen := subscriptionHarness(t, res)

	if resp := claudeCode(t, h, "/v1/messages", planMessage, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var sent struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(nextSeen(t, seen).body), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.System) != 2 || sent.System[0].Text != "You are Claude Code" ||
		sent.System[1].Text != prompt {
		t.Errorf("system blocks = %+v, want Claude Code's first and the guardrail's after", sent.System)
	}
}

func TestEachKindOfKeyReachesOnlyItsOwnModels(t *testing.T) {
	h, _ := subscriptionHarness(t, nil)

	// A subscription key cannot spend the organisation's money.
	resp := claudeCode(t, h, "/v1/messages",
		`{"model":"keera-code","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("subscription key on an organisation model: status %d, want 404", resp.StatusCode)
	}
	// A standard key carries no Claude sign-in to forward.
	resp = h.post(t, "/v1/messages", planMessage)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("standard key on a subscription model: status %d, want 404", resp.StatusCode)
	}

	for key, want := range map[string]string{subKey: "keera-frontier", testKey: "keera-code"} {
		req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
		req.Header.Set(KeyHeader, key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var list struct {
			Data []modelEntry `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&list)
		_ = resp.Body.Close()
		if len(list.Data) != 1 || list.Data[0].ID != want {
			t.Errorf("/v1/models for %s lists %+v, want only %s", key, list.Data, want)
		}
	}
}

func TestASubscriptionKeyGoesInItsOwnHeader(t *testing.T) {
	h, _ := subscriptionHarness(t, nil)
	// In Authorization it pushes out the sign-in it is meant to sit next to.
	resp := claudeCode(t, h, "/v1/messages", planMessage, map[string]string{
		"Authorization": "Bearer " + subKey, KeyHeader: "",
	})
	if _, msg := errorOf(t, resp); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(msg, "this is a subscription key") {
		t.Errorf("status %d: %q; want 401 saying where the key goes", resp.StatusCode, msg)
	}
}

func TestAClaudeSignInWithoutAKeeraKeyIsToldWhatToRun(t *testing.T) {
	// What every request looks like once an administrator has pointed Claude
	// Code here and the person has not set it up yet.
	h, _ := subscriptionHarness(t, nil)
	resp := claudeCode(t, h, "/v1/messages", planMessage, map[string]string{KeyHeader: ""})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(string(body), "keera connect claude-code --subscription") {
		t.Errorf("status %d: %s; want 401 saying what to run", resp.StatusCode, body)
	}
	if strings.Contains(string(body), claudeToken) {
		t.Error("the refusal quotes the caller's Claude sign-in")
	}
}

func TestASubscriptionModelNeedsAClaudeSignInOnTheMessagesAPI(t *testing.T) {
	h, _ := subscriptionHarness(t, nil)

	resp := claudeCode(t, h, "/v1/messages", planMessage, map[string]string{"Authorization": ""})
	if _, msg := errorOf(t, resp); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(msg, "carries no Claude sign-in") {
		t.Errorf("no sign-in: status %d: %q; want 401 asking for the sign-in",
			resp.StatusCode, msg)
	}

	resp = claudeCode(t, h, "/v1/chat/completions",
		`{"model":"keera-frontier","messages":[{"role":"user","content":"hi"}]}`, nil)
	if code, _ := errorOf(t, resp); resp.StatusCode != http.StatusBadRequest ||
		code != "subscription_model" {
		t.Errorf("chat completions: status %d, code %q; want 400 subscription_model",
			resp.StatusCode, code)
	}
}

func TestNoBudgetStopsWhatAPlanPaysFor(t *testing.T) {
	h, _ := subscriptionHarness(t, nil)
	h.budgets.err = &policy.ErrBudgetExceeded{Scope: policy.Scope{Type: policy.ScopeOrg}}
	if resp := claudeCode(t, h, "/v1/messages", planMessage, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200: the organisation's budget is not spent", resp.StatusCode)
	}
}

func TestPlanUsageIsReadOffTheHeaders(t *testing.T) {
	now := time.Now()
	h := http.Header{}
	if p := planUsage(h, now); p != nil {
		t.Errorf("an answer without plan headers gave %+v, want nil", p)
	}
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1783713600")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "not a number")
	p := planUsage(h, now)
	switch {
	case p == nil:
		t.Fatal("no plan usage read")
	case p.FiveHour != nil:
		t.Errorf("an unreadable utilisation was kept: %v", *p.FiveHour)
	case p.SevenDay == nil || *p.SevenDay != 0.5:
		t.Errorf("7d = %v, want 0.5", p.SevenDay)
	case p.SevenDayResetsAt == nil || p.SevenDayResetsAt.Unix() != 1783713600:
		t.Errorf("7d reset = %v, want 1783713600", p.SevenDayResetsAt)
	case !p.UpdatedAt.Equal(now):
		t.Errorf("updated at %v, want %v", p.UpdatedAt, now)
	}
}

// errorOf reads an error answer's code and message. The Anthropic shape has
// no code, as Anthropic's own errors have none.
func errorOf(t *testing.T, resp *http.Response) (code, msg string) {
	t.Helper()
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(errors.Join(err, errors.New(string(raw))))
	}
	return doc.Error.Code, doc.Error.Message
}
