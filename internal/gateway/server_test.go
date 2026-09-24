package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/store"
)

// ---------------------------------------------------------------- test doubles

type fakeSource struct {
	resolved map[string]*policy.Resolved
	models   map[string]policy.Model
	filters  map[string]policy.Filter
	routers  map[string]policy.Router
	mcp      map[string]policy.MCPServer
}

func (f *fakeSource) Resolve(_ context.Context, presented string) (*policy.Resolved, error) {
	if r, ok := f.resolved[presented]; ok {
		return r, nil
	}
	return nil, policy.ErrUnknownKey
}

func (f *fakeSource) Model(alias string) (policy.Model, bool) {
	m, ok := f.models[alias]
	return m, ok
}

func (f *fakeSource) Filter(orgID, name string) (policy.Filter, bool) {
	fl, ok := f.filters[orgID+"/"+name]
	return fl, ok
}

func (f *fakeSource) Router(orgID, alias string) (policy.Router, bool) {
	rt, ok := f.routers[orgID+"/"+alias]
	return rt, ok
}

func (f *fakeSource) MCPServer(alias string) (policy.MCPServer, bool) {
	m, ok := f.mcp[alias]
	return m, ok
}

func (f *fakeSource) Routers(orgID string) []policy.Router {
	out := []policy.Router{}
	for key, rt := range f.routers {
		if strings.HasPrefix(key, orgID+"/") {
			out = append(out, rt)
		}
	}
	slices.SortFunc(out, func(a, b policy.Router) int { return cmp.Compare(a.Alias, b.Alias) })
	return out
}

func (f *fakeSource) Models() []policy.Model {
	out := make([]policy.Model, 0, len(f.models))
	for _, m := range f.models {
		out = append(out, m)
	}
	return out
}

type fakeBudgets struct {
	err     error
	mu      sync.Mutex
	charged int64
	// spent is what Spent reports for every window, which is enough for the
	// header assertions: what varies in them is the limit, not the ledger.
	spent int64
}

func (f *fakeBudgets) Allow([]policy.Scope, time.Time) error { return f.err }

func (f *fakeBudgets) Spent(policy.ScopeType, string, policy.Period, time.Time) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spent
}

func (f *fakeBudgets) Charge(_ []policy.Scope, micros int64, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charged += micros
}

func (f *fakeBudgets) total() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.charged
}

type fakeSink struct {
	mu     sync.Mutex
	events []store.Event
}

func (f *fakeSink) Record(e store.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeSink) last(t *testing.T) store.Event {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		t.Fatal("no usage event was recorded; this request would not be billed or audited")
	}
	return f.events[len(f.events)-1]
}

func (f *fakeSink) all() []store.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Event(nil), f.events...)
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// ------------------------------------------------------------------- harness

const testKey = auth.Prefix + "test"

type harness struct {
	gw      *httptest.Server
	src     *fakeSource
	budgets *fakeBudgets
	sink    *fakeSink
	metrics *metrics.Registry
	// upstream records what the inference plane actually received.
	upstreamBodies chan []byte
	upstreamAuth   chan string
	// srv is the gateway behind gw, for the tests that have to reach past the
	// request path into what this process has measured. Not the same object as
	// server() returns, which is deliberately a fresh one.
	srv *Server
}

// newHarness wires a gateway in front of a stand-in for vLLM.
func newHarness(t *testing.T, backend http.HandlerFunc, models map[string]policy.Model,
	resolved *policy.Resolved) *harness {
	t.Helper()
	return newHarnessWith(t, backend, models, resolved, Options{})
}

// newHarnessWith is the same, for the tests that are about a deployment's
// settings rather than about the request.
func newHarnessWith(t *testing.T, backend http.HandlerFunc, models map[string]policy.Model,
	resolved *policy.Resolved, opts Options) *harness {
	t.Helper()

	h := &harness{
		budgets:        &fakeBudgets{},
		sink:           &fakeSink{},
		metrics:        metrics.New(),
		upstreamBodies: make(chan []byte, 8),
		upstreamAuth:   make(chan string, 8),
	}

	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		select {
		case h.upstreamBodies <- raw:
		default:
		}
		select {
		case h.upstreamAuth <- r.Header.Get("Authorization"):
		default:
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		backend(w, r)
	}))
	t.Cleanup(inference.Close)

	if models == nil {
		models = map[string]policy.Model{
			"keera-code": {
				Alias: "keera-code", Kind: policy.KindChat,
				Backends: []string{inference.URL + "/v1"}, BackendModel: "served-name",
				// 1.00 in, 4.00 per million out.
				InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000,
				MaxContext: 65536, Enabled: true,
			},
		}
	} else {
		for alias, m := range models {
			if len(m.Backends) == 0 {
				m.Backends = []string{inference.URL + "/v1"}
				models[alias] = m
			}
		}
	}
	if resolved == nil {
		resolved = policy.Resolve(
			policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"}, nil, nil, nil)
	}
	h.src = &fakeSource{
		resolved: map[string]*policy.Resolved{testKey: resolved},
		models:   models,
	}

	srv := New(h.src, h.budgets, ratelimit.New(), h.sink, h.metrics, opts,
		slog.New(slog.DiscardHandler))
	h.srv = srv
	h.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(h.gw.Close)
	return h
}

