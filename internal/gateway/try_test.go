package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// refusesHunter2 is a gate that stops a prompt carrying the test credential.
func refusesHunter2(segments []string) string {
	for _, s := range segments {
		if strings.Contains(s, "hunter2") {
			return "REFUSED: it carries a credential"
		}
	}
	return "ALLOW"
}

func TestTryingAGateShowsWhatItsModelWasSentAndAnswered(t *testing.T) {
	h := gateHarness(t, refusesHunter2)
	f := h.src.filters["org_1/redact"]

	got := h.server().TryFilter(t.Context(), f, []string{"deploy with hunter2", "add two numbers"})

	if got.Error != "" || len(got.Trials) != 2 {
		t.Fatalf("try = %+v, want one trial per prompt", got)
	}
	refused, allowed := got.Trials[0], got.Trials[1]
	if refused.Outcome != store.FilterRefuse || refused.Reason != "it carries a credential" {
		t.Errorf("first trial = %+v, want refused with the gate's sentence", refused)
	}
	if allowed.Outcome != store.FilterPass {
		t.Errorf("second trial = %+v, want it allowed", allowed)
	}
	if refused.Read != ReadText {
		t.Errorf("read = %q, want the written word, which decides when it is there", refused.Read)
	}

	// The whole prompt: the instruction, the wire format after it, and the
	// prompt as the segments the model reads. It showed no reasoning, so it
	// was asked why after.
	if len(refused.Calls) != 2 || refused.Calls[1].Purpose != PurposeExplain {
		t.Fatalf("calls = %+v, want the decision, then why", refused.Calls)
	}
	call := refused.Calls[0]
	if call.Purpose != PurposeDecide || !call.Sent || call.Model != "keera-guard" {
		t.Errorf("call = %+v, want the decision, sent to the filter's model", call)
	}
	if len(call.Messages) != 2 {
		t.Fatalf("messages = %+v, want the system and the user message", call.Messages)
	}
	if system := call.Messages[0].Content; !strings.HasPrefix(system, f.Prompt) ||
		!strings.Contains(system, "--- how to answer ---") {
		t.Errorf("system message = %q, want the instruction and the wire format", system)
	}
	if user := call.Messages[1].Content; user != `["deploy with hunter2"]` {
		t.Errorf("user message = %q, want the prompt as one segment", user)
	}
	// The messages are the request; it is not sent twice.
	if call.Request != nil {
		t.Errorf("request = %s, want none beside the messages", call.Request)
	}
	if call.Answer != "REFUSED: it carries a credential" {
		t.Errorf("answer = %q, want what the model wrote", call.Answer)
	}
}

func TestTryingARewriteFilterShowsThePromptAsItWouldBeForwarded(t *testing.T) {
	h := filterHarness(t, redactor, nil)

	got := h.server().TryFilter(t.Context(), h.src.filters["org_1/redact"],
		[]string{"the password is hunter2", "nothing secret here"})

	if got.Trials[0].Outcome != store.FilterRewrite ||
		got.Trials[0].After != "the password is [CREDENTIAL]" {
		t.Errorf("first trial = %+v, want the redacted text", got.Trials[0])
	}
	if got.Trials[1].Outcome != store.FilterPass || got.Trials[1].After != "" {
		t.Errorf("second trial = %+v, want it passed unchanged", got.Trials[1])
	}
	if got.CostMicros <= 0 {
		t.Errorf("cost = %d, want what the two runs cost", got.CostMicros)
	}
}

func TestTryingAPatternFilterNamesTheRulesThatFired(t *testing.T) {
	h := filterHarness(t, redactor, nil)
	f := patternFilter(policy.FilterRule{Pattern: `hunter2`, Replace: "[CREDENTIAL]"})

	got := h.server().TryFilter(t.Context(), f, []string{"the password is hunter2"})

	if !got.Free {
		t.Error("a pattern filter's try was not marked free")
	}
	tr := got.Trials[0]
	if tr.Outcome != store.FilterRewrite || len(tr.Hits) != 1 || tr.Hits[0].Matches != 1 {
		t.Errorf("trial = %+v, want the one rule's one match", tr)
	}
	// No model ran, so there is nothing to show or ask.
	if len(tr.Calls) != 0 || tr.Explanation != "" {
		t.Errorf("trial = %+v, want no model calls", tr)
	}
}

