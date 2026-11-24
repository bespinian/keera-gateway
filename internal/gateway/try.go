package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A try puts prompts an administrator wrote through one filter or route step,
// and shows everything its model was sent and answered. A check says whether a
// filter or step works at all; a try is for finding out why it decided as it
// did on a prompt that went wrong.
//
// Like a check, it has no rate limit, budget or usage row, but a model on the
// deployment's key needs credit and is billed. Nothing it reads or writes is
// stored: the prompts are the administrator's own, and go back only to them.

// How an answer was read. A gate's word or a step's letter decides when it is
// there; otherwise the first token's probabilities do. See choice.go.
const (
	ReadText          = "text"
	ReadProbabilities = "probabilities"
)

// Why a call was made.
const (
	PurposeDecide  = "decide"
	PurposeRetry   = "retry"
	PurposeExplain = "explain"
)

const (
	// explainTokens is room for a sentence or two, and for a model that
	// thinks a little first without showing it.
	explainTokens = 400
	// explainTimeout is longer than a route step's, since it writes prose.
	explainTimeout = time.Minute
	// maxCallResponse caps the raw answer a try shows. A rewrite of a long
	// prompt is still whole well within it.
	maxCallResponse = 256 << 10
)

// explainQuestion follows the model's answer. What it writes is its account
// of the answer, given afterwards, not a record of how it was computed.
const explainQuestion = "In one or two short sentences, say why you gave that answer: " +
	"what in the request decided it, and which part of your instruction applied. " +
	"Write plain prose, not a list, and do not repeat the request."

// CallMessage is one message of a chat request.
type CallMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ModelCall is one call a filter or route step made to its model, exactly as
// it was sent and answered.
type ModelCall struct {
	// Purpose is why it was made: decide, retry (asked again another way,
	// after an answer that could not be read) or explain.
	Purpose string `json:"purpose"`
	Model   string `json:"model"`
	// Path is where it went on the backend: /chat/completions, or /systemone
	// for a decision model.
	Path string `json:"path"`
	// Messages is the chat request's messages.
	Messages []CallMessage `json:"messages,omitempty"`
	// Request is the body as sent when it has no messages: a decision
	// model's. A chat request's would repeat the messages.
	Request json.RawMessage `json:"request,omitempty"`
	// Sent is false when the request was built but not sent, such as one too
	// large for the model's context.
	Sent   bool `json:"sent"`
	Status int  `json:"status,omitempty"`
	// Answer is what the model wrote.
	Answer string `json:"answer,omitempty"`
	// Reasoning is what a reasoning model thought before answering, when the
	// backend returns it.
	Reasoning string `json:"reasoning,omitempty"`
	// Response is the body as received, cut at 256 KiB.
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
	MS       int64  `json:"ms,omitempty"`
}

// callRecord is one call as it went out and came back.
type callRecord struct {
	path    string
	payload []byte
	// sent is false for a request built but not sent.
	sent bool
	// received is the body as the backend sent it, before a decision model's
	// answer is turned into a chat completion.
	received []byte
	status   int
	took     time.Duration
}

// callLog keeps a try's calls. Every method is safe on nil, which is what real
// traffic passes.
type callLog struct {
	calls []ModelCall
	// last is the latest question that got an answer, for explain to follow
	// up on.
	last *answered
}

type answered struct {
	m         policy.Model
	q         guardQuestion
	answer    string
	reasoning string
}

// note records one call. chat is the answer as a chat completion, which a
// decision model's is turned into.
func (l *callLog) note(m policy.Model, q guardQuestion, c callRecord, chat []byte, err error) {
	if l == nil {
		return
	}
	call := ModelCall{
		Purpose: PurposeDecide, Model: m.Alias, Path: c.path, Sent: c.sent,
		Status: c.status, MS: c.took.Milliseconds(),
	}
	switch {
	case len(q.followUp) > 0:
		call.Purpose = PurposeExplain
	case len(l.calls) > 0:
		call.Purpose = PurposeRetry
	}
	var req struct {
		Messages []CallMessage `json:"messages"`
	}
	_ = json.Unmarshal(c.payload, &req)
	call.Messages = req.Messages
	if len(call.Messages) == 0 {
		call.Request = c.payload
	}
	if len(c.received) > 0 {
		call.Response = shorten(string(c.received), maxCallResponse)
	}
	if c.status < 300 && len(chat) > 0 {
		call.Answer, _ = completionText(chat)
		call.Reasoning = reasoningText(c.received)
	}
	if err != nil {
		call.Error = err.Error()
	}
	l.calls = append(l.calls, call)
	// A decision model writes no text, so it cannot be asked why.
	if call.Purpose != PurposeExplain && call.Answer != "" && m.Kind != policy.KindDecision {
		l.last = &answered{m: m, q: q, answer: call.Answer, reasoning: call.Reasoning}
	}
}