// server is the gateway behind the test listener, for the paths a caller
// reaches without going through the inference routes.
func (h *harness) server() *Server {
	return New(h.src, h.budgets, ratelimit.New(), h.sink, metrics.New(), Options{},
		slog.New(slog.DiscardHandler))
}

// url is the test listener's address for one inference route. A client is
// given the gateway's origin plus httpx.InferencePrefix as its base URL and
// appends the path itself, so the tests below name the path a client names and
// this puts the prefix on - including for /api/hello, where the doubled
// segment is what an Anthropic-shaped client actually sends.
func (h *harness) url(path string) string {
	return h.gw.URL + httpx.InferencePrefix + path
}

func (h *harness) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	return h.postWithKey(t, path, body, testKey)
}

func (h *harness) postWithKey(t *testing.T, path, body, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func jsonBackend(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func sseBackend(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			_ = rc.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	}
}

// --------------------------------------------------------------------- tests

func TestChatCompletionRewritesTheAliasAndAccountsForTheTokens(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`), nil, nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The alias is the client's contract; the model id is the backend's.
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "served-name" {
		t.Errorf("the inference plane was asked for %q, want the backend model name", sent.Model)
	}

	// The client's own credential must never reach the inference plane.
	if got := <-h.upstreamAuth; strings.Contains(got, testKey) {
		t.Errorf("the client's API key was forwarded upstream: %q", got)
	}

	ev := h.sink.last(t)
	if ev.InputTokens != 1000 || ev.OutputTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", ev.InputTokens, ev.OutputTokens)
	}
	if ev.Alias != "keera-code" {
		t.Errorf("the usage event names %q; reports are read by alias", ev.Alias)
	}
	if ev.Estimated {
		t.Error("a reported usage record must not be marked as an estimate")
	}
	// 1000 in at 1.00/M plus 500 out at 4.00/M.
	if want := int64(1000 + 2000); ev.CostMicros != want {
		t.Errorf("cost = %d, want %d", ev.CostMicros, want)
	}
	if h.budgets.total() != ev.CostMicros {
		t.Errorf("charged %d against budgets, want %d", h.budgets.total(), ev.CostMicros)
	}
	if ev.OrgID != "org_1" || ev.TeamID != "team_1" {
		t.Errorf("event = %+v, want the key's org and team for attribution", ev)
	}
}

func TestStreamingHidesTheInjectedUsageChunkButStillBills(t *testing.T) {
	h := newHarness(t, sseBackend(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":2,"total_tokens":32}}`,
	), nil, nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[],"stream":true}`)
	body, _ := io.ReadAll(resp.Body)

	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want \"no\" - without it an ingress buffers the stream", got)
	}
	if strings.Contains(string(body), `"usage"`) {
		t.Errorf("the gateway's own usage chunk reached the client:\n%s", body)
	}
	if !strings.Contains(string(body), `"content":"a"`) {
		t.Errorf("generated content was lost:\n%s", body)
	}

	// The upstream was asked for the usage the client did not know to request.
	if !strings.Contains(string(<-h.upstreamBodies), `"include_usage":true`) {
		t.Error("include_usage was not added, so the stream would have been billed as free")
	}

	ev := h.sink.last(t)
	if !ev.Stream {
		t.Error("the event does not record that this was a stream")
	}
	if ev.InputTokens != 30 || ev.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 30/2", ev.InputTokens, ev.OutputTokens)
	}
	if ev.TTFT <= 0 {
		t.Error("time to first token was not recorded")
	}
}

func TestStreamingPassesThroughAUsageChunkTheClientAskedFor(t *testing.T) {
	h := newHarness(t, sseBackend(
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
	), nil, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[],"stream":true,"stream_options":{"include_usage":true}}`)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"usage"`) {
		t.Errorf("a client that asked for usage did not receive it:\n%s", body)
	}
}

func TestUnknownAndForbiddenModelsAreIndistinguishable(t *testing.T) {
	// Answering 403 for a model that exists but is not permitted would let any
	// key enumerate another team's catalogue.
	restricted := policy.Resolve(
		policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{AllowedModels: []string{"keera-code"}}, nil, nil)

	models := map[string]policy.Model{
		"keera-code":    {Alias: "keera-code", Kind: policy.KindChat, BackendModel: "a", Enabled: true},
		"keera-premium": {Alias: "keera-premium", Kind: policy.KindChat, BackendModel: "b", Enabled: true},
		"keera-embed":   {Alias: "keera-embed", Kind: policy.KindEmbedding, BackendModel: "c", Enabled: true},
		"keera-off":     {Alias: "keera-off", Kind: policy.KindChat, BackendModel: "d"},
	}
	h := newHarness(t, jsonBackend(`{}`), models, restricted)

	for _, alias := range []string{"keera-premium", "keera-nonexistent", "keera-embed", "keera-off"} {
		t.Run(alias, func(t *testing.T) {
			resp := h.post(t, "/v1/chat/completions", `{"model":"`+alias+`","messages":[]}`)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
	// Refused requests are recorded - they are what a developer asks about
	// afterwards - but a request that never reached the inference plane is not
	// billed for anything.
	if h.sink.count() != 4 {
		t.Errorf("recorded %d refusals, want 4; a refusal nobody kept cannot be "+
			"explained to the developer it happened to", h.sink.count())
	}
	for _, ev := range h.sink.all() {
		if ev.Status != http.StatusNotFound {
			t.Errorf("recorded status = %d, want 404", ev.Status)
		}
		if ev.CostMicros != 0 || ev.InputTokens != 0 || ev.OutputTokens != 0 {
			t.Errorf("a refusal was billed: %+v", ev)
		}
	}
}

func TestAuthenticationFailures(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	tests := []struct{ name, key string }{
		{"no key at all", ""},
		{"a key that does not exist", auth.Prefix + "nope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.postWithKey(t, "/v1/chat/completions", `{"model":"keera-code"}`, tc.key)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			var envelope struct {
				Error struct{ Type, Code string } `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				t.Fatalf("the error envelope is not parseable, which reads to a client as a blank failure: %v", err)
			}
			if envelope.Error.Type == "" {
				t.Error("the error envelope has no type")
			}
		})
	}
}

