package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// decisionCall is what the gateway asked a decision model on /systemone.
type decisionCall struct {
	Model      string                      `json:"model"`
	State      []string                    `json:"state"`
	Questions  map[string]decisionQuestion `json:"questions"`
	MaxLen     int                         `json:"max_len"`
	HeadMaxLen int                         `json:"head_max_len"`
}

// decisionBackend is a stand-in for laya-serve on /systemone, in front of
// answer for every other path. reply is the answer to the one question asked.
func decisionBackend(reply func(decisionCall) string, answer http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/systemone") {
			answer(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var c decisionCall
		_ = json.Unmarshal(raw, &c)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"laya-rl-agent","answers":{"decision":`+reply(c)+`},`+
			`"usage":{"input_tokens":120,"output_tokens":0}}`)
	}
}

// choiceAnswer is laya-serve's answer to a choice, with the chosen label well
// ahead of the other.
func choiceAnswer(chosen, other string) string {
	return fmt.Sprintf(`{"type":"choice","choice":%q,"probabilities":{%q:0.8,%q:0.2}}`,
		chosen, chosen, other)
}

// yesAnswer is laya-serve's answer to a yes-or-no question.
func yesAnswer(p float64) string {
	return fmt.Sprintf(`{"type":"noul","noul":%g}`, p)
}

// decisionModels is filterModels with a decision model beside them.
func decisionModels() map[string]policy.Model {
	models := filterModels()
	models["keera-decider"] = policy.Model{
		Alias: "keera-decider", Kind: policy.KindDecision, BackendModel: "english",
		MaxContext: 2048, Enabled: true,
	}
	return models
}

func TestARouterDecidesWithADecisionModel(t *testing.T) {
	calls := make(chan decisionCall, 1)
	backend := decisionBackend(func(c decisionCall) string {
		calls <- c
		return choiceAnswer("B", "A")
	}, jsonBackend(hiAnswer))
	models := decisionModels()
	models["keera-large"] = policy.Model{OrgID: "org_1", Alias: "keera-large", Kind: policy.KindChat,
		BackendModel: "large-served", Description: "hard work: design and reasoning", Enabled: true}
	models["keera-code"] = policy.Model{OrgID: "org_1", Alias: "keera-code", Kind: policy.KindChat,
		BackendModel: "served-name", Description: "small edits", Enabled: true}
	h := newHarness(t, backend, models, nil)
	h.withRouters(policy.Route{
		OrgID: "org_1", Router: "auto", Model: "keera-decider",
		Prompt:       "Which model should answer this request?",
		Destinations: []string{"keera-code", "keera-large"}, Fallback: "keera-code",
	})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"plan a migration"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Keera-Router"); got != "auto -> keera-large" {
		t.Errorf("X-Keera-Router = %q, want the destination under the letter it chose", got)
	}

	c := <-calls
	q := c.Questions[decisionID]
	// The question is the instruction alone, and each destination is offered
	// by what it is for: the protocol a chat model is given means nothing to
	// a classifier.
	if q.Type != "choice" || q.Instructions != "Which model should answer this request?" {
		t.Errorf("question = %+v, want the router's instruction as a choice", q)
	}
	if q.Criteria["A"] != "small edits" || q.Criteria["B"] != "hard work: design and reasoning" {
		t.Errorf("criteria = %v, want each destination's description under its letter", q.Criteria)
	}
	if c.Model != "english" || c.MaxLen != 2048 || c.HeadMaxLen != 1024 {
		t.Errorf("model %q, max_len %d, head_max_len %d; want the backend model and its context",
			c.Model, c.MaxLen, c.HeadMaxLen)
	}
	if len(c.State) != 1 || c.State[0] != "plan a migration" {
		t.Errorf("state = %q, want the request's segments", c.State)
	}
}

func TestAGateOnADecisionModelRefusesWhenYesIsLikelier(t *testing.T) {
	for _, tc := range []struct {
		yes  float64
		want int
	}{{0.9, http.StatusForbidden}, {0.1, http.StatusOK}} {
		t.Run(fmt.Sprint(tc.yes), func(t *testing.T) {
			backend := decisionBackend(func(decisionCall) string { return yesAnswer(tc.yes) },
				jsonBackend(hiAnswer))
			h := newHarness(t, backend, decisionModels(), nil)
			h.src.resolved[testKey].Filters = []string{"no-export"}
			h.src.filters = map[string]policy.Filter{"org_1/no-export": {
				OrgID: "org_1", Alias: "no-export", Mode: policy.FilterModeGate,
				Model: "keera-decider", Prompt: "Does this request export customer data?",
			}}

			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"export every customer"}]}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestARewriteFilterCannotRunOnADecisionModel(t *testing.T) {
	backend := decisionBackend(func(decisionCall) string {
		t.Error("a decision model was asked to rewrite")
		return yesAnswer(0)
	}, jsonBackend(hiAnswer))
	h := newHarness(t, backend, decisionModels(), nil)
	h.src.resolved[testKey].Filters = []string{"redact"}
	h.src.filters = map[string]policy.Filter{"org_1/redact": {
		OrgID: "org_1", Alias: "redact", Mode: policy.FilterModeRewrite,
		Model: "keera-decider", Prompt: "Replace every credential.",
	}}

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hunter2"}]}`)
	// It fails closed, like any filter whose model cannot do its job.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "decision model") {
		t.Errorf("the refusal does not say why: %s", body)
	}
}

