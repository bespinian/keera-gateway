package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// standInKind is which kind of inference plane the stand-in answers as.
type standInKind int

const (
	// servesLogprobs answers the lettered question with the distribution
	// behind its one token, as vLLM and llama.cpp do. It is the default.
	servesLogprobs standInKind = iota
	// namesOnly ignores the logprobs fields and writes the name out, as a
	// hosted provider does.
	namesOnly
)

// routeHarness wires a gateway whose organisation has one router of one route
// step, in front of a stand-in inference plane answering as three models,
// told apart by the backend model name the gateway sends.
//
// decide is what the deciding model answers, given the segments it was shown.
func routeHarness(t *testing.T, decide func(segments []string) string,
	rt policy.Route, resolved *policy.Resolved, kind ...standInKind,
) *harness {
	t.Helper()

	plane := servesLogprobs
	if len(kind) > 0 {
		plane = kind[0]
	}

	backend := guardBackend("picker-served", func(c guardCall) string {
		// The same model can be both a router's and a filter's; on the wire
		// only the format appended to the instruction tells the calls apart.
		// A gate is asked for a verdict, so it is given one.
		answer := decide(c.segments)
		if strings.Contains(c.instruction, "ALLOW") {
			answer = "ALLOW"
		}
		if c.logprobs && plane == servesLogprobs {
			if body, ok := lettered(c.instruction, answer); ok {
				return body
			}
		}
		return chatAnswer(answer, 50, 2)
	}, jsonBackend(hiAnswer))

	if resolved == nil {
		resolved = policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	}
	models := map[string]policy.Model{
		"keera-small": {
			OrgID: "org_1", Alias: "keera-small", Kind: policy.KindChat, BackendModel: "small-served",
			Description:        "a fast local model for short edits",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 1_000_000, Enabled: true,
		},
		"keera-large": {
			OrgID: "org_1", Alias: "keera-large", Kind: policy.KindChat, BackendModel: "large-served",
			Description:        "a hosted model for work that needs reasoning",
			InputMicrosPerMTok: 9_000_000, OutputMicrosPerMTok: 9_000_000, Enabled: true,
		},
		"keera-picker": {
			Alias: "keera-picker", Kind: policy.KindChat, BackendModel: "picker-served",
			InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 2_000_000, Enabled: true,
		},
		"keera-embed": {
			Alias: "keera-embed", Kind: policy.KindEmbedding, BackendModel: "embed-served",
			Enabled: true,
		},
	}
	h := newHarness(t, backend, models, resolved)
	h.withRouters(rt)
	return h
}

// withRouters makes each route step a router of its own, under the router
// alias the step names. None leaves no router.
func (h *harness) withRouters(rts ...policy.Route) {
	h.src.routers = map[string]policy.Router{}
	for _, rt := range rts {
		h.src.routers[rt.OrgID+"/"+rt.Router] = routerOf(rt)
	}
}

// routerOf is the router of one route step that rt reads back as.
func routerOf(rt policy.Route) policy.Router {
	step := rt.Step
	if step == "" {
		step = "route"
	}
	return policy.Router{OrgID: rt.OrgID, Alias: rt.Router, Start: &policy.Step{
		ID: step, Kind: policy.StepRoute, Mode: rt.Mode, Model: rt.Model, Prompt: rt.Prompt,
		Destinations: rt.Destinations, Ceilings: rt.Ceilings, Fallback: rt.Fallback,
	}}
}

// autoRoute is the ordinary shape: two destinations, a fallback, and an
// instruction the stand-in never reads.
func autoRoute() policy.Route {
	return policy.Route{
		OrgID: "org_1", Router: "auto", Model: "keera-picker",
		Prompt:       "Send short edits to the small model and anything else to the large one.",
		Destinations: []string{"keera-small", "keera-large"},
		Fallback:     "keera-small",
	}
}

// picks is a deciding model that always names the same destination.
func picks(alias string) func([]string) string {
	return func([]string) string { return alias }
}