func TestBudgetExceededIsNotRetryable(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	h.budgets.err = &policy.ErrBudgetExceeded{
		Scope:  policy.Scope{Type: policy.ScopeTeam, ID: "team_1", Period: policy.PeriodMonth},
		Spent:  2_000_000,
		Budget: 1_000_000,
	}
	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	// 429 would tell a client to back off and try again against a limit that
	// will not move until the period rolls over.
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", resp.StatusCode)
	}
}

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	limited := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{RPM: func() *int { n := 1; return &n }()}, nil, nil)
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil, limited)

	if resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`); resp.StatusCode != 200 {
		t.Fatalf("the first request should be admitted, got %d", resp.StatusCode)
	}
	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After leaves a client guessing")
	}
}

func TestARefusedRequestDoesNotSpendTheLevelsAboveIt(t *testing.T) {
	// One team is over its own rate limit; another team in the same
	// organisation is not. The refusals must cost the organisation nothing, or
	// one runaway editor drains the ceiling that protects everybody else's.
	rpm := func(n int) *policy.Limits { return &policy.Limits{RPM: &n} }
	org := rpm(3)
	busy := policy.Resolve(policy.Key{ID: "key_a", OrgID: "org_1", TeamID: "team_1"},
		org, rpm(1), nil)
	quiet := policy.Resolve(policy.Key{ID: "key_b", OrgID: "org_1", TeamID: "team_2"},
		org, nil, nil)

	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
		nil, busy)
	const otherKey = testKey + "_b"
	h.src.resolved[otherKey] = quiet

	const body = `{"model":"keera-code","messages":[]}`
	if resp := h.post(t, "/v1/chat/completions", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("the busy team's first request should be admitted, got %d", resp.StatusCode)
	}
	for i := range 5 {
		resp := h.post(t, "/v1/chat/completions", body)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("refusal %d: status = %d, want 429", i, resp.StatusCode)
		}
		if got := resp.Header.Get("X-Keera-RateLimit-Scope"); got != string(policy.ScopeTeam) {
			t.Errorf("the refusal names the %q limit, want the team's", got)
		}
	}

	// Three admitted requests are the organisation's whole minute, and one has
	// been used. The other team must get the remaining two.
	for i := range 2 {
		resp := h.postWithKey(t, "/v1/chat/completions", body, otherKey)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the quiet team's request %d: status = %d, want 200 - the "+
				"organisation was charged for requests it never forwarded",
				i, resp.StatusCode)
		}
	}
	resp := h.postWithKey(t, "/v1/chat/completions", body, otherKey)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: the organisation's own limit should bind now",
			resp.StatusCode)
	}
	if got := resp.Header.Get("X-Keera-RateLimit-Scope"); got != string(policy.ScopeOrg) {
		t.Errorf("the refusal names the %q limit, want the organisation's", got)
	}
}

func TestUpstreamUnreachableIsRecorded(t *testing.T) {
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served",
			// A port nothing listens on.
			Backends: []string{"http://127.0.0.1:1/v1"}, Enabled: true,
		},
	}
	h := newHarness(t, jsonBackend(`{}`), models, nil)
	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.Status != http.StatusBadGateway {
		t.Errorf("the failure was not recorded: %+v", ev)
	}
}

func TestDispatchFailsOverToTheNextBackend(t *testing.T) {
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served", Enabled: true,
		},
	}
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":2,"completion_tokens":2}}`), models, nil)
	// Put a dead backend in front of the live one.
	m := h.src.models["keera-code"]
	m.Backends = append([]string{"http://127.0.0.1:1/v1"}, m.Backends...)
	h.src.models["keera-code"] = m

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 - a dead replica must not fail the request", resp.StatusCode)
	}
}

