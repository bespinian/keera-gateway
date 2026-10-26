package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Probe is what one model check found. It targets a silent failure: when the
// vLLM tool-call parser does not match the model, the call comes back as text
// in `content` with `tool_calls` empty, and nothing logs an error.
type Probe struct {
	Alias string `json:"alias"`
	// Backend is the base URL that answered, for a model with several.
	Backend string `json:"backend,omitempty"`
	// Reachable is a connection and an HTTP answer, whatever the status.
	Reachable bool `json:"reachable"`
	Status    int  `json:"status,omitempty"`
	// Streamed means the response arrived as server-sent events. A buffering
	// ingress delivers the stream all at once at the end.
	Streamed bool `json:"streamed"`
	// ToolCalls is the key result: the model's call came back in
	// `tool_calls`.
	ToolCalls bool `json:"tool_calls"`
	// ToolCallAsText means the parser returned the model's tool call as
	// prose.
	ToolCallAsText bool `json:"tool_call_as_text"`
	// ToolsOff means the backend refuses any request with tools, as vLLM does
	// without --enable-auto-tool-choice. The check then asks without them.
	ToolsOff bool `json:"tools_off,omitempty"`
	// Served is the model id the backend answered with. It catches a
	// backend_model the plane does not serve.
	Served     string `json:"served_model,omitempty"`
	TTFTMillis int64  `json:"ttft_ms,omitempty"`
	TotalMS    int64  `json:"total_ms,omitempty"`
	// Sample is the start of whatever prose came back.
	Sample   string   `json:"sample,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// NoCredit says the model is on the deployment's key and the
	// organisation has no credit, so it was not called.
	NoCredit bool `json:"no_credit,omitempty"`
	// OK means it answered, and a chat model made a usable tool call, or its
	// backend refuses tools and plain chat works (ToolsOff).
	OK bool `json:"ok"`
}

// probeTimeout limits one check. A model still loading its weights will not
// answer in time, which is useful to know.
const probeTimeout = 30 * time.Second

// CheckModel tests one model end to end, with the same client, credentials
// and round-robin as real traffic. It skips serve, so there is no rate limit,
// budget or usage event. On the deployment's key it needs credit and is
// billed.
func (s *Server) CheckModel(ctx context.Context, m policy.Model) Probe {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	p := Probe{Alias: m.Alias}
	switch {
	case len(m.Backends) == 0:
		p.Error = "this model has no backend configured"
		return p
	case m.Subscription:
		p.Error = "this model is paid by each person's own Claude subscription, so the " +
			"gateway holds no credential to check it with; send it a message from Claude Code"
		return p
	case m.Locked:
		p.Error = "this model " + lockedReason(m) + "; " + policy.LockedMessage(m.OrgName)
		return p
	}
	if why := s.checkCredit(m.OrgID, m); why != "" {
		p.Error, p.NoCredit = why, true
		return p
	}
	switch m.Kind {
	case policy.KindEmbedding:
		return s.checkEmbedding(ctx, m, p)
	case policy.KindCompletion:
		return s.checkCompletion(ctx, m, p)
	case policy.KindDecision:
		return s.checkDecision(ctx, m, p)
	default:
		return s.checkChat(ctx, m, p)
	}
}

// toolProbe is the tool the check offers. No model can answer it on its own,
// so prose instead of a call is a real failure.
var toolProbe = []map[string]any{{
	"type": "function",
	"function": map[string]any{
		"name":        "get_build_status",
		"description": "Get the current build status of a named CI pipeline. The only way to know a build status.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pipeline": map[string]any{"type": "string"}},
			"required":   []string{"pipeline"},
		},
	},
}}

func (s *Server) checkChat(ctx context.Context, m policy.Model, p Probe) Probe {
	start := time.Now()
	resp := s.probeCall(ctx, m, "/chat/completions", chatProbe(m, true), &p)
	if resp == nil && toolsOff(p) {
		// A backend that refuses tools still works without them.
		p = Probe{Alias: p.Alias, ToolsOff: true}
		start = time.Now()
		resp = s.probeCall(ctx, m, "/chat/completions", chatProbe(m, false), &p)
	}
	if resp == nil {
		return p
	}
	defer func() { _ = resp.Body.Close() }()
	p.Streamed = httpx.MediaType(resp.Header.Get("Content-Type")) == "text/event-stream"

	r := &probeReply{}
	if p.Streamed {
		_ = scanSSE(resp.Body, s.opts.MaxResponseBytes, func(chunk []byte) error {
			r.scan(chunk)
			return nil
		})
	} else {
		body, ok := s.probeBody(resp, &p)
		if !ok {
			return p
		}
		r.first = time.Now()
		r.scan(body)
	}
	p.Served, p.ToolCalls = r.served, r.toolCalls
	text := r.text.String()

	p.TotalMS = time.Since(start).Milliseconds()
	if !r.first.IsZero() {
		p.TTFTMillis = r.first.Sub(start).Milliseconds()
	}
	p.Sample = sample(text)

	// The function's name in the prose means the parser does not match.
	p.ToolCallAsText = !p.ToolCalls && strings.Contains(text, "get_build_status")

	switch {
	case p.ToolsOff && text == "":
		p.Error = "the backend answered 200 but the reply was empty"
	case p.ToolsOff:
		p.OK = true
	case p.ToolCallAsText:
		p.Error = "the model produced a tool call as text: `tool_calls` was empty while the " +
			"function name appeared in `content`. The vLLM tool-call parser does not match " +
			"this model - check --enable-auto-tool-choice and --tool-call-parser."
	case !p.ToolCalls:
		p.Error = "the model answered but called no tool. A coding agent will read this " +
			"deployment as a model that never edits a file."
	default:
		p.OK = true
	}
	p.Warnings = chatWarnings(p, m)
	return p
}

// chatProbe is the chat check's request: the tool question, or, for a
// backend that refuses tools, a plain one.
func chatProbe(m policy.Model, tools bool) map[string]any {
	ask := "Is the build for the pipeline named 'release' passing? Use the tool."
	if !tools {
		ask = "Say hello in one word."
	}
	body := map[string]any{
		"model":       m.BackendModel,
		"messages":    []map[string]string{{"role": "user", "content": ask}},
		"stream":      true,
		"max_tokens":  128,
		"temperature": 0,
	}
	if tools {
		body["tools"], body["tool_choice"] = toolProbe, "auto"
	}
	return body
}

// toolsOff reports vLLM refusing tools because it was started without tool
// calling.
func toolsOff(p Probe) bool {
	return p.Status == http.StatusBadRequest && strings.Contains(p.Error, "--enable-auto-tool-choice")
}

// probeMessage is the part of a chat message or streamed delta a check reads.
type probeMessage struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type probeReply struct {
	served    string
	toolCalls bool
	text      strings.Builder
	first     time.Time
}

// scan reads one chunk of the answer, or the whole answer.
func (r *probeReply) scan(chunk []byte) {
	var c struct {
		Model   string `json:"model"`
		Choices []struct {
			Delta   probeMessage `json:"delta"`
			Message probeMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &c) != nil {
		return
	}
	if c.Model != "" {
		r.served = c.Model
	}
	for _, ch := range c.Choices {
		if len(ch.Delta.ToolCalls) > 0 || len(ch.Message.ToolCalls) > 0 {
			r.toolCalls = true
		}
		for _, s := range []string{ch.Delta.Content, ch.Message.Content} {
			if s == "" {
				continue
			}
			if r.first.IsZero() {
				r.first = time.Now()
			}
			r.text.WriteString(s)
		}
	}
}

// chatWarnings collects notes on a chat check that passed. None is a failure.
func chatWarnings(p Probe, m policy.Model) []string {
	var out []string
	if p.ToolsOff {
		out = append(out, "the backend refuses requests that offer tools, so coding agents "+
			"and other clients that use tools get a 400. Plain chat works. To allow tools, "+
			"start vLLM with --enable-auto-tool-choice and a --tool-call-parser that "+
			"matches this model.")
	}
	if p.OK && !p.Streamed {
		out = append(out, "the backend did not stream. Editors show a completion token by "+
			"token; buffered, it arrives all at once at the end.")
	}
	return append(out, servedWarnings(p, m)...)
}

// servedWarnings is the warning every kind of check can give. Completion and
// embedding checks ask for no stream, so they do not warn about one.
func servedWarnings(p Probe, m policy.Model) []string {
	if p.Served != "" && m.BackendModel != "" && p.Served != m.BackendModel {
		return []string{fmt.Sprintf("the backend answered as %q, not %q - check the "+
			"inference plane's --served-model-name.", p.Served, m.BackendModel)}
	}
	return nil
}

// decisionProbe is the question a decision model's check asks. The answer is
// obvious, so a wrong one means the model is not reading its input.
var decisionProbe = decisionQuestion{
	Type:         "choice",
	Instructions: "Which language is this text written in?",
	Criteria:     map[string]string{"A": "English", "B": "Go source code"},
}

// checkDecision asks one question with an obvious answer. Routers and gates
// read one answer, so there is no stream or tool call to check.
func (s *Server) checkDecision(ctx context.Context, m policy.Model, p Probe) Probe {
	body, ok := s.probeAnswer(ctx, m, decisionPath,
		decisionBody(m, decisionProbe, []string{"func Add(a, b int) int { return a + b }"}), &p)
	if !ok {
		return p
	}
	raw, err := chatFromDecision(body, decisionProbe)
	if err != nil {
		p.Error = "the backend answered 200, but " + err.Error()
		return p
	}
	letter, share, _ := readChoice(raw, len(decisionProbe.Criteria))
	p.Sample = fmt.Sprintf("%s (%.0f%% sure)", decisionProbe.Criteria[string(choiceLetters[letter])],
		share*100)
	if letter != 1 {
		p.Error = "it took Go source code for English: the model is not reading what it is sent"
		return p
	}
	p.OK = true
	return p
}

func (s *Server) checkCompletion(ctx context.Context, m policy.Model, p Probe) Probe {
	body, ok := s.probeAnswer(ctx, m, "/completions", map[string]any{
		"model": m.BackendModel, "prompt": "func hello() {", "max_tokens": 16, "stream": false,
	}, &p)
	if !ok {
		return p
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(body, &out)
	p.Served = out.Model
	if len(out.Choices) > 0 {
		p.Sample = sample(out.Choices[0].Text)
	}
	if p.Sample == "" {
		p.Error = "the backend answered 200 but the completion was empty"
		return p
	}
	p.OK = true
	p.Warnings = servedWarnings(p, m)
	return p
}

func (s *Server) checkEmbedding(ctx context.Context, m policy.Model, p Probe) Probe {
	body, ok := s.probeAnswer(ctx, m, "/embeddings",
		map[string]any{"model": m.BackendModel, "input": "keera"}, &p)
	if !ok {
		return p
	}
	var out struct {
		Model string `json:"model"`
		Data  []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &out)
	p.Served = out.Model
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		p.Error = "the backend answered 200 but returned no embedding"
		return p
	}
	p.Sample = fmt.Sprintf("%d dimensions", len(out.Data[0].Embedding))
	p.OK = true
	p.Warnings = servedWarnings(p, m)
	return p
}

// probeCall sends a check's request and records which backend answered and
// its status. It returns nil when the check is over, with p.Error saying why.
func (s *Server) probeCall(ctx context.Context, m policy.Model, path string,
	request map[string]any, p *Probe,
) *http.Response {
	payload, err := json.Marshal(request)
	if err != nil {
		p.Error = err.Error()
		return nil
	}
	resp, err := s.send(ctx, m, outbound{path: path, payload: payload})
	if err != nil {
		p.Error = unreachable(err)
		return nil
	}
	p.Reachable, p.Status = true, resp.StatusCode
	if resp.StatusCode < 300 {
		// Billed at its maximum: a check may not read far enough to find the
		// usage record, and the cost is small.
		output, _ := request["max_tokens"].(int)
		if line, ok := m.Bill(policy.Tokens{
			Input:  len(payload) / bytesPerToken,
			Output: output,
		}); ok {
			s.billCheck(m.OrgID, []policy.BillLine{line})
		}
	}
	if resp.Request != nil && resp.Request.URL != nil {
		p.Backend = strings.TrimSuffix(resp.Request.URL.String(), path)
	}
	if resp.StatusCode >= 300 {
		// A body too large to read still leaves the status to report.
		body, _ := readCapped(resp.Body, s.opts.MaxResponseBytes)
		_ = resp.Body.Close()
		p.Error = "the backend answered " + upstreamComplaint(body, resp.StatusCode)
		return nil
	}
	return resp
}

// probeAnswer sends a check's request and reads the whole answer, timing
// both. It returns false when the check is over, with p.Error saying why.
func (s *Server) probeAnswer(ctx context.Context, m policy.Model, path string,
	request map[string]any, p *Probe,
) ([]byte, bool) {
	start := time.Now()
	resp := s.probeCall(ctx, m, path, request, p)
	if resp == nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	body, ok := s.probeBody(resp, p)
	if !ok {
		return nil, false
	}
	p.TotalMS = time.Since(start).Milliseconds()
	return body, true
}

// probeBody reads a whole answer, or says in p why it could not.
func (s *Server) probeBody(resp *http.Response, p *Probe) ([]byte, bool) {
	body, err := readCapped(resp.Body, s.opts.MaxResponseBytes)
	if err != nil {
		p.Error = "the backend's answer could not be read: " + err.Error()
		return nil, false
	}
	return body, true
}

// upstreamComplaint renders a refusal as its status and the backend's
// message, if any. The caller adds the subject.
func upstreamComplaint(body []byte, status int) string {
	if msg := upstreamText(body); msg != "" {
		return strconv.Itoa(status) + ": " + sample(msg)
	}
	return strconv.Itoa(status)
}

// unreachable explains a failed connection. Usually vLLM is still loading
// weights, which takes minutes; saying so saves a needless restart.
func unreachable(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the backend did not answer within " + probeTimeout.String() + ". On a first start the inference plane " +
			"is still downloading and loading weights, which takes minutes - wait rather " +
			"than restart. Otherwise: " + err.Error()
	}
	return "the backend could not be reached: " + err.Error()
}

// sample is the start of s, short enough for a report, cut between
// characters.
func sample(s string) string {
	return shorten(strings.TrimSpace(s), 400)
}