// lettered answers the lettered question as vLLM would: the chosen
// destination's letter, and the distribution it was picked out of. The
// letters are read from the instruction the gateway sent, as a real model
// would read them.
func lettered(instruction, alias string) (string, bool) {
	var chosen string
	var alternatives []string
	for line := range strings.SplitSeq(instruction, "\n") {
		line = strings.TrimSpace(line)
		// "A) keera-small - a fast local model for short edits"
		if len(line) < 3 || line[1] != ')' {
			continue
		}
		letter, rest := line[:1], strings.TrimSpace(line[2:])
		name, _, _ := strings.Cut(rest, " - ")
		if name == alias {
			chosen = letter
			continue
		}
		alternatives = append(alternatives, letter)
	}
	if chosen == "" {
		return "", false
	}
	// A model that knows the answer: almost all the mass on one letter.
	tops := []string{`{"token":"` + chosen + `","logprob":-0.01}`}
	for _, letter := range alternatives {
		tops = append(tops, `{"token":"`+letter+`","logprob":-5.0}`)
	}
	return `{"id":"2","choices":[{"message":{"content":"` + chosen + `"},"logprobs":{"content":[` +
		`{"token":"` + chosen + `","logprob":-0.01,"top_logprobs":[` +
		strings.Join(tops, ",") + `]}]}}],` +
		`"usage":{"prompt_tokens":50,"completion_tokens":1}}`, true
}

func TestRouteStepSendsTheRequestToTheModelItChose(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The decision reaches the backend first, so the second body is the one the
	// chosen model was actually asked.
	<-h.upstreamBodies
	forwarded := string(<-h.upstreamBodies)
	if !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the chosen model: %s", forwarded)
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "auto -> keera-large" {
		t.Errorf("X-Keera-Router = %q, want %q", got, "auto -> keera-large")
	}

	// The usage row is the destination's, so per-model reporting is still
	// about models.
	ev := h.sink.last(t)
	if ev.Alias != "keera-large" {
		t.Errorf("alias = %q, want the destination keera-large", ev.Alias)
	}
	if ev.Router != "auto" || ev.RouterOutcome != store.RouterChose {
		t.Errorf("router = %q/%q, want auto/chose", ev.Router, ev.RouterOutcome)
	}
}

// The answer names the model that answered, not the router, so a developer
// reading it can tell what produced it.
func TestRouteStepAnswerNamesTheModelThatAnswered(t *testing.T) {
	h := routeHarness(t, picks("keera-small"), autoRoute(), nil)

	resp := h.post(t, "/v1/messages",
		`{"model":"auto","max_tokens":16,"messages":[{"role":"user","content":"rename this"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "keera-small" {
		t.Errorf("answer named %q, want the destination keera-small", out.Model)
	}
}

// The decision has to be made on what the client sent, not on what the filters
// left, or a redaction filter would launder a prompt into the hosted model.
func TestRouteStepReadsTheRequestBeforeTheKeysFiltersDo(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact"}

	var seen []string
	h := routeHarness(t, func(segments []string) string {
		seen = segments
		return "keera-small"
	}, autoRoute(), resolved)
	// A filter on the same deciding model, so that both hooks run. It is told
	// apart from the router by what the gateway asks it to produce.
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": {
			OrgID: "org_1", Alias: "redact", Model: "keera-picker",
			Mode: policy.FilterModeGate, Prompt: "Allow everything.",
		},
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"deploy with hunter2"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(seen) != 1 || seen[0] != "deploy with hunter2" {
		t.Errorf("the router was shown %q, want the request as the client sent it", seen)
	}
}

// The destination list is the authority, not the answer. A deciding model that
// names something else has not decided, however plausible the name is.
func TestRouteStepWillNotSendToAModelItWasNotOffered(t *testing.T) {
	h := routeHarness(t, picks("keera-embed"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 by way of the fallback", resp.StatusCode)
	}
	ev := h.sink.last(t)
	if ev.Alias != "keera-small" || ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("alias/outcome = %q/%q, want keera-small/fallback", ev.Alias, ev.RouterOutcome)
	}
}

// An instruction step with one destination, written or left once the others
// are down, has nothing to choose. Its model is never asked, so it cannot
// fail the request, and the request goes straight there.
func TestRouteStepWithOneDestinationDoesNotDecide(t *testing.T) {
	one := autoRoute()
	one.Destinations, one.Fallback = []string{"keera-large"}, ""
	for _, tc := range []struct {
		name       string
		rt         policy.Route
		disable    string
		want, sent string
	}{
		{"written", one, "", "keera-large", "large-served"},
		{"left", autoRoute(), "keera-large", "keera-small", "small-served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := routeHarness(t, picks("keera-small"), tc.rt, nil)
			if tc.disable != "" {
				m := h.src.models[tc.disable]
				m.Enabled = false
				h.src.models[tc.disable] = m
			}

			resp := h.post(t, "/v1/chat/completions",
				`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, tc.sent) {
				t.Errorf("the first request upstream was not to the destination: %s", forwarded)
			}
			if n := len(h.upstreamBodies); n != 0 {
				t.Errorf("%d further requests were sent; the deciding model should not be asked", n)
			}
			if ev := h.sink.last(t); ev.Alias != tc.want || ev.RouterOutcome != store.RouterChose {
				t.Errorf("alias/outcome = %q/%q, want %s/chose", ev.Alias, ev.RouterOutcome, tc.want)
			}
		})
	}
}