func TestUpstreamErrorsArePassedThroughUnchanged(t *testing.T) {
	// vLLM's own error text is what tells a developer their prompt was too
	// long. Replacing it with a generic message loses that.
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"maximum context length is 65536 tokens"}}`)
	}
	h := newHarness(t, backend, nil, nil)
	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream's 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "maximum context length") {
		t.Errorf("the upstream's message was lost:\n%s", body)
	}
	if ev := h.sink.last(t); ev.Status != http.StatusBadRequest {
		t.Errorf("a refused request must still be recorded, got %+v", ev)
	}
}

func TestOutputCeilingIsAppliedToTheForwardedRequest(t *testing.T) {
	capped := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{MaxOutputTokens: func() *int { n := 256; return &n }()}, nil, nil)
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil, capped)

	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[],"max_tokens":100000}`)
	var sent struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MaxTokens != 256 {
		t.Errorf("max_tokens reached the backend as %d, want the guardrail ceiling of 256", sent.MaxTokens)
	}
}

func TestListModelsShowsOnlyWhatTheKeyMayUse(t *testing.T) {
	restricted := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{AllowedModels: []string{"keera-code"}}, nil, nil)
	models := map[string]policy.Model{
		"keera-code":    {Alias: "keera-code", Kind: policy.KindChat, BackendModel: "a", MaxContext: 65536, Enabled: true},
		"keera-premium": {Alias: "keera-premium", Kind: policy.KindChat, BackendModel: "b", Enabled: true},
		"keera-off":     {Alias: "keera-off", Kind: policy.KindChat, BackendModel: "c"},
	}
	h := newHarness(t, jsonBackend(`{}`), models, restricted)

	req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID         string `json:"id"`
			MaxContext int    `json:"max_context"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want \"list\" - clients parse the OpenAI shape", out.Object)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "keera-code" {
		t.Fatalf("data = %+v, want only keera-code", out.Data)
	}
	if out.Data[0].MaxContext != 65536 {
		t.Errorf("max_context = %d, want it advertised so clients can size a request",
			out.Data[0].MaxContext)
	}
}

func TestEmbeddingsGoToTheirOwnSurface(t *testing.T) {
	models := map[string]policy.Model{
		"keera-embed": {Alias: "keera-embed", Kind: policy.KindEmbedding, BackendModel: "e", Enabled: true},
	}
	h := newHarness(t, jsonBackend(`{"data":[],"usage":{"prompt_tokens":9}}`), models, nil)
	resp := h.post(t, "/v1/embeddings", `{"model":"keera-embed","input":"hello"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.InputTokens != 9 {
		t.Errorf("embedding tokens = %d, want 9", ev.InputTokens)
	}
}

func TestMalformedRequestsAreRejectedBeforeTheInferencePlane(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	tests := []struct {
		name, body string
	}{
		{"not JSON", `{"model":`},
		{"no model", `{"messages":[]}`},
		{"an empty model", `{"model":"","messages":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(t, "/v1/chat/completions", tc.body)
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Errorf("status = %d, want a 4xx", resp.StatusCode)
			}
		})
	}
	select {
	case body := <-h.upstreamBodies:
		t.Errorf("a malformed request reached the inference plane: %s", body)
	default:
	}
}

func TestACancelledStreamIsStillCharged(t *testing.T) {
	// A coding agent cancelling a turn is routine. The tokens were generated
	// and the GPU time was spent, so the request must not vanish from the
	// ledger - otherwise any budget can be evaded by always cancelling.
	released := make(chan struct{})
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for range 3 {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"}}]}`+"\n\n")
			_ = rc.Flush()
		}
		<-released // the usage chunk never arrives, because the client leaves first
	}
	h := newHarness(t, backend, nil, nil)
	t.Cleanup(func() { close(released) })

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"keera-code","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading the first chunk: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for h.sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ev := h.sink.last(t)
	if !ev.Canceled {
		t.Error("the event does not record that the client disconnected")
	}
	if !ev.Estimated {
		t.Error("token counts inferred from a cut-short stream must be marked as estimates")
	}
	if ev.OutputTokens == 0 {
		t.Error("generated tokens were not counted, so the request would be free")
	}
	if ev.InputTokens == 0 {
		t.Error("the prompt was not estimated, so the input side would be free")
	}
	if h.budgets.total() == 0 {
		t.Error("nothing was charged against the budget for a cancelled stream")
	}
}

func TestTheOutputCeilingDoesNotTouchEmbeddings(t *testing.T) {
	// An embedding request has no output length, and max_tokens is a field the
	// backend does not expect on that surface.
	capped := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{MaxOutputTokens: func() *int { n := 256; return &n }()}, nil, nil)
	models := map[string]policy.Model{
		"keera-embed": {Alias: "keera-embed", Kind: policy.KindEmbedding, BackendModel: "e", Enabled: true},
	}
	h := newHarness(t, jsonBackend(`{"data":[],"usage":{"prompt_tokens":4}}`), models, capped)

	h.post(t, "/v1/embeddings", `{"model":"keera-embed","input":"hello"}`)
	sent := <-h.upstreamBodies
	if strings.Contains(string(sent), "max_tokens") {
		t.Errorf("max_tokens was added to an embedding request: %s", sent)
	}
}