func TestTryingARouteStepShowsEveryDestinationsShare(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil)

	got := h.srv.TryRoute(t.Context(), autoRoute(), []string{"plan a migration"})

	if got.Error != "" || len(got.Trials) != 1 {
		t.Fatalf("try = %+v, want one trial", got)
	}
	tr := got.Trials[0]
	if tr.Chosen != "keera-large" || tr.Read != ReadProbabilities {
		t.Errorf("trial = %+v, want keera-large, read from the probabilities", tr)
	}
	if len(tr.Shares) != len(got.Destinations) {
		t.Fatalf("shares = %v, want one per destination %v", tr.Shares, got.Destinations)
	}
	if tr.Shares[1] < 0.9 || tr.Shares[0]+tr.Shares[1] < 0.999 {
		t.Errorf("shares = %v, want nearly all of it on keera-large", tr.Shares)
	}
	system := tr.Calls[0].Messages[0].Content
	if !strings.Contains(system, "A) keera-small (a fast local model") {
		t.Errorf("system message = %q, want the lettered destinations", system)
	}
}

func TestTryingARouteStepShowsTheQuestionItAskedAgain(t *testing.T) {
	h := routeHarness(t, picks("keera-large"), autoRoute(), nil, namesOnly)

	tr := h.srv.TryRoute(t.Context(), autoRoute(), []string{"plan a migration"}).Trials[0]

	if tr.Chosen != "keera-large" || tr.Read != ReadText {
		t.Errorf("trial = %+v, want keera-large, read from the name it wrote", tr)
	}
	if len(tr.Calls) != 3 || tr.Calls[0].Purpose != PurposeDecide ||
		tr.Calls[1].Purpose != PurposeRetry || tr.Calls[2].Purpose != PurposeExplain {
		t.Fatalf("calls = %+v, want the lettered question, the named one, then why", tr.Calls)
	}
	// Why is asked about the answer that was read, the name.
	if why := tr.Calls[2].Messages; why[0].Content != tr.Calls[1].Messages[0].Content {
		t.Errorf("why followed %q, want the named question", why[0].Content)
	}
	if !strings.Contains(tr.Calls[1].Messages[0].Content, "Answer at once with the model's name only") {
		t.Errorf("the second call did not ask for a name: %q", tr.Calls[1].Messages[0].Content)
	}
}

func TestAnUndecidedTrySaysWhereTheRequestWouldGo(t *testing.T) {
	h := routeHarness(t, picks("keera-nowhere"), autoRoute(), nil, namesOnly)

	tr := h.srv.TryRoute(t.Context(), autoRoute(), []string{"plan a migration"}).Trials[0]

	if tr.Error == "" || !strings.Contains(tr.Becomes, "keera-small") {
		t.Errorf("trial = %+v, want the failure and the fallback it leads to", tr)
	}
	if last := tr.Calls[len(tr.Calls)-1]; last.Answer != "keera-nowhere" {
		t.Errorf("answer = %q, want what the model wrote, to see why it failed", last.Answer)
	}
}

func TestTryingAStepThatReadsNothingSaysSo(t *testing.T) {
	rt := autoRoute()
	rt.Mode = policy.RouteModeFallback
	h := routeHarness(t, picks("keera-large"), rt, nil)

	got := h.srv.TryRoute(t.Context(), rt, []string{"plan a migration"})

	if got.Error == "" || len(got.Trials) != 0 {
		t.Errorf("try = %+v, want an error and no trials", got)
	}
}