// Asking no model does not take away the fallback: when the one destination
// cannot answer, the request goes there, as it would after a failed decision.
func TestRouteStepWithOneDestinationFallsBackWhenItCannotAnswer(t *testing.T) {
	rt := autoRoute()
	rt.Destinations = []string{"keera-large"}
	h := routeHarness(t, picks("keera-large"), rt, nil)
	large := h.src.models["keera-large"]
	large.Enabled = false
	h.src.models["keera-large"] = large

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 by way of the fallback", resp.StatusCode)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "small-served") {
		t.Errorf("the request was not sent to the fallback: %s", forwarded)
	}
	ev := h.sink.last(t)
	if ev.Alias != "keera-small" || ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("alias/outcome = %q/%q, want keera-small/fallback", ev.Alias, ev.RouterOutcome)
	}
	if !strings.Contains(ev.Note, "keera-large is disabled") || ev.Error != "" {
		t.Errorf("note/error = %q/%q, want a note on why the destination was passed over, "+
			"and no error", ev.Note, ev.Error)
	}
}

func TestRouteStepIgnoresTwoDestinationsNamedAtOnce(t *testing.T) {
	h := routeHarness(t, picks("either keera-small or keera-large would do"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 by way of the fallback", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("outcome = %q, want fallback for an answer that chose nothing", ev.RouterOutcome)
	}
}

func TestRouteStepWithNoFallbackRefusesWhatItCannotDecide(t *testing.T) {
	rt := autoRoute()
	rt.Fallback = ""
	h := routeHarness(t, picks("no idea"), rt, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want error", ev.RouterOutcome)
	}
	// The generation happened, so it is charged, or a client could make the
	// router free by talking it into failing.
	if ev.CostMicros == 0 {
		t.Error("the decision that could not be made was not charged")
	}
}

// The other half of the same choice: a router that has somewhere safe to send a
// request it cannot place does so rather than taking a department offline.
func TestRouteStepFallsBackWhenItsModelIsGone(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil)
	delete(h.src.models, "keera-picker")

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ev := h.sink.last(t)
	if ev.Alias != "keera-small" || ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("alias/outcome = %q/%q, want keera-small/fallback", ev.Alias, ev.RouterOutcome)
	}
	if !strings.Contains(ev.Note, "is not one of this organisation's models") || ev.Error != "" {
		t.Errorf("note/error = %q/%q, want a note on why the decision could not be made, "+
			"and no error", ev.Note, ev.Error)
	}
	if ev.RouterMS < 0 {
		t.Errorf("router_ms = %d", ev.RouterMS)
	}
}