func TestServeChatEnforcesForACallerWithNoKey(t *testing.T) {
	// The control panel's playground authorises its own caller and then hands
	// the request here. It must land on exactly the path a key takes: the
	// alias is rewritten, the allow-list holds, and the request is billed.
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 4_000_000, Enabled: true,
		},
		"keera-premium": {
			Alias: "keera-premium", Kind: policy.KindChat, BackendModel: "premium", Enabled: true,
		},
	}
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500}}`), models, nil)
	res := policy.ResolveOrg("org_1", "user_7", &policy.Limits{
		AllowedModels: []string{"keera-code"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.server().ServeChat(w, r, res)
	}))
	t.Cleanup(srv.Close)

	post := func(body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	resp := post(`{"model":"keera-code","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var sent map[string]any
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "served-name" {
		t.Errorf("model = %v, want the backend name", sent["model"])
	}

	ev := h.sink.last(t)
	if ev.OrgID != "org_1" || ev.UserID != "user_7" || ev.KeyID != "" {
		t.Errorf("event = %+v; a panel request is the organisation's and the person's", ev)
	}
	if h.budgets.total() == 0 {
		t.Error("a request made from the panel has to be charged like any other")
	}

	// And a model that exists but is outside the organisation's allow-list is
	// refused here too, or the panel would be the way round every guardrail.
	if got := post(`{"model":"keera-premium","messages":[]}`).StatusCode; got != http.StatusNotFound {
		t.Errorf("status = %d for a disallowed alias, want 404", got)
	}
}

// A model pointed at a hosted endpoint carries that endpoint's credential and
// not the developer's. This is the whole mechanism behind `provider:` entries in
// the catalogue, and it is invisible when it goes wrong: the upstream answers
// 401 and the developer reads it as their own key being rejected.
func TestHostedModelSendsTheEndpointsOwnCredential(t *testing.T) {
	seen := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer upstream.Close()

	models := map[string]policy.Model{"keera-frontier": {
		Alias: "keera-frontier", Kind: policy.KindChat,
		Backends: []string{upstream.URL + "/v1"}, BackendModel: "claude-opus-5",
		APIKeyEnv: "ANTHROPIC_API_KEY", Enabled: true,
	}}
	src := &fakeSource{
		resolved: map[string]*policy.Resolved{testKey: policy.Resolve(
			policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)},
		models: models,
	}

	env := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-upstream"}
	srv := New(src, &fakeBudgets{}, ratelimit.New(), &fakeSink{}, metrics.New(),
		Options{APIKeys: func(name string) string { return env[name] }},
		slog.New(slog.DiscardHandler))
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	post := func(t *testing.T) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gw.URL+httpx.InferencePrefix+"/v1/chat/completions",
			strings.NewReader(`{"model":"keera-frontier","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}

	post(t)
	if got := <-seen; got != "Bearer sk-ant-upstream" {
		t.Errorf("upstream saw %q, want the credential from the named variable", got)
	}

	// With the variable unset the request still goes, without a credential, so
	// the endpoint's own 401 is what surfaces rather than a Keera-shaped error
	// about configuration the developer cannot see.
	delete(env, "ANTHROPIC_API_KEY")
	post(t)
	if got := <-seen; got != "" {
		t.Errorf("upstream saw %q, want no credential at all", got)
	}
}

// ------------------------------------------------- what the developer is told

// budgeted is a key with both kinds of limit on it, which is the shape every
// header assertion below needs.
func budgeted() *policy.Resolved {
	month := policy.PeriodMonth
	return policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{
			RPM: new(10), BudgetMicros: new(int64(1_000_000)), BudgetPeriod: &month,
		}, nil, nil)
}

func TestLimitHeadersRideEveryAnswer(t *testing.T) {
	// A developer cannot see a guardrail from inside their editor. If the only
	// time the gateway mentions one is the refusal, the first they know of a
	// budget is the moment their agent stops.
	h := newHarnessWith(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
		nil, budgeted(), Options{Currency: "CHF"})
	h.budgets.spent = 400_000

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"X-Keera-Budget-Scope":           "org",
		"X-Keera-Budget-Limit":           "1.00",
		"X-Keera-Budget-Remaining":       "0.60",
		"X-Keera-Budget-Currency":        "CHF",
		"X-RateLimit-Limit-Requests":     "10",
		"X-RateLimit-Remaining-Requests": "9",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if _, err := time.Parse(time.RFC3339, resp.Header.Get("X-Keera-Budget-Reset")); err != nil {
		t.Errorf("X-Keera-Budget-Reset is not a timestamp a client can act on: %v", err)
	}
}

func TestASpentBudgetSaysWhenItLiftsAndWhereToLook(t *testing.T) {
	h := newHarnessWith(t, jsonBackend(`{}`), nil, budgeted(),
		Options{PanelURL: "https://keera.example.ch"})
	h.budgets.err = &policy.ErrBudgetExceeded{
		Scope:    policy.Scope{Type: policy.ScopeTeam, ID: "team_1", Period: policy.PeriodMonth},
		Spent:    2_000_000,
		Budget:   1_000_000,
		ResetsAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	var envelope struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	// This message is read inside an editor by the person it just stopped, and
	// is the only thing they will be shown. An id and the word "exceeded" leave
	// them with nothing to do but ask somebody.
	for _, want := range []string{"your team", "1 October 2026", "https://keera.example.ch/access"} {
		if !strings.Contains(envelope.Error.Message, want) {
			t.Errorf("the refusal does not say %q: %s", want, envelope.Error.Message)
		}
	}
}

func TestRefusalsAreRecordedWithTheirReason(t *testing.T) {
	// The dashboard counts refusals and the developer's own screen lists them.
	// Both read the usage log, so a refusal that is answered but not written is
	// a support question nobody can answer.
	limited := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{RPM: new(1)}, nil, nil)
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`),
		nil, limited)

	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	ev := h.sink.last(t)
	if ev.Status != http.StatusTooManyRequests {
		t.Errorf("recorded status = %d, want 429", ev.Status)
	}
	if ev.Alias != "keera-code" || ev.KeyID != "key_1" {
		t.Errorf("a refusal that names neither the model nor the key cannot be "+
			"attributed to anyone: %+v", ev)
	}
	if ev.CostMicros != 0 {
		t.Errorf("a refusal was billed %d", ev.CostMicros)
	}
}