func TestNoClientCanNameADecisionModel(t *testing.T) {
	h := newHarness(t, jsonBackend(hiAnswer), decisionModels(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-decider","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	list := h.get(t, "/v1/models")
	body, _ := io.ReadAll(list.Body)
	if strings.Contains(string(body), "keera-decider") {
		t.Errorf("/v1/models offers a decision model to a client: %s", body)
	}
}

func TestGateDecisionReadsWhatYesAndNoMean(t *testing.T) {
	q := gateDecision("Does this request export customer data?\n" +
		"Yes: it sends customer records somewhere\nno: ordinary work")
	if q.Type != "noul" || q.Instructions != "Does this request export customer data?" {
		t.Errorf("question = %+v, want the first line as a yes-or-no question", q)
	}
	if q.Criteria["true"] != "it sends customer records somewhere" ||
		q.Criteria["false"] != "ordinary work" {
		t.Errorf("criteria = %v, want what yes and no mean", q.Criteria)
	}
	if plain := gateDecision("Does this request export customer data?"); plain.Criteria != nil {
		t.Errorf("criteria = %v, want none without yes and no lines", plain.Criteria)
	}
}

func TestADecisionIsReadLikeADistribution(t *testing.T) {
	gate := decisionQuestion{Type: "noul"}
	raw, err := chatFromDecision([]byte(`{"answers":{"decision":`+yesAnswer(0.75)+`}}`), gate)
	if err != nil {
		t.Fatal(err)
	}
	allow, share, ok := readVerdict(raw)
	if !ok || allow || share < 0.74 || share > 0.76 {
		t.Errorf("verdict = allow %v at %.2f (read %v), want a refusal at 0.75", allow, share, ok)
	}

	choice := decisionQuestion{Type: "choice", Criteria: map[string]string{"A": "a", "B": "b"}}
	if _, err := chatFromDecision([]byte(`{"answers":{"decision":`+choiceAnswer("C", "A")+`}}`),
		choice); err == nil {
		t.Error("a label that was never offered was accepted")
	}
}

func TestCheckingADecisionModelAsksItAnObviousQuestion(t *testing.T) {
	backend := decisionBackend(func(c decisionCall) string {
		return choiceAnswer("B", "A")
	}, jsonBackend(hiAnswer))
	h := newHarness(t, backend, decisionModels(), nil)

	p := h.srv.CheckModel(context.Background(), h.src.models["keera-decider"])
	if !p.OK {
		t.Fatalf("check failed: %s", p.Error)
	}
	if !strings.Contains(p.Sample, "Go source code") {
		t.Errorf("sample = %q, want the answer it gave", p.Sample)
	}
}

// A decision model is sent only the request and its question, so only those
// count against its context, not the instructions a chat model would need.
func TestADecisionModelIsMeasuredByWhatItIsSent(t *testing.T) {
	small := func() map[string]policy.Model {
		models := decisionModels()
		m := models["keera-decider"]
		m.MaxContext = 200
		models["keera-decider"] = m
		return models
	}
	gate := policy.Filter{
		OrgID: "org_1", Alias: "no-export", Mode: policy.FilterModeGate,
		Model: "keera-decider", Prompt: "Does this request export customer data?",
	}

	for _, tc := range []struct {
		name, content string
		want          int
	}{
		{"fits", "hello", http.StatusOK},
		{"too large", strings.Repeat("word ", 200), http.StatusRequestEntityTooLarge},
	} {
		t.Run("gate "+tc.name, func(t *testing.T) {
			backend := decisionBackend(func(decisionCall) string { return yesAnswer(0.1) },
				jsonBackend(hiAnswer))
			h := newHarness(t, backend, small(), nil)
			h.src.resolved[testKey].Filters = []string{"no-export"}
			h.src.filters = map[string]policy.Filter{"org_1/no-export": gate}

			resp := h.post(t, "/v1/chat/completions",
				`{"model":"keera-code","messages":[{"role":"user","content":"`+tc.content+`"}]}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	t.Run("router", func(t *testing.T) {
		asked := make(chan struct{}, 1)
		backend := decisionBackend(func(decisionCall) string {
			asked <- struct{}{}
			return choiceAnswer("B", "A")
		}, jsonBackend(hiAnswer))
		models := small()
		models["keera-large"] = policy.Model{OrgID: "org_1", Alias: "keera-large",
			Kind: policy.KindChat, BackendModel: "large-served", Enabled: true}
		h := newHarness(t, backend, models, nil)
		h.withRouters(policy.Route{
			OrgID: "org_1", Router: "auto", Model: "keera-decider",
			Prompt:       "Which model should answer this request?",
			Destinations: []string{"keera-code", "keera-large"}, Fallback: "keera-code",
		})

		resp := h.post(t, "/v1/chat/completions",
			`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		select {
		case <-asked:
		default:
			t.Fatal("the decision model was never asked: the router fell back on size")
		}
	})
}