// reasoningText reads what a reasoning model thought. vLLM, llama.cpp and
// DeepSeek call it reasoning_content; OpenRouter and newer vLLM, reasoning.
// Some send both, so only one is kept.
func reasoningText(raw []byte) string {
	var envelope struct {
		Choices []struct {
			Message writtenText `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Choices) == 0 {
		return ""
	}
	msg := envelope.Choices[0].Message
	return strings.TrimSpace(cmp.Or(msg.ReasoningContent, msg.Reasoning))
}

// unexplained reports whether the model answered without showing why. A
// reasoning model's thoughts come with its answer, and are a better account
// than one asked for afterwards. A model that gave no answer, or a decision
// model, has nothing to be asked about.
func (l *callLog) unexplained() bool {
	return l != nil && l.last != nil && l.last.reasoning == ""
}

// explain asks the model why it answered as it did, in the same conversation,
// when it showed no reasoning. Most filters and steps answer one word or one
// letter, with no room to think, so this is the only way to hear its reasons.
// It returns the explanation, why it could not be had, and what asking cost.
func (s *Server) explain(ctx context.Context, orgID string, log *callLog,
) (text, failed string, micros int64) {
	if !log.unexplained() {
		return "", "", 0
	}
	m, q := log.last.m, log.last.q
	q.followUp = []CallMessage{
		{Role: "assistant", Content: log.last.answer},
		{Role: "user", Content: explainQuestion},
	}
	q.decision, q.outputTokens, q.timeout = nil, explainTokens, explainTimeout
	q.limit, q.subject = maxRouteResponseBytes, "the model"
	ans, err := s.askGuard(ctx, m, q, false)
	s.billCheck(orgID, ans.bills)
	if err == nil && ans.status >= 300 {
		err = errors.New("the model answered " + upstreamComplaint(ans.raw, ans.status))
	}
	if err == nil {
		text, err = completionText(ans.raw)
	}
	if err != nil {
		return "", err.Error(), ans.micros
	}
	return text, "", ans.micros
}

// Asked is what one prompt of a try asked the model, and what that took.
type Asked struct {
	// Calls is every call to the model, in order.
	Calls []ModelCall `json:"calls,omitempty"`
	// Explanation is the model's own account of its answer, asked for
	// afterwards when its call shows no reasoning.
	Explanation  string `json:"explanation,omitempty"`
	ExplainError string `json:"explain_error,omitempty"`
	// MS is how long the answer took, without the explanation.
	MS         int64 `json:"ms,omitempty"`
	CostMicros int64 `json:"cost_micros,omitempty"`
}

// ask runs one prompt of a try, which returns what it cost, and bills it. It
// then asks the model why, when it showed no reasoning.
func (s *Server) ask(ctx context.Context, orgID string,
	run func(*callLog) (int64, []policy.BillLine),
) Asked {
	log := &callLog{}
	start := time.Now()
	micros, bills := run(log)
	s.billCheck(orgID, bills)
	a := Asked{MS: time.Since(start).Milliseconds(), CostMicros: micros}
	var explained int64
	a.Explanation, a.ExplainError, explained = s.explain(ctx, orgID, log)
	a.CostMicros += explained
	a.Calls = log.calls
	return a
}

/* ---------------------------------------------------------------- filters */

// FilterTry is what trying one filter on prompts found.
type FilterTry struct {
	Alias  string            `json:"alias"`
	Mode   policy.FilterMode `json:"mode"`
	Model  string            `json:"model,omitempty"`
	Shadow bool              `json:"shadow,omitempty"`
	// Trials is one per prompt, in the order given.
	Trials     []FilterTrial `json:"trials"`
	CostMicros int64         `json:"cost_micros,omitempty"`
	// Free says no model ran, so a zero cost is not read as missing.
	Free bool `json:"free,omitempty"`
	// Error is why the filter could not run at all.
	Error string `json:"error,omitempty"`
}

// FilterTrial is one prompt, sent through the filter as a request of its own.
type FilterTrial struct {
	Prompt string `json:"prompt"`
	// Outcome is what an enforcing filter would do: pass, rewrite, refuse, or
	// error when it could not run.
	Outcome store.FilterOutcome `json:"outcome"`
	// After is the prompt as it would be forwarded, after a rewrite.
	After  string `json:"after,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// Confidence is a gate verdict's share of probability, from 0 to 1. 0 means
	// unknown.
	Confidence float64 `json:"confidence,omitempty"`
	// Read is how a gate's verdict was read: from its text, or from the first
	// token's probabilities when the text said neither.
	Read string `json:"read,omitempty"`
	// Hits is which pattern rules fired.
	Hits []patternHit `json:"hits,omitempty"`
	Asked
}

// TryFilter sends each prompt through a filter as its own request, and asks
// the model why when it showed no reasoning.
func (s *Server) TryFilter(ctx context.Context, f policy.Filter, prompts []string) FilterTry {
	t := FilterTry{Alias: f.Alias, Mode: f.Mode, Model: f.Model, Shadow: f.Shadow, Trials: []FilterTrial{}}
	if !f.UsesModel() {
		t.Free = true
		rules := s.patterns.rulesFor(f)
		for _, prompt := range prompts {
			start := time.Now()
			out, hits, err := runPattern(rules, []string{prompt})
			tr := FilterTrial{
				Prompt: prompt, Hits: hits,
				MS: time.Since(start).Milliseconds(),
			}
			tr.settle(out, err)
			t.Trials = append(t.Trials, tr)
		}
		return t
	}
	m, why := s.filterModel(f)
	if why != "" {
		t.Error = why + ", so this filter cannot run"
		return t
	}
	if t.Error = s.checkCredit(f.OrgID, m); t.Error != "" {
		return t
	}

	for _, prompt := range prompts {
		if ctx.Err() != nil {
			break
		}
		tr := FilterTrial{Prompt: prompt}
		tr.Asked = s.ask(ctx, f.OrgID, func(log *callLog) (int64, []policy.BillLine) {
			out, cost, err := s.runFilter(ctx, f, m, []string{prompt}, nil, log)
			tr.Confidence, tr.Read = cost.confidence, cost.read
			tr.settle(out, err)
			return cost.micros, cost.bills
		})
		t.CostMicros += tr.CostMicros
		t.Trials = append(t.Trials, tr)
	}
	return t
}

// settle reads what the filter did with one prompt, as chainFilter does.
func (tr *FilterTrial) settle(out []string, err error) {
	tr.Outcome, tr.Reason, _ = filterOutcome([]string{tr.Prompt}, out, err)
	switch tr.Outcome {
	case store.FilterError:
		tr.Error = err.Error()
	case store.FilterRewrite:
		tr.After = out[0]
	}
}

/* ------------------------------------------------------------ route steps */

// RouteTry is what trying one route step on prompts found.
type RouteTry struct {
	Step  string           `json:"step"`
	Mode  policy.RouteMode `json:"mode"`
	Model string           `json:"model,omitempty"`
	// Destinations is what the step offered, in the order of its letters.
	Destinations []string `json:"destinations"`
	// Dropped names the destinations not offered, and why.
	Dropped    []string     `json:"dropped,omitempty"`
	Trials     []RouteTrial `json:"trials"`
	CostMicros int64        `json:"cost_micros,omitempty"`
	// Error is why the step could not decide at all.
	Error string `json:"error,omitempty"`
}

// RouteTrial is one prompt and where the step sent it.
type RouteTrial struct {
	Prompt string `json:"prompt"`
	Chosen string `json:"chosen,omitempty"`
	// Confidence is the chosen destination's share of probability, from 0 to
	// 1. 0 means unknown.
	Confidence float64 `json:"confidence,omitempty"`
	// Shares is each destination's share, in the order of Destinations, when
	// the choice was read from the probabilities.
	Shares []float64 `json:"shares,omitempty"`
	// Read is how the choice was read: from its text, or from the first
	// token's probabilities.
	Read string `json:"read,omitempty"`
	// Error is a decision the step could not make, and Becomes what happens
	// to such a request.
	Error   string `json:"error,omitempty"`
	Becomes string `json:"becomes,omitempty"`
	Asked
}

// TryRoute asks an instruction step where each prompt should go, and asks the
// model why when it showed no reasoning. The other modes read no prompt, so
// there is nothing to try; their check shows all they do.
func (s *Server) TryRoute(ctx context.Context, rt policy.Route, prompts []string) RouteTry {
	t := RouteTry{Step: rt.Step, Mode: rt.Mode, Model: rt.ReadsWith(), Trials: []RouteTrial{}}
	if !rt.Mode.Decides() {
		t.Error = "this route step does not read requests, so there is nothing to try; its " +
			"check shows where it sends them"
		return t
	}
	var m policy.Model
	if t.Destinations, t.Dropped, m, t.Error = s.readyToDecide(rt); t.Error != "" {
		return t
	}

	for _, prompt := range prompts {
		if ctx.Err() != nil {
			break
		}
		tr := RouteTrial{Prompt: prompt}
		tr.Asked = s.ask(ctx, rt.OrgID, func(log *callLog) (int64, []policy.BillLine) {
			chose, err := s.decide(ctx, rt, m, t.Destinations, routeWindow([]string{prompt}), log)
			tr.Chosen, tr.Confidence = chose.alias, chose.confidence
			tr.Shares, tr.Read = chose.shares, chose.read
			if err != nil {
				tr.Error, tr.Becomes = err.Error(), undecidedBecomes(rt)
			}
			return chose.micros, chose.bills
		})
		t.CostMicros += tr.CostMicros
		t.Trials = append(t.Trials, tr)
	}
	return t
}