func TestTheSystemPromptReachesTheInferencePlaneAheadOfTheConversation(t *testing.T) {
	// The org's wording, then the team's, then the client's own system message.
	// Order is the assertion: a guardrail that lands after what it is guarding
	// against is a suggestion.
	res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"},
		&policy.Limits{SystemPrompt: new("Answer in British English.")},
		&policy.Limits{SystemPrompt: new("Never suggest a new dependency.")}, nil)
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil, res)

	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[`+
		`{"role":"system","content":"You are a poet."},{"role":"user","content":"hi"}]}`)

	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(<-h.upstreamBodies, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("the backend received %d messages, want 3: %+v", len(sent.Messages), sent.Messages)
	}
	if sent.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", sent.Messages[0].Role)
	}
	want := "Answer in British English.\n\nNever suggest a new dependency."
	if sent.Messages[0].Content != want {
		t.Errorf("first message = %q, want %q", sent.Messages[0].Content, want)
	}
	if sent.Messages[1].Content != "You are a poet." || sent.Messages[2].Content != "hi" {
		t.Errorf("the client's own conversation did not survive: %+v", sent.Messages[1:])
	}
}

func TestAKeyWithNoSystemPromptSendsTheBodyUntouched(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil, nil)
	const conversation = `{"role":"user","content":"hi"}`
	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[`+conversation+`]}`)

	got := string(<-h.upstreamBodies)
	if !strings.Contains(got, `"messages":[`+conversation+`]`) {
		t.Errorf("a request under no prompt guardrail was rewritten: %s", got)
	}
}

func TestASystemPromptIsNotAddedToEmbeddings(t *testing.T) {
	// An embedding has no conversation and no instruction to follow. Adding a
	// messages array to one would be a field the backend does not expect.
	res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{SystemPrompt: new("Answer in British English.")}, nil, nil)
	models := map[string]policy.Model{
		"keera-embed": {
			Alias: "keera-embed", Kind: policy.KindEmbedding,
			BackendModel: "served-embed", Enabled: true,
		},
	}
	h := newHarness(t, jsonBackend(`{"data":[],"usage":{"prompt_tokens":8}}`), models, res)

	resp := h.post(t, "/v1/embeddings", `{"model":"keera-embed","input":"hello"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := string(<-h.upstreamBodies)
	if strings.Contains(got, "messages") {
		t.Errorf("an embedding request grew a messages array: %s", got)
	}
}

func TestAChatBodyWithNoMessagesArrayIsRefusedWhenAPromptIsSet(t *testing.T) {
	// Forwarding this would hand a client the one body shape that escapes the
	// guardrail, so it is refused here rather than passed on to be rejected
	// upstream for a different reason.
	res := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{SystemPrompt: new("Answer in British English.")}, nil, nil)
	h := newHarness(t, jsonBackend(`{}`), nil, res)

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":"hi"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	select {
	case body := <-h.upstreamBodies:
		t.Errorf("the request reached the inference plane anyway: %s", body)
	default:
	}
	if ev := h.sink.last(t); ev.Status != http.StatusBadRequest {
		t.Errorf("a refused request must still be recorded, got %+v", ev)
	}
}

func TestAnOverLongAliasIsRefusedWithoutBeingRecordedOrQuoted(t *testing.T) {
	// 'model' is free text from the client, and every refusal below it writes a
	// usage row carrying the value and an answer quoting it. An alias the size
	// of a request body is neither, so it is turned away before either happens.
	h := newHarness(t, jsonBackend(`{}`), nil, nil)
	alias := strings.Repeat("a", maxAliasBytes+1)

	resp := h.post(t, "/v1/chat/completions", `{"model":"`+alias+`","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), alias) {
		t.Errorf("the answer quoted the whole alias back: %s", body)
	}
	if ev := h.sink.last(t); ev.Alias != "" {
		t.Errorf("recorded alias is %d bytes; an over-long one must not reach a "+
			"usage row: %q", len(ev.Alias), ev.Alias)
	}
}

