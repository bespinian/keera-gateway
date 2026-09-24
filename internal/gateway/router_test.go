package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// routerStandIn is which kind of inference plane the stand-in answers as.
type routerStandIn int

const (
	// servesLogprobs is an inference plane of your own, which answers the
	// lettered question with the distribution behind its one token. It is what
	// vLLM and llama.cpp do, so it is the default here.
	servesLogprobs routerStandIn = iota
	// namesOnly is a plane that serves no distribution: it ignores the
	// logprobs fields and writes the name out. It is what a hosted provider
	// does, and what the router has to find out and work around.
	namesOnly
)

// routerHarness wires a gateway whose organisation has one router, in front of
// a stand-in inference plane answering as three models. They are told apart by
// the backend model name, which is what the gateway actually sends.
//
// decide is what the deciding model answers, given the segments it was shown.
func routerHarness(t *testing.T, decide func(segments []string) string,
	rt policy.Router, resolved *policy.Resolved, kind ...routerStandIn,
) *harness {
	t.Helper()

	plane := servesLogprobs
	if len(kind) > 0 {
		plane = kind[0]
	}

	backend := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Model    string `json:"model"`
			Logprobs bool   `json:"logprobs"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")

		if sent.Model != "picker-served" {
			_, _ = io.WriteString(w, `{"id":"1","choices":[{"message":{"content":"hi"}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			return
		}
		var segments []string
		if len(sent.Messages) > 1 {
			_ = json.Unmarshal([]byte(sent.Messages[1].Content), &segments)
		}
		// The same model can be both a router's and a filter's, and on the
		// wire the only thing that tells the two calls apart is the format the
		// gateway appended to the instruction. A gate is asked for a verdict,
		// so it is given one; everything else is the routing decision.
		answer := decide(segments)
		if len(sent.Messages) > 0 && strings.Contains(sent.Messages[0].Content, "ALLOW") {
			answer = "ALLOW"
		}
		var instruction string
		if len(sent.Messages) > 0 {
			instruction = sent.Messages[0].Content
		}
		if sent.Logprobs && plane == servesLogprobs {
			if body, ok := lettered(instruction, answer); ok {
				_, _ = io.WriteString(w, body)
				return
			}
		}
		reply, _ := json.Marshal(answer)
		_, _ = io.WriteString(w, `{"id":"2","choices":[{"message":{"content":`+string(reply)+
			`}}],"usage":{"prompt_tokens":50,"completion_tokens":2}}`)
	}

	if resolved == nil {
		resolved = policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	}
	models := map[string]policy.Model{
		"keera-small": {
			Alias: "keera-small", Kind: policy.KindChat, BackendModel: "small-served",
			Description:        "a fast local model for short edits",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 1_000_000, Enabled: true,
		},
		"keera-large": {
			Alias: "keera-large", Kind: policy.KindChat, BackendModel: "large-served",
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
	h.src.routers = map[string]policy.Router{"org_1/" + rt.Alias: rt}
	return h
}

// autoRouter is the ordinary shape: two destinations, a fallback, and an
// instruction the stand-in never reads.
func autoRouter() policy.Router {
	return policy.Router{
		OrgID: "org_1", Alias: "auto", Model: "keera-picker",
		Prompt:       "Send short edits to the small model and anything else to the large one.",
		Destinations: []string{"keera-small", "keera-large"},
		Fallback:     "keera-small",
	}
}

// picks is a deciding model that always names the same destination.
func picks(alias string) func([]string) string {
	return func([]string) string { return alias }
}

// lettered answers the lettered question the way an inference plane of your
// own does: the chosen destination's letter, and the distribution it was
// picked out of.
//
// The letters are read back out of the instruction the gateway sent, so the
// stand-in agrees with the gateway about which letter is which for the same
// reason a real model would - because it read the list.
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
	// The chosen letter takes almost all of the mass and the rest share what
	// is left, which is what a model that knows the answer looks like.
	tops := []string{`{"token":"` + chosen + `","logprob":-0.01}`}
	for _, letter := range alternatives {
		tops = append(tops, `{"token":"`+letter+`","logprob":-5.0}`)
	}
	return `{"id":"2","choices":[{"message":{"content":"` + chosen + `"},"logprobs":{"content":[` +
		`{"token":"` + chosen + `","logprob":-0.01,"top_logprobs":[` +
		strings.Join(tops, ",") + `]}]}}],` +
		`"usage":{"prompt_tokens":50,"completion_tokens":1}}`, true
}

func TestRouterSendsTheRequestToTheModelItChose(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil)

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

	// The usage row is the destination's, with the fact that it was chosen
	// beside it - which is what makes per-model reporting still about models.
	ev := h.sink.last(t)
	if ev.Alias != "keera-large" {
		t.Errorf("alias = %q, want the destination keera-large", ev.Alias)
	}
	if ev.Router != "auto" || ev.RouterOutcome != store.RouterChose {
		t.Errorf("router = %q/%q, want auto/chose", ev.Router, ev.RouterOutcome)
	}
}

// A router is on the hot path for every request that names it, so the answer
// the client sees has to name the model that answered rather than the router.
// Otherwise a developer reading a response cannot tell what produced it.
func TestRouterAnswerNamesTheModelThatAnswered(t *testing.T) {
	h := routerHarness(t, picks("keera-small"), autoRouter(), nil)

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
func TestRouterReadsTheRequestBeforeTheFiltersDo(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.Filters = []string{"redact"}

	var seen []string
	h := routerHarness(t, func(segments []string) string {
		seen = segments
		return "keera-small"
	}, autoRouter(), resolved)
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
// names something else has not made a decision this router was allowed to act
// on, however plausible the name is.
func TestRouterWillNotSendToAModelItWasNotOffered(t *testing.T) {
	h := routerHarness(t, picks("keera-embed"), autoRouter(), nil)

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

// A prompt arguing for a destination is data, not an instruction - and the
// clean-up that reads a name out of a sentence must not be the way in.
func TestRouterIgnoresTwoDestinationsNamedAtOnce(t *testing.T) {
	h := routerHarness(t, picks("either keera-small or keera-large would do"), autoRouter(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 by way of the fallback", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("outcome = %q, want fallback for an answer that chose nothing", ev.RouterOutcome)
	}
}

func TestRouterWithNoFallbackRefusesWhatItCannotDecide(t *testing.T) {
	rt := autoRouter()
	rt.Fallback = ""
	h := routerHarness(t, picks("no idea"), rt, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterError {
		t.Errorf("outcome = %q, want error", ev.RouterOutcome)
	}
	// The generation happened, so it is charged. A router a client could make
	// free by talking it into failing is one a client has a reason to break.
	if ev.CostMicros == 0 {
		t.Error("the decision that could not be made was not charged")
	}
}

// The other half of the same choice: a router that has somewhere safe to send a
// request it cannot place does so rather than taking a department offline.
func TestRouterFallsBackWhenItsModelIsGone(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil)
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
	if !strings.Contains(ev.Error, "catalogue no longer holds") {
		t.Errorf("error = %q, want it to say why the decision could not be made", ev.Error)
	}
	if ev.RouterMS < 0 {
		t.Errorf("router_ms = %d", ev.RouterMS)
	}
}

// A router is a name a key may or may not use, exactly as a model is, and the
// answer to a key that may not use it is the one a model gets: it does not
// exist, so another team's routers are not enumerable through 403s.
func TestRouterIsRefusedToAKeyWhoseAllowListOmitsIt(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.AllowedModels = []string{"keera-small"}
	h := routerHarness(t, picks("keera-large"), autoRouter(), resolved)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Allowing a router allows where it sends. A key narrowed to the router is the
// way a scope is made to route, so a destination outside its allow-list has to
// be reachable or the arrangement would work for nobody.
func TestRouterMayPlaceARequestOutsideTheKeysAllowList(t *testing.T) {
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	resolved.AllowedModels = []string{"auto"}
	h := routerHarness(t, picks("keera-large"), autoRouter(), resolved)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.Alias != "keera-large" {
		t.Errorf("alias = %q, want keera-large", ev.Alias)
	}
}

// A router decides with a chat model and its destinations answer chat requests,
// so it is not a name the other surfaces have - which is the answer a chat model
// named on /v1/embeddings already gets.
func TestRouterIsNotAModelOnTheEmbeddingSurface(t *testing.T) {
	h := routerHarness(t, picks("keera-small"), autoRouter(), nil)

	resp := h.post(t, "/v1/embeddings", `{"model":"auto","input":"hello"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// The catalogue is shared by every tenant, so an alias must not be able to mean
// something else in one organisation.
func TestAModelAliasWinsOverARouterOfTheSameName(t *testing.T) {
	rt := autoRouter()
	rt.Alias = "keera-small"
	h := routerHarness(t, picks("keera-large"), rt, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-small","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ev := h.sink.last(t); ev.Router != "" {
		t.Errorf("router = %q, want the alias to have been served as a model", ev.Router)
	}
}

// What the deciding model is told about each destination is the catalogue's own
// description, which is the reason that field exists.
func TestTheDecidingModelIsToldWhatEachDestinationIsFor(t *testing.T) {
	h := routerHarness(t, picks("keera-small"), autoRouter(), nil)

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
// than offered and then found unreachable - which would be a fallback waiting
// to happen on every request that picked it.
func TestADisabledDestinationIsNotOffered(t *testing.T) {
	h := routerHarness(t, picks("keera-small"), autoRouter(), nil)
	large := h.src.models["keera-large"]
	large.Enabled = false
	h.src.models["keera-large"] = large

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if instruction := string(<-h.upstreamBodies); strings.Contains(instruction, "keera-large") {
		t.Errorf("a disabled destination was offered: %s", instruction)
	}
}

func TestRoutersAreListedWhereAClientLooksForModels(t *testing.T) {
	h := routerHarness(t, picks("keera-small"), autoRouter(), nil)

	req, _ := http.NewRequest(http.MethodGet, h.url("/v1/models"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

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
	var found bool
	for _, m := range out.Data {
		switch m.ID {
		case "auto":
			found = true
			if len(m.Destinations) != 2 {
				t.Errorf("the router was listed without its destinations: %+v", m)
			}
		case "keera-small":
			// The description a router decides on is the same one a client is
			// choosing between aliases by.
			if m.Description != "a fast local model for short edits" {
				t.Errorf("description = %q", m.Description)
			}
		}
	}
	if !found {
		t.Error("the router was not in /v1/models, so no client could discover it")
	}
}

/* ---------------------------------------------------------------- unit tests */

func TestChosenDestination(t *testing.T) {
	destinations := []string{"keera-small", "keera-large", "keera-small-eu"}
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

// The window is filled from the end, because the question a router answers is
// what is being asked now - and a decision made on the head of a coding agent's
// conversation would be a decision about an hour ago, on every request for the
// rest of the session.
func TestRouterWindowKeepsTheNewestSegments(t *testing.T) {
	long := strings.Repeat("x", routerWindowBytes)
	got := routerWindow([]string{long, "the older question", "the newest question"})
	if len(got) != 2 || got[len(got)-1] != "the newest question" {
		t.Fatalf("routerWindow kept %d segments ending %q", len(got), got[len(got)-1])
	}

	// One segment larger than the whole window is kept whole: half a sentence
	// with no way to know it is half would be worse than a long one.
	if got := routerWindow([]string{long + long}); len(got) != 1 {
		t.Errorf("routerWindow dropped an oversized lone segment: %d segments", len(got))
	}
	if got := routerWindow(nil); got != nil {
		t.Errorf("routerWindow(nil) = %v, want nil", got)
	}
}

/* ------------------------------------------------- the router that does not
   decide */

// chainHarness wires a gateway whose organisation has one fallback router in
// front of a stand-in inference plane where some models are down.
//
// A fallback router's whole behaviour is a function of what its destinations
// answer, so that - rather than what a deciding model says - is what these
// tests vary. down names the backend models that answer with a failure of
// their own; everything else answers normally.
func chainHarness(t *testing.T, rt policy.Router, down map[string]int) *harness {
	t.Helper()

	backend := func(w http.ResponseWriter, r *http.Request) {
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
	}

	models := map[string]policy.Model{
		"keera-small": {
			Alias: "keera-small", Kind: policy.KindChat, BackendModel: "small-served",
			InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 1_000_000, Enabled: true,
		},
		"keera-large": {
			Alias: "keera-large", Kind: policy.KindChat, BackendModel: "large-served",
			InputMicrosPerMTok: 9_000_000, OutputMicrosPerMTok: 9_000_000, Enabled: true,
		},
		"keera-spare": {
			Alias: "keera-spare", Kind: policy.KindChat, BackendModel: "spare-served",
			InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 5_000_000, Enabled: true,
		},
	}
	h := newHarness(t, backend, models,
		policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil))
	h.src.routers = map[string]policy.Router{"org_1/" + rt.Alias: rt}
	return h
}

// chainRouter is the ordinary shape of the other kind: an order, and nothing
// that reads anything.
func chainRouter(destinations ...string) policy.Router {
	if len(destinations) == 0 {
		destinations = []string{"keera-small", "keera-large"}
	}
	return policy.Router{
		OrgID: "org_1", Alias: "ha", Mode: policy.RouterModeFallback,
		Destinations: destinations,
	}
}

func TestFallbackRouterUsesItsFirstDestination(t *testing.T) {
	h := chainHarness(t, chainRouter(), nil)

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
	// Nothing read the request, so nothing generated anything to read it with:
	// what was charged is the answering model's own ten input tokens and five
	// output tokens at a micro each, and not a micro more.
	if got := h.budgets.total(); got != 15 {
		t.Errorf("%d micros were charged, want 15 - a fallback router decides nothing, "+
			"so there is no generation of its own to pay for", got)
	}
}

func TestFallbackRouterMovesToTheNextWhenOneFails(t *testing.T) {
	h := chainHarness(t, chainRouter(), map[string]int{"small-served": 503})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the second destination answered", resp.StatusCode)
	}
	if first := string(<-h.upstreamBodies); !strings.Contains(first, "small-served") {
		t.Errorf("the first destination was not tried first: %s", first)
	}
	if second := string(<-h.upstreamBodies); !strings.Contains(second, "large-served") {
		t.Errorf("the second destination was not tried next: %s", second)
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "ha -> keera-large (fallback)" {
		t.Errorf("X-Keera-Router = %q; a client has to be able to see that it is being "+
			"answered by the second choice", got)
	}

	ev := h.sink.last(t)
	if ev.Alias != "keera-large" {
		t.Errorf("alias = %q; the row's tokens and price are the answering model's",
			ev.Alias)
	}
	if ev.RouterOutcome != store.RouterFellBack {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterFellBack)
	}
	// The request succeeded, so this row is the only thing that says the first
	// destination is down.
	if !strings.Contains(ev.Error, "keera-small answered 503") {
		t.Errorf("the failed attempt was not recorded: %q", ev.Error)
	}
}

func TestFallbackRouterTriesEveryDestinationInOrder(t *testing.T) {
	h := chainHarness(t, chainRouter("keera-small", "keera-spare", "keera-large"),
		map[string]int{"small-served": 500, "spare-served": 502})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"small-served", "spare-served", "large-served"} {
		if got := string(<-h.upstreamBodies); !strings.Contains(got, want) {
			t.Fatalf("destination out of order: wanted %s, got %s", want, got)
		}
	}
}

func TestFallbackRouterServesTheLastFailureWhenNothingAnswers(t *testing.T) {
	h := chainHarness(t, chainRouter(),
		map[string]int{"small-served": 503, "large-served": 500})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	// The last destination's own answer, rather than something this process
	// invented over it: it is the truest thing available about why there is no
	// completion.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 - what the last destination said", resp.StatusCode)
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

func TestFallbackRouterDoesNotRepeatARefusedRequest(t *testing.T) {
	h := chainHarness(t, chainRouter(), map[string]int{"small-served": 400})

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

func TestFallbackRouterRefusesWhenNothingCanServe(t *testing.T) {
	h := chainHarness(t, chainRouter("keera-gone", "keera-also-gone"), nil)

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

func TestFallbackRouterSkipsADestinationTheCatalogueCannotServe(t *testing.T) {
	h := chainHarness(t, chainRouter("keera-gone", "keera-large"), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if forwarded := string(<-h.upstreamBodies); !strings.Contains(forwarded, "large-served") {
		t.Errorf("the request did not go to the destination that exists: %s", forwarded)
	}
	// A destination that is not in the catalogue is not an attempt that was
	// made and failed - it is a name this router never had, and the request
	// went where the router's first real destination was.
	if ev := h.sink.last(t); ev.RouterOutcome != store.RouterChose {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterChose)
	}
}

// A router that decided does not get a second go. The client named a router,
// the router named a model, and that model answering 500 is that model's
// failure - asking a different one would answer the developer out of a model
// nothing chose.
func TestInstructionRouterDoesNotFailOverFromWhatItChose(t *testing.T) {
	rt := autoRouter()
	h := routerHarness(t, picks("keera-large"), rt, nil)
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

/* --------------------------------------------- the routers that measure */

// measuredRouter is a router that reads nothing and ranks what it has by what
// this process has seen of it.
func measuredRouter(mode policy.RouterMode, destinations ...string) policy.Router {
	return policy.Router{
		OrgID: "org_1", Alias: "pool", Mode: mode, Destinations: destinations,
	}
}

func TestLatencyRouterPlacesTheRequestOnTheFasterDestination(t *testing.T) {
	h := chainHarness(t,
		measuredRouter(policy.RouterModeLatency, "keera-small", "keera-large"), nil)

	// What the last few minutes looked like: the first destination, which a
	// fallback router would use for everything, is the slow one.
	now := time.Now()
	h.srv.load.observe("keera-small", 2*time.Second, now)
	h.srv.load.observe("keera-large", 90*time.Millisecond, now)

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
	// It went where the router meant it to go, so this is the router getting
	// what it wanted - which on these modes is the top-ranked destination
	// rather than the first one written.
	ev := h.sink.last(t)
	if ev.RouterOutcome != store.RouterChose {
		t.Errorf("outcome = %q, want %q", ev.RouterOutcome, store.RouterChose)
	}
	// keera-large's own ten input and five output tokens at nine micros each,
	// and not a micro more: nothing read the request, so there is no
	// generation of the router's own to pay for.
	if got := h.budgets.total(); got != 135 {
		t.Errorf("%d micros were charged, want 135 - a measured router decides nothing, "+
			"so there is no generation of its own to pay for", got)
	}
}

func TestLeastBusyRouterPlacesTheRequestWhereThereIsRoom(t *testing.T) {
	h := chainHarness(t,
		measuredRouter(policy.RouterModeLeastBusy, "keera-small", "keera-large"), nil)

	// Two requests already outstanding against the destination a fallback
	// router would send everything to.
	defer h.srv.load.begin("keera-small")()
	defer h.srv.load.begin("keera-small")()

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

func TestAMeasuredRouterStillFailsOverToWhatItRankedSecond(t *testing.T) {
	// The ranking is a guess about which destination should take the request,
	// and it is wrong here: the fast one is down. What must not happen is the
	// request failing because the router was confident.
	h := chainHarness(t,
		measuredRouter(policy.RouterModeLatency, "keera-small", "keera-large"),
		map[string]int{"large-served": 503})
	now := time.Now()
	h.srv.load.observe("keera-small", 2*time.Second, now)
	h.srv.load.observe("keera-large", 90*time.Millisecond, now)

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
	// And the destination that failed is ranked last for a while, so the next
	// request does not walk into it again on the strength of the same reading.
	if !h.srv.load.read("keera-large", time.Now()).failing {
		t.Error("a destination that answered 503 is still being ranked on its latency; " +
			"the next request would be sent to it too")
	}
}

func TestAMeasuredRouterChargesNoRoutingTime(t *testing.T) {
	h := chainHarness(t,
		measuredRouter(policy.RouterModeLeastBusy, "keera-small", "keera-large"), nil)

	if resp := h.post(t, "/v1/chat/completions",
		`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	<-h.upstreamBodies

	// Ordering a list of two is not a wait the client spends. router_ms is
	// what the router added over and above the answering model's own work, and
	// on a router that neither generates nor retries there is nothing to add.
	if ev := h.sink.last(t); ev.RouterMS != 0 {
		t.Errorf("router_ms = %d, want 0 - nothing was generated and nothing failed",
			ev.RouterMS)
	}
}

func TestTheInFlightCountIsGivenBackWhenTheRequestIsOver(t *testing.T) {
	h := chainHarness(t,
		measuredRouter(policy.RouterModeLeastBusy, "keera-small", "keera-large"), nil)

	for range 3 {
		if resp := h.post(t, "/v1/chat/completions",
			`{"model":"pool","messages":[{"role":"user","content":"hello"}]}`); resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		<-h.upstreamBodies
	}

	// A count that is never given back is a destination this process believes
	// is busy forever and will never rank first again - which is a router that
	// works for an hour and then stops moving traffic.
	now := time.Now()
	for _, alias := range []string{"keera-small", "keera-large"} {
		if got := h.srv.load.read(alias, now).inflight; got != 0 {
			t.Errorf("%s has %d requests in flight after all of them finished",
				alias, got)
		}
	}
}

/* ------------------------------------- deciding by reading the distribution */

// The saving the lettered question exists for: one token of generation on
// every request that names the router, rather than as many as the model felt
// like writing.
func TestADecisionAsksForOneTokenAndTheDistributionBehindIt(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil)

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
// request, because only the letters that were offered are looked at. This is
// the failure that used to be a decision naming nothing.
func TestALetterNoDestinationHasFallsBack(t *testing.T) {
	// The stand-in is asked for a destination it was never offered, so it
	// letters nothing and writes the name out instead - which, on the lettered
	// question, is not a letter.
	h := routerHarness(t, picks("keera-nowhere"), autoRouter(), nil)

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
// asked for a name from then on, which is the whole of the configuration this
// needs.
func TestAPlaneWithoutLogprobsIsFoundOutOnceAndThenAskedForAName(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil, namesOnly)

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

// A check is what somebody reads before putting a router in front of their
// developers, and how sure the model was is the part of it that says whether a
// sample landed right for a reason.
func TestACheckReportsHowSureEachDecisionWas(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil)

	probe := h.srv.CheckRouter(t.Context(), autoRouter())
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
// unknown, and a check that read it as hopeless would send somebody looking
// for a problem their deployment does not have.
func TestACheckSaysNothingAboutConfidenceItCannotRead(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil, namesOnly)

	probe := h.srv.CheckRouter(t.Context(), autoRouter())
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