// A key that may not use a router is told it does not exist, as for a
// model, so another project's routers are not enumerable through 403s.
func TestRouterIsRefusedToAKeyWhoseAllowListOmitsIt(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.AllowedModels = []string{"keera-small"}
	h := routeHarness(t, picks("keera-large"), autoRoute(), resolved)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Allowing a router allows where it sends. Narrowing a key to the router is
// how a scope is made to route, so destinations outside its allow-list must
// stay reachable.
func TestRouterMayPlaceARequestOutsideTheKeysAllowList(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.AllowedModels = []string{"auto"}
	h := routeHarness(t, picks("keera-large"), autoRoute(), resolved)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.Alias != "keera-large" {
		t.Errorf("alias = %q, want keera-large", ev.Alias)
	}
}

// A router only answers chat requests, so the other surfaces do not know its
// name, as for a chat model named on /v1/embeddings.
func TestRouterIsNotAModelOnTheEmbeddingSurface(t *testing.T) {
	h := routeHarness(t, picks("keera-small"), autoRoute(), nil)

	resp := h.post(t, "/v1/embeddings", `{"model":"auto","input":"hello"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// A model is looked up before a router, so a router cannot take over a
// model's alias.
func TestAModelAliasWinsOverARouterOfTheSameName(t *testing.T) {
	rt := autoRoute()
	rt.Router = "keera-small"
	h := routeHarness(t, picks("keera-large"), rt, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-small","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.Router != "" {
		t.Errorf("router = %q, want the alias to have been served as a model", ev.Router)
	}
}

// The deciding model is told each destination's catalogue description.
func TestTheDecidingModelIsToldWhatEachDestinationIsFor(t *testing.T) {
	h := routeHarness(t, picks("keera-small"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	instruction := string(<-h.upstreamBodies)
	for _, want := range []string{
		"a fast local model for short edits",
		"a hosted model for work that needs reasoning",
		// And the administrator's own instruction, before the format.
		"Send short edits to the small model",
	} {
		if !strings.Contains(instruction, want) {
			t.Errorf("the decision was asked without %q: %s", want, instruction)
		}
	}
}

// A destination the catalogue cannot serve is left out of the choice rather
// than offered and then found unreachable.
func TestADisabledDestinationIsNotOffered(t *testing.T) {
	rt := autoRoute()
	rt.Destinations = append(rt.Destinations, "keera-picker")
	h := routeHarness(t, picks("keera-small"), rt, nil)
	large := h.src.models["keera-large"]
	large.Enabled = false
	h.src.models["keera-large"] = large

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	instruction := string(<-h.upstreamBodies)
	if !strings.Contains(instruction, "keera-small") || strings.Contains(instruction, "keera-large") {
		t.Errorf("the instruction does not offer exactly what is enabled: %s", instruction)
	}
}

// A request the router cannot read is refused, and recorded as an error
// rather than as a choice.
func TestARouterRefusalOfAnUnreadableRequestIsAnError(t *testing.T) {
	h := routeHarness(t, picks("keera-small"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions", `{"model":"auto","messages":"hello"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want error", ev.RouterOutcome)
	}
}

// A client finds a router where it finds models, listed once with where it
// may send.
func TestARouterIsListedWithItsDestinations(t *testing.T) {
	h := routeHarness(t, picks("keera-small"), autoRoute(), nil)

	resp := h.get(t, "/v1/models")

	var out struct {
		Data []struct {
			ID           string   `json:"id"`
			Description  string   `json:"description"`
			Destinations []string `json:"destinations"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, m := range out.Data {
		switch m.ID {
		case "auto":
			listed++
			if len(m.Destinations) != 2 {
				t.Errorf("the router was listed without its destinations: %+v", m)
			}
		case "keera-small":
			// The same description a route step decides on.
			if m.Description != "a fast local model for short edits" {
				t.Errorf("description = %q", m.Description)
			}
		}
	}
	if listed != 1 {
		t.Errorf("auto was listed %d times, want once", listed)
	}
}

func TestChosenDestination(t *testing.T) {
	destinations := []string{"keera-small", "keera-large", "keera-small-eu", "anthropic/keera-large"}
	tests := []struct {
		name   string
		answer string
		want   string
	}{
		{"the name on its own", "keera-large", "keera-large"},
		{"trailing punctuation", "keera-large.", "keera-large"},
		{"fenced, which small models do", "`keera-small`", "keera-small"},
		{"quoted and capitalised", `"Keera-Large"`, "keera-large"},
		{"a sentence around it", "I would send this to keera-large", "keera-large"},
		// The longest match wins, so a name containing another name is not
		// read as the one it contains.
		{"a name that contains another", "keera-small-eu", "keera-small-eu"},
		{"a name with a slash in a sentence", "Send it to anthropic/keera-large.", "anthropic/keera-large"},
		{"two names is not a decision", "keera-small or keera-large", ""},
		{"a name that is not offered", "gpt-4", ""},
		{"prose that names nothing", "it depends on the request", ""},
		{"a name glued to other letters", "xkeera-larges", ""},
		{"nothing at all", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := chosenDestination(tc.answer, destinations)
			if tc.want == "" {
				if ok {
					t.Errorf("chosenDestination(%q) = %q, want no decision", tc.answer, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("chosenDestination(%q) = %q, %v; want %q", tc.answer, got, ok, tc.want)
			}
		})
	}
}

// The window is filled from the end, because a router answers what is being
// asked now.
func TestRouteStepWindowKeepsTheNewestSegments(t *testing.T) {
	long := strings.Repeat("x", routeWindowBytes)
	got := routeWindow([]string{long, "the older question", "the newest question"})
	if len(got) != 2 || got[len(got)-1] != "the newest question" {
		t.Fatalf("routeWindow kept %d segments ending %q", len(got), got[len(got)-1])
	}

	// One segment larger than the whole window is kept whole: half a sentence
	// would mislead more than a long one.
	if got := routeWindow([]string{long + long}); len(got) != 1 {
		t.Errorf("routeWindow dropped an oversized lone segment: %d segments", len(got))
	}
	if got := routeWindow(nil); got != nil {
		t.Errorf("routeWindow(nil) = %v, want nil", got)
	}
}

// chainHarness wires a gateway whose organisation has one fallback step in
// front of a stand-in inference plane. down names the backend models that
// answer with a failure of their own.
func chainHarness(t *testing.T, rt policy.Route, down map[string]int) *harness {
	t.Helper()
	return chainHarnessWith(t, rt, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")
		if status, ok := down[sent.Model]; ok {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"no capacity"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"1","choices":[{"message":{"content":"hi"}}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
}

// chainHarnessWith is chainHarness in front of a backend of the test's own.
func chainHarnessWith(t *testing.T, rt policy.Route, backend http.HandlerFunc) *harness {
	t.Helper()
	models := map[string]policy.Model{
		"keera-small": {
			OrgID: "org_1", Alias: "keera-small", Kind: policy.KindChat, BackendModel: "small-served",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 1_000_000, Enabled: true,
		},
		"keera-large": {
			OrgID: "org_1", Alias: "keera-large", Kind: policy.KindChat, BackendModel: "large-served",
			InputMicrosPerMTok: 9_000_000, OutputMicrosPerMTok: 9_000_000, Enabled: true,
		},
		"keera-spare": {
			OrgID: "org_1", Alias: "keera-spare", Kind: policy.KindChat, BackendModel: "spare-served",
			InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 5_000_000, Enabled: true,
		},
	}
	h := newHarness(t, backend, models,
		policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil))
	h.withRouters(rt)
	return h
}

// chainRoute is the ordinary fallback step: an order, and nothing that reads
// the request.
func chainRoute(destinations ...string) policy.Route {
	if len(destinations) == 0 {
		destinations = []string{"keera-small", "keera-large"}
	}
	return policy.Route{
		OrgID: "org_1", Router: "ha", Mode: policy.RouteModeFallback,
		Destinations: destinations,
	}
}

func TestFallbackStepUsesItsFirstDestination(t *testing.T) {
	h := chainHarness(t, chainRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "small-served") {
		t.Errorf("the request did not go to the first destination: %s", forwarded)
	}
	if n := len(h.upstreamBodies); n != 0 {
		t.Errorf("%d further requests were sent; a destination that answered is the answer", n)
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "ha -> keera-small" {
		t.Errorf("X-Keera-Router = %q, want %q", got, "ha -> keera-small")
	}

	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterChose {
		t.Errorf("outcome = %q, want %q - the first destination answering is this "+
			"router getting what it wanted", ev.RouterOutcome, store.RouterChose)
	}
	// Nothing read the request, so only the answering model's ten input and
	// five output tokens are charged, at a micro each.
	if got := h.budgets.total(); got != 15 {
		t.Errorf("%d micros were charged, want 15 - a fallback step decides nothing, "+
			"so there is no generation of its own to pay for", got)
	}
}

// Each destination that fails hands the request to the next, in the written
// order, until one answers.
func TestFallbackStepMovesToTheNextWhenOneFails(t *testing.T) {
	h := chainHarness(t, chainRoute("keera-small", "keera-spare", "keera-large"),
		map[string]int{"small-served": 503, "spare-served": 502})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the last destination answered", resp.StatusCode)
	}
	for _, want := range []string{"small-served", "spare-served", "large-served"} {
		if got := string(<-h.upstreamBodies); !strings.Contains(got, want) {
			t.Fatalf("destination out of order: wanted %s, got %s", want, got)
		}
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "ha -> keera-large (fallback)" {
		t.Errorf("X-Keera-Router = %q; a client has to be able to see that it is being "+
			"answered by a later choice", got)
	}

	ev := h.sink.last(t)
	if ev.Alias != "keera-large" {
		t.Errorf("alias = %q; the row's tokens and price are the answering model's",
			ev.Alias)
	}
	if ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterFellBack)
	}
	// The request succeeded, so only this row's note says the first
	// destination is down.
	if !strings.Contains(ev.Note, "keera-small answered 503") || ev.Error != "" {
		t.Errorf("note/error = %q/%q, want the failed attempt as a note", ev.Note, ev.Error)
	}
}

func TestFallbackStepServesTheLastFailureWhenNothingAnswers(t *testing.T) {
	h := chainHarness(t, chainRoute(),
		map[string]int{"small-served": 503, "large-served": 500})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	// The last destination's own answer says best why there is no completion.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 - what the last destination said", resp.StatusCode)
	}
	// Not "(fallback)": that says a later destination answered, and none did.
	want := "ha -> keera-large (every destination failed)"
	if got := resp.Header.Get("X-Keera-Router"); got != want {
		t.Errorf("X-Keera-Router = %q, want %q", got, want)
	}
	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want %q - the router ran out of destinations",
			ev.RouterOutcome, store.RouterError)
	}
	if !strings.Contains(ev.Error, "keera-small answered 503") {
		t.Errorf("the earlier failure was not recorded: %q", ev.Error)
	}
}

func TestFallbackStepDoesNotRepeatARefusedRequest(t *testing.T) {
	h := chainHarness(t, chainRoute(), map[string]int{"small-served": 400})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	<-h.upstreamBodies
	if n := len(h.upstreamBodies); n != 0 {
		t.Errorf("the request was sent to %d further destinations; a request that is "+
			"malformed or over a quota is all of those things at the next one too", n)
	}
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterChose {
		t.Errorf("outcome = %q; the destination answered, and what it said about the "+
			"request is not the router failing to place it", ev.RouterOutcome)
	}
}

func TestFallbackStepRefusesWhenNothingCanServe(t *testing.T) {
	h := chainHarness(t, chainRoute("keera-gone", "keera-also-gone"), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if n := len(h.upstreamBodies); n != 0 {
		t.Errorf("%d requests were sent to a model; none of the destinations exists", n)
	}
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterError)
	}
}

func TestFallbackStepSkipsADestinationTheCatalogueCannotServe(t *testing.T) {
	h := chainHarness(t, chainRoute("keera-gone", "keera-large"), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the destination that exists: %s", forwarded)
	}
	// A destination missing from the catalogue is not a failed attempt, so
	// the first real destination counts as chosen.
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterChose {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterChose)
	}
}

// A router that decided does not get a second go: when the chosen model
// answers 500, asking another would answer from a model nothing chose.
func TestInstructionStepDoesNotFailOverFromWhatItChose(t *testing.T) {
	rt := autoRoute()
	h := routeHarness(t, picks("keera-large"), rt, nil)
	h.src.models["keera-large"] = policy.Model{
		Alias: "keera-large", Kind: policy.KindChat, BackendModel: "large-served",
		Backends: h.src.models["keera-large"].Backends, Enabled: true,
	}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	<-h.upstreamBodies // the decision
	<-h.upstreamBodies // the chosen model
	if n := len(h.upstreamBodies); n != 0 {
		t.Errorf("%d further destinations were tried after the router had chosen", n)
	}
}

// measuredRoute is a router that reads nothing and ranks its destinations by
// what this process has measured.
func measuredRoute(mode policy.RouteMode, destinations ...string) policy.Route {
	return policy.Route{
		OrgID: "org_1", Router: "pool", Mode: mode, Destinations: destinations,
	}
}

func TestLatencyStepPlacesTheRequestOnTheFasterDestination(t *testing.T) {
	h := chainHarness(t,
		measuredRoute(policy.RouteModeLatency, "keera-small", "keera-large"), nil)

	// The first destination, which a fallback step would use, is the slow one.
	now := time.Now()
	h.srv.load.observe("org_1/keera-small", 2*time.Second, now)
	h.srv.load.observe("org_1/keera-large", 90*time.Millisecond, now)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the destination answering fastest: %s", forwarded)
	}
	if n := len(h.upstreamBodies); n != 0 {
		t.Errorf("%d further requests were sent; the first one answered", n)
	}
	// On these modes 'chose' means the top-ranked destination, not the first
	// one written.
	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterChose {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterChose)
	}
	// keera-large's ten input and five output tokens at nine micros each:
	// nothing read the request, so the router itself costs nothing.
	if got := h.budgets.total(); got != 135 {
		t.Errorf("%d micros were charged, want 135 - a measured step decides nothing, "+
			"so there is no generation of its own to pay for", got)
	}
}

func TestLeastBusyStepPlacesTheRequestWhereThereIsRoom(t *testing.T) {
	h := chainHarness(t,
		measuredRoute(policy.RouteModeLeastBusy, "keera-small", "keera-large"), nil)

	// Two requests already outstanding against the destination a fallback
	// router would send everything to.
	defer h.srv.load.begin("org_1/keera-small")()
	defer h.srv.load.begin("org_1/keera-small")()

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the destination with room: %s", forwarded)
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "pool -> keera-large" {
		t.Errorf("X-Keera-Router = %q, want %q", got, "pool -> keera-large")
	}
}

func TestAMeasuredStepStillFailsOverToWhatItRankedSecond(t *testing.T) {
	// The ranking is wrong here: the fast one is down. The request must not
	// fail because the router was confident.
	h := chainHarness(t,
		measuredRoute(policy.RouteModeLatency, "keera-small", "keera-large"),
		map[string]int{"large-served": 503})
	now := time.Now()
	h.srv.load.observe("org_1/keera-small", 2*time.Second, now)
	h.srv.load.observe("org_1/keera-large", 90*time.Millisecond, now)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the second-ranked destination answered",
			resp.StatusCode)
	}
	if first := string(<-h.upstreamBodies); !strings.Contains(first, "large-served") {
		t.Errorf("the top-ranked destination was not tried first: %s", first)
	}
	if second := string(<-h.upstreamBodies); !strings.Contains(second, "small-served") {
		t.Errorf("the request did not move on to the next one: %s", second)
	}

	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("outcome = %q, want %q - being answered by something other than what "+
			"the router ranked first is what falling back is", ev.RouterOutcome,
			store.RouterFellBack)
	}
	// The destination that failed is ranked last for a while, so the next
	// request does not walk into it again.
	if !h.srv.load.read("org_1/keera-large", time.Now()).failing {
		t.Error("a destination that answered 503 is still being ranked on its latency; " +
			"the next request would be sent to it too")
	}
}

func TestTheLatencyReadingLeavesOutTimeLostOnFailedDestinations(t *testing.T) {
	const lost = 300 * time.Millisecond
	h := chainHarnessWith(t, chainRoute(), func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), "small-served") {
			time.Sleep(lost)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"no capacity"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"1","choices":[{"message":{"content":"hi"}}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The destination that rescued the request answered at once. Charging it
	// the first one's wait would rank it as slow.
	r := h.srv.load.read("org_1/keera-large", time.Now())
	if !r.known || r.latency >= lost {
		t.Errorf("keera-large measured %v (known %v), want less than the %v lost on keera-small",
			r.latency, r.known, lost)
	}
}

func TestARouterRowSaysHowItEndedWhenTheClientHangsUpFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := chainHarnessWith(t, chainRoute(), func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// No destination answered, so this is not 'chose'.
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterError)
	}
	// The client left, the destination did not fail: a measured step must
	// not rank it last for it.
	if r := h.srv.load.read("org_1/keera-small", time.Now()); r.failing {
		t.Error("keera-small is marked failing because the client hung up")
	}
}

// A client that hangs up while the model decides got no answer, so the row
// says error, not chose.
func TestARouterRowSaysErrorWhenTheClientHangsUpWhileItDecides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := routeHarness(t, func([]string) string {
		cancel()
		// Long enough for the gateway to notice and give up on the decision.
		time.Sleep(200 * time.Millisecond)
		return "keera-large"
	}, autoRoute(), nil)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterError)
	}
}

func TestAMeasuredStepChargesNoRoutingTime(t *testing.T) {
	h := chainHarness(t,
		measuredRoute(policy.RouteModeLeastBusy, "keera-small", "keera-large"), nil)

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	<-h.upstreamBodies

	// router_ms is what the router added over the answering model's own
	// work, and a router that neither generates nor retries adds nothing.
	if ev := h.sink.last(t); ev.RouterMS != 0 {
		t.Errorf("router_ms = %d, want 0 - nothing was generated and nothing failed",
			ev.RouterMS)
	}
}

func TestTheInFlightCountIsGivenBackWhenTheRequestIsOver(t *testing.T) {
	h := chainHarness(t,
		measuredRoute(policy.RouteModeLeastBusy, "keera-small", "keera-large"), nil)

	for range 3 {
		if resp := h.post(t, "/v1/chat/completions",
			`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`); resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		<-h.upstreamBodies
	}

	// A count never given back would rank the destination as busy forever,
	// and the router would stop moving traffic.
	now := time.Now()
	for _, alias := range []string{"keera-small", "keera-large"} {
		if got := h.srv.load.read(policy.ModelKey("org_1", alias), now).inflight; got != 0 {
			t.Errorf("%s has %d requests in flight after all of them finished",
				alias, got)
		}
	}
}

// The lettered question costs one token of generation per request, rather
// than as many as the model felt like writing.
func TestADecisionAsksForOneTokenAndTheDistributionBehindIt(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	asked := string(<-h.upstreamBodies)
	for _, want := range []string{`"max_tokens":1`, `"logprobs":true`, `"top_logprobs":20`} {
		if !strings.Contains(asked, want) {
			t.Errorf("the decision did not ask for %s: %s", want, asked)
		}
	}
	// The destinations are lettered, and the model is asked for the letter
	// rather than for a name it could spell wrong.
	for _, want := range []string{"A) keera-small", "B) keera-large", "Answer with one letter."} {
		if !strings.Contains(asked, want) {
			t.Errorf("the decision did not offer %q: %s", want, asked)
		}
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the chosen model: %s", forwarded)
	}
}

// A model that answers with a letter no destination has cannot place a
// request, because only the letters that were offered are looked at.
func TestALetterNoDestinationHasFallsBack(t *testing.T) {
	// The stand-in is asked for a destination it was never offered, so it
	// writes the name out instead of a letter.
	h := routeHarness(t, picks("keera-nowhere"), autoRoute(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.Alias != "keera-small" {
		t.Errorf("the request went to %q, want the router's fallback", ev.Alias)
	}
}

// A plane that serves no distribution still routes. It is found out once and
// asked for a name from then on, with nothing to configure.
func TestAPlaneWithoutLogprobsIsFoundOutOnceAndThenAskedForAName(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil, namesOnly)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The lettered question, the same question asked for a name, and then the
	// request itself.
	if asked := string(<-h.upstreamBodies); !strings.Contains(asked, `"logprobs":true`) {
		t.Errorf("the first decision did not try for a distribution: %s", asked)
	}
	retried := string(<-h.upstreamBodies)
	if strings.Contains(retried, `"logprobs":true`) {
		t.Errorf("it asked for a distribution twice: %s", retried)
	}
	if !strings.Contains(retried, "Answer with one of the names above") {
		t.Errorf("the retry did not ask for a name: %s", retried)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the chosen model: %s", forwarded)
	}

	// The second request pays nothing for finding that out again.
	resp = h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan another migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", resp.StatusCode)
	}
	if asked := string(<-h.upstreamBodies); strings.Contains(asked, `"logprobs":true`) {
		t.Errorf("it tried for a distribution again on the next request: %s", asked)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the second request did not go to the chosen model: %s", forwarded)
	}
	if n := len(h.upstreamBodies); n != 0 {
		t.Errorf("%d requests were sent that nothing accounts for", n)
	}
}

// A check reports how sure the model was, which says whether a sample landed
// right for a reason.
func TestACheckReportsHowSureEachDecisionWas(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil)

	probe := h.srv.CheckRoute(t.Context(), autoRoute())
	if !probe.OK {
		t.Fatalf("the check failed: %s", probe.Error)
	}
	if len(probe.Decisions) == 0 {
		t.Fatal("the check made no decisions")
	}
	for i, d := range probe.Decisions {
		if d.Error != "" {
			t.Fatalf("sample %d could not be decided: %s", i+1, d.Error)
		}
		if d.Confidence <= 0.9 || d.Confidence > 1 {
			t.Errorf("sample %d was %.2f sure, want the stand-in's near-certainty",
				i+1, d.Confidence)
		}
	}
}

// The same check against a plane that serves no distribution says nothing
// about confidence rather than claiming the router was unsure. Zero is
// unknown.
func TestACheckSaysNothingAboutConfidenceItCannotRead(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil, namesOnly)

	probe := h.srv.CheckRoute(t.Context(), autoRoute())
	if !probe.OK {
		t.Fatalf("the check failed: %s", probe.Error)
	}
	for i, d := range probe.Decisions {
		if d.Chosen != "keera-large" {
			t.Errorf("sample %d chose %q, want keera-large", i+1, d.Chosen)
		}
		if d.Confidence != 0 {
			t.Errorf("sample %d claimed %.2f confidence from a plane that reports none",
				i+1, d.Confidence)
		}
	}
}

// Any refusal after the router decided carries what the decision cost, not
// only the router's own refusals: the budget was charged either way.
func TestARefusalAfterTheRouterCarriesWhatItSpent(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil)
	large := h.src.models["keera-large"]
	large.MaxContext = 10
	h.src.models["keera-large"] = large

	resp := h.post(t, "/v1/chat/completions", `{"model":"auto","messages":[`+
		`{"role":"user","content":"`+strings.Repeat("a", 600)+`"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a prompt over the chosen model's window",
			resp.StatusCode)
	}
	ev := h.sink.last(t)
	if ev.CostMicros == 0 || ev.CostMicros != h.budgets.total() {
		t.Errorf("cost = %d, want the %d the decision charged to the budgets",
			ev.CostMicros, h.budgets.total())
	}
}