func TestUnknownModelsShareOneMetricLabel(t *testing.T) {
	// A metric label lives as long as the process - nothing sweeps the
	// exposition the way the key cache and the rate-limit buckets are swept -
	// so a label taken from 'model' would let any key with no rate limit grow
	// the registry until the process died.
	h := newHarness(t, jsonBackend(`{}`), nil, nil)

	for _, alias := range []string{"nope-1", "nope-2", "nope-3"} {
		if resp := h.post(t, "/v1/chat/completions",
			`{"model":"`+alias+`","messages":[]}`); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", alias, resp.StatusCode)
		}
	}
	// The alias is still on the usage row, which is where a developer's
	// question about it is answered.
	if ev := h.sink.last(t); ev.Alias != "nope-3" {
		t.Errorf("recorded alias = %q, want nope-3", ev.Alias)
	}

	var out strings.Builder
	h.metrics.Write(&out)
	exposition := out.String()
	for _, alias := range []string{"nope-1", "nope-2", "nope-3"} {
		if strings.Contains(exposition, alias) {
			t.Errorf("%q became a metric label; the exposition grows with every "+
				"string a client invents", alias)
		}
	}
	if got := strings.Count(exposition, `keera_requests_total{model="unknown"`); got != 1 {
		t.Errorf("three unknown models produced %d series, want 1", got)
	}
}

func TestAKnownModelKeepsItsMetricLabelWhenRefused(t *testing.T) {
	// The point of the label is that an administrator can see which model is
	// being refused, so a model that exists has to keep its name - the
	// catalogue is what bounds it.
	restricted := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"},
		&policy.Limits{AllowedModels: []string{"keera-code"}}, nil, nil)
	models := map[string]policy.Model{
		"keera-code":    {Alias: "keera-code", Kind: policy.KindChat, BackendModel: "a", Enabled: true},
		"keera-premium": {Alias: "keera-premium", Kind: policy.KindChat, BackendModel: "b", Enabled: true},
	}
	h := newHarness(t, jsonBackend(`{}`), models, restricted)

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-premium","messages":[]}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var out strings.Builder
	h.metrics.Write(&out)
	if !strings.Contains(out.String(), `keera_requests_total{model="keera-premium"`) {
		t.Errorf("a refusal for a model the catalogue holds lost its label:\n%s", out.String())
	}
}

func TestAFailedCallRecordsWhatTheBackendSaid(t *testing.T) {
	// The dashboard's failure count is where an investigation starts. Without
	// the message beside the status it is also where it stops: "400" is both a
	// prompt over the context window and a sampling field this backend does not
	// support, and only one of those is the developer's to fix.
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"CUDA out of memory"}}`)
	}
	h := newHarness(t, backend, nil, nil)
	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)

	ev := h.sink.last(t)
	if ev.Status != http.StatusInternalServerError {
		t.Fatalf("recorded status = %d, want 500", ev.Status)
	}
	if ev.Error != "CUDA out of memory" {
		t.Errorf("the backend's own wording was lost: %q", ev.Error)
	}
	if ev.Alias != "keera-code" || ev.KeyID != "key_1" {
		t.Errorf("a failure that names neither the model nor the key cannot be "+
			"acted on: %+v", ev)
	}
}

func TestAnUnreachableBackendRecordsWhichOneAndWhy(t *testing.T) {
	// What the client is told stays deliberately vague - the deployment's own
	// topology is none of its business. Whoever has to fix it needs exactly the
	// part the client is not given.
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served",
			Backends: []string{"http://127.0.0.1:1/v1"}, Enabled: true,
		},
	}
	h := newHarness(t, jsonBackend(`{}`), models, nil)
	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)

	ev := h.sink.last(t)
	if ev.Status != http.StatusBadGateway {
		t.Fatalf("recorded status = %d, want 502", ev.Status)
	}
	if !strings.Contains(ev.Error, "127.0.0.1:1") {
		t.Errorf("the recorded failure does not name the endpoint that could not be "+
			"reached: %q", ev.Error)
	}
}

func TestARefusalRecordsTheSentenceWithoutThePanelAddress(t *testing.T) {
	// The message on the row is the refusal itself. The "and here is where to
	// go and look" the client is sent is for the client: storing it would put
	// one fixed URL on every refused row in the log.
	h := newHarnessWith(t, jsonBackend(`{}`), nil, budgeted(),
		Options{PanelURL: "https://keera.example.ch"})
	h.budgets.err = &policy.ErrBudgetExceeded{
		Scope:  policy.Scope{Type: policy.ScopeTeam, ID: "team_1", Period: policy.PeriodMonth},
		Spent:  2_000_000,
		Budget: 1_000_000,
	}

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", resp.StatusCode)
	}
	var envelope struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(envelope.Error.Message, "https://keera.example.ch/access") {
		t.Errorf("the client was not told where to look: %s", envelope.Error.Message)
	}

	ev := h.sink.last(t)
	if ev.Error == "" || !strings.Contains(ev.Error, "your team") {
		t.Errorf("the recorded refusal does not say what stopped the request: %q", ev.Error)
	}
	if strings.Contains(ev.Error, "keera.example.ch") {
		t.Errorf("the panel address was stored on the usage row: %q", ev.Error)
	}
}

func TestASuccessfulCallRecordsNoError(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"usage":{"prompt_tokens":2,"completion_tokens":2}}`),
		nil, nil)
	h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if ev := h.sink.last(t); ev.Error != "" {
		t.Errorf("a served request carries a failure message: %q", ev.Error)
	}
}