// explainingGate refuses, and when asked why, says so. The explanation is
// asked in the same conversation, so it has four messages. With thinks, the
// refusal comes with the model's reasoning.
func explainingGate(t *testing.T, thinks bool) (*harness, *[][]CallMessage) {
	t.Helper()
	var asked [][]CallMessage
	backend := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent struct {
			Messages []CallMessage `json:"messages"`
		}
		_ = json.Unmarshal(raw, &sent)
		asked = append(asked, sent.Messages)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case len(sent.Messages) == 4:
			_, _ = io.WriteString(w, chatAnswer("The prompt carries a password.", 120, 8))
		case thinks:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"REFUSED: a credential",`+
				`"reasoning_content":"The user pasted hunter2, which is a password."}}],`+
				`"usage":{"prompt_tokens":100,"completion_tokens":4}}`)
		default:
			_, _ = io.WriteString(w, chatAnswer("REFUSED: a credential", 100, 4))
		}
	}
	resolved := policy.Resolve(policy.Key{ID: "key_1", OrgID: "org_1"}, nil, nil, nil)
	h := newHarness(t, backend, filterModels(), resolved)
	h.src.filters = map[string]policy.Filter{
		"org_1/redact": {
			OrgID: "org_1", Alias: "redact", Mode: policy.FilterModeGate, Model: "keera-guard",
			Prompt: "Refuse requests that carry a credential.",
		},
	}
	return h, &asked
}

func TestATryAsksAModelThatShowsNoReasoningWhyInTheSameConversation(t *testing.T) {
	h, asked := explainingGate(t, false)

	tr := h.server().TryFilter(t.Context(), h.src.filters["org_1/redact"],
		[]string{"deploy with hunter2"}).Trials[0]

	if tr.Explanation != "The prompt carries a password." || tr.ExplainError != "" {
		t.Errorf("explanation = %q (%q), want the model's answer", tr.Explanation, tr.ExplainError)
	}
	if len(*asked) != 2 {
		t.Fatalf("the model was asked %d times, want the decision and why", len(*asked))
	}
	why := (*asked)[1]
	if why[2].Role != "assistant" || why[2].Content != "REFUSED: a credential" ||
		why[0].Content != (*asked)[0][0].Content || why[3].Content != explainQuestion {
		t.Errorf("explanation request = %+v, want the decision's conversation and its answer", why)
	}
	if len(tr.Calls) != 2 || tr.Calls[1].Purpose != PurposeExplain {
		t.Errorf("calls = %+v, want the decision and the explanation", tr.Calls)
	}
	// The explanation is charged with the trial.
	thinker, _ := explainingGate(t, true)
	plain := thinker.server().TryFilter(t.Context(), thinker.src.filters["org_1/redact"],
		[]string{"deploy with hunter2"}).Trials[0]
	if tr.CostMicros <= plain.CostMicros {
		t.Errorf("cost = %d, want more than the decision's %d alone", tr.CostMicros, plain.CostMicros)
	}
}

func TestATryDoesNotAskAModelThatShowedItsReasoning(t *testing.T) {
	h, asked := explainingGate(t, true)

	tr := h.server().TryFilter(t.Context(), h.src.filters["org_1/redact"],
		[]string{"deploy with hunter2"}).Trials[0]

	if len(*asked) != 1 || len(tr.Calls) != 1 {
		t.Fatalf("the model was asked %d times, want only the decision", len(*asked))
	}
	if tr.Explanation != "" || tr.ExplainError != "" {
		t.Errorf("explanation = %q (%q), want none", tr.Explanation, tr.ExplainError)
	}
	if tr.Calls[0].Reasoning != "The user pasted hunter2, which is a password." {
		t.Errorf("reasoning = %q, want what the backend returned", tr.Calls[0].Reasoning)
	}
}

func TestATryShowsTheRequestItWouldHaveSentWhenItIsTooLarge(t *testing.T) {
	h := gateHarness(t, refusesHunter2)
	guard := h.src.models["keera-guard"]
	guard.MaxContext = 10
	h.src.models["keera-guard"] = guard

	tr := h.server().TryFilter(t.Context(), h.src.filters["org_1/redact"],
		[]string{strings.Repeat("a long prompt ", 50)}).Trials[0]

	if tr.Outcome != store.FilterError {
		t.Fatalf("trial = %+v, want an error", tr)
	}
	if len(tr.Calls) != 1 || tr.Calls[0].Sent || len(tr.Calls[0].Messages) != 2 ||
		tr.Calls[0].Error == "" {
		t.Errorf("calls = %+v, want the unsent request and why", tr.Calls)
	}
	// Nothing was answered, so there is nothing to ask about.
	if tr.Explanation != "" || tr.ExplainError != "" {
		t.Errorf("explanation = %q (%q), want none for a request never sent",
			tr.Explanation, tr.ExplainError)
	}
}

// A spare format argument is printed into the prompt as %!(EXTRA ...), right
// where the model reads how to answer.
func TestTheWireFormatHasNoFormattingLeftInIt(t *testing.T) {
	for name, h := range map[string]*harness{
		"rewrite": filterHarness(t, redactor, nil),
		"gate":    gateHarness(t, refusesHunter2),
	} {
		t.Run(name, func(t *testing.T) {
			tr := h.server().TryFilter(t.Context(), h.src.filters["org_1/redact"],
				[]string{"deploy with hunter2"}).Trials[0]
			system := tr.Calls[0].Messages[0].Content
			if strings.Contains(system, "%!") || strings.Contains(system, "%[") {
				t.Errorf("the system message has formatting left in it:\n%s", system)
			}
			if !strings.Contains(system, "array of 1 text segments") {
				t.Errorf("the system message does not give the segment count:\n%s", system)
			}
		})
	}
}

// A decision model's request has no messages, so its body is all there is to
// show. A chat request's would only repeat its messages.
func TestATryShowsTheBodyOfARequestWithoutMessages(t *testing.T) {
	var log callLog
	body := []byte(`{"state":["hi"],"questions":{}}`)
	log.note(policy.Model{Alias: "keera-decide", Kind: policy.KindDecision}, guardQuestion{},
		callRecord{path: decisionPath, payload: body, sent: true}, nil, nil)
	if got := string(log.calls[0].Request); got != string(body) {
		t.Errorf("request = %s, want the body as sent", got)
	}
}