func TestATranslationFailureIsNotRecordedAsTheBackendsWords(t *testing.T) {
	// The Messages shape turns an unreadable 200 into a 502. The backend said
	// nothing wrong in that case - it answered, and the gateway could not carry
	// the answer - so the row must say that, and must not quote a body that is
	// somebody's completion rather than an error message.
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"not":"a completion","secret":"the customer's prompt"}`)
	}
	h := newHarness(t, backend, nil, nil)
	resp := h.post(t, "/v1/messages",
		`{"model":"keera-code","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	ev := h.sink.last(t)
	if !strings.Contains(ev.Error, "could not translate") {
		t.Errorf("the recorded failure does not say whose it is: %q", ev.Error)
	}
	if strings.Contains(ev.Error, "the customer's prompt") {
		t.Errorf("the response body was stored as though it were an error: %q", ev.Error)
	}
}

// ------------------------------------------------------------------ overhead

// What the gateway adds on its own account is the number an in-path proxy is
// judged on, and keera_request_duration cannot show it: that is almost entirely
// the model, so a gateway that doubled its own cost would not move it visibly.
func TestOverheadIsRecordedForAServedRequest(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`), nil, nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"keera-code","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out strings.Builder
	h.metrics.Write(&out)
	if !strings.Contains(out.String(),
		`keera_gateway_overhead_seconds_count{model="keera-code",org="org_1"} 1`) {
		t.Errorf("the served request added no overhead observation:\n%s", out.String())
	}
}

// A refusal never reached the inference plane, so there is no upstream call for
// its overhead to be overhead of - its whole latency is already on its usage
// row. Counting it here would mix "the gateway is slow" with "the gateway said
// no quickly", and the second is the larger population.
func TestOverheadIsNotRecordedForARefusal(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"no-such-model","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	var out strings.Builder
	h.metrics.Write(&out)
	if strings.Contains(out.String(), "keera_gateway_overhead_seconds_count") {
		t.Errorf("a refusal was counted as gateway overhead:\n%s", out.String())
	}
}

// A provider that served part of the prompt from its own cache charges a
// fraction of the input price for that part, and the row has to say the same
// number its console will. Before the gateway read this field, a coding agent
// resending a stable context every turn was billed several times over for it.
func TestChatCompletionChargesTheCachedPartOfThePromptAtItsOwnRate(t *testing.T) {
	models := map[string]policy.Model{
		"keera-code": {
			Alias: "keera-code", Kind: policy.KindChat, BackendModel: "served-name",
			// 1.00 in, a tenth of that for what came out of the cache, 4.00 out.
			InputMicrosPerMTok:       1_000_000,
			CachedInputMicrosPerMTok: 100_000,
			OutputMicrosPerMTok:      4_000_000,
			MaxContext:               65536, Enabled: true,
		},
	}
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,`+
		`"prompt_tokens_details":{"cached_tokens":900}}}`), models, nil)

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ev := h.sink.last(t)
	// The prompt count stays the whole prompt: the cached part is a share of
	// it, not a third column to be added to it.
	if ev.InputTokens != 1000 || ev.CachedInputTokens != 900 || ev.OutputTokens != 500 {
		t.Errorf("tokens = %d in (%d cached) / %d out, want 1000 (900) / 500",
			ev.InputTokens, ev.CachedInputTokens, ev.OutputTokens)
	}
	// 100 in at 1.00/M, 900 cached at 0.10/M, 500 out at 4.00/M.
	if want := int64(100 + 90 + 2000); ev.CostMicros != want {
		t.Errorf("cost = %d, want %d", ev.CostMicros, want)
	}
	// What the same request cost before the discount was read, which is the
	// number this test exists to stop coming back.
	if ev.CostMicros == int64(1000+2000) {
		t.Error("the whole prompt was charged at the input price; the cached rate was ignored")
	}
	if h.budgets.total() != ev.CostMicros {
		t.Errorf("charged %d against budgets, want %d", h.budgets.total(), ev.CostMicros)
	}
}

// A model with no cached rate charges those tokens at the input price, which
// is what the gateway did before the rate existed. Every model already in a
// deployment's catalogue is one of these, so this is the behaviour an upgrade
// must not change.
func TestChatCompletionChargesCachedTokensAtTheInputRateWhenNoneIsStated(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"id":"1","choices":[{"message":{"content":"hi"}}],`+
		`"usage":{"prompt_tokens":1000,"completion_tokens":500,`+
		`"prompt_tokens_details":{"cached_tokens":900}}}`), nil, nil)

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ev := h.sink.last(t)
	if ev.CachedInputTokens != 900 {
		t.Errorf("cached tokens = %d, want them recorded at 900 even unpriced",
			ev.CachedInputTokens)
	}
	if want := int64(1000 + 2000); ev.CostMicros != want {
		t.Errorf("cost = %d, want the full input price %d", ev.CostMicros, want)
	}
}
