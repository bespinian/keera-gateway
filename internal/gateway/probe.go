package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Probe is what one model check found.
//
// The failure that breaks coding agents is silent: when the vLLM tool-call
// parser does not match the model, the call comes back as text in `content`
// with `tool_calls` empty. The endpoint answers 200, nothing logs an error,
// and the agent never edits a file.
type Probe struct {
	Alias string `json:"alias"`
	// Backend is the base URL that answered, for a model with several.
	Backend string `json:"backend,omitempty"`
	// Reachable is a connection and an HTTP answer, whatever the status.
	Reachable bool `json:"reachable"`
	Status    int  `json:"status,omitempty"`
	// Streamed means the response arrived as server-sent events. A buffering
	// ingress turns the stream into one delivery at the end.
	Streamed bool `json:"streamed"`
	// ToolCalls is the one that matters: the model was given a tool and a
	// question only it can answer, and the call came back in `tool_calls`.
	ToolCalls bool `json:"tool_calls"`
	// ToolCallAsText means the model called the tool and the parser returned
	// it as prose: the misconfiguration worth naming.
	ToolCallAsText bool `json:"tool_call_as_text"`
	// Served is the model id the backend answered with. It catches a
	// backend_model the inference plane does not serve.
	Served     string `json:"served_model,omitempty"`
	TTFTMillis int64  `json:"ttft_ms,omitempty"`
	TotalMS    int64  `json:"total_ms,omitempty"`
	// Sample is the start of whatever prose came back.
	Sample string `json:"sample,omitempty"`
	Error  string `json:"error,omitempty"`
	// Warnings are the things that are not failures but are worth saying.
	Warnings []string `json:"warnings,omitempty"`
	// OK is the summary: it answered, and a chat model made a usable tool
	// call.
	OK bool `json:"ok"`
}

// probeTimeout bounds one check. A model still loading its weights will not
// answer in time, which is itself useful to know.
const probeTimeout = 30 * time.Second

// CheckModel exercises one model end to end against its own backends, with
// the same client, credentials and round-robin as real traffic. It skips
// serve: a check is not a tenant's request, so it has no rate limit, budget,
// billing or usage event.
func (s *Server) CheckModel(ctx context.Context, m policy.Model) Probe {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	p := Probe{Alias: m.Alias}
	if len(m.Backends) == 0 {
		p.Error = "this model has no backend configured"
		return p
	}
	switch m.Kind {
	case policy.KindEmbedding:
		return s.checkEmbedding(ctx, m, p)
	case policy.KindCompletion:
		return s.checkCompletion(ctx, m, p)
	default:
		return s.checkChat(ctx, m, p)
	}
}

// toolProbe is the tool the check offers. No model can answer it from its own
// knowledge, so prose instead of a call is a real failure.
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
	payload, err := json.Marshal(map[string]any{
		"model": m.BackendModel,
		"messages": []map[string]string{{
			"role":    "user",
			"content": "Is the build for the pipeline named 'release' passing? Use the tool.",
		}},
		"tools":       toolProbe,
		"tool_choice": "auto",
		"stream":      true,
		"max_tokens":  128,
		"temperature": 0,
	})
	if err != nil {
		p.Error = err.Error()
		return p
	}

	start := time.Now()
	resp, backend, err := s.probeDispatch(ctx, m, "/chat/completions", payload)
	if err != nil {
		p.Error = unreachable(err)
		return p
	}
	defer func() { _ = resp.Body.Close() }()
	p.Backend, p.Reachable, p.Status = backend, true, resp.StatusCode

	if resp.StatusCode >= 300 {
		p.Error = upstreamMessage(resp)
		return p
	}
	p.Streamed = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

	r := &probeReply{}
	if p.Streamed {
		readSSE(resp.Body, r.scan)
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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

	// The point of the check: the function's name in the prose means the call
	// was parsed as text, so the parser does not match the model.
	if !p.ToolCalls && strings.Contains(text, "get_build_status") {
		p.ToolCallAsText = true
	}

	switch {
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

// probeMessage is the part of a chat message, or of a streamed delta, that a
// check reads.
type probeMessage struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// probeReply gathers what a chat check's answer carried, chunk by chunk.
type probeReply struct {
	served    string
	toolCalls bool
	text      strings.Builder
	first     time.Time
}

// scan reads one chunk of the answer, streamed or whole.
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

// chatWarnings collects what is worth saying about a check that otherwise
// passed. None of these is a failure on its own.
func chatWarnings(p Probe, m policy.Model) []string {
	var out []string
	if p.OK && !p.Streamed {
		out = append(out, "the backend did not stream. Editors show a completion token by "+
			"token; buffered, it arrives all at once at the end.")
	}
	if p.Served != "" && m.BackendModel != "" && p.Served != m.BackendModel {
		out = append(out, fmt.Sprintf("the backend answered as %q, not %q - check the "+
			"inference plane's --served-model-name.", p.Served, m.BackendModel))
	}
	return out
}

func (s *Server) checkCompletion(ctx context.Context, m policy.Model, p Probe) Probe {
	payload, _ := json.Marshal(map[string]any{
		"model": m.BackendModel, "prompt": "func hello() {", "max_tokens": 16, "stream": false,
	})
	start := time.Now()
	resp, backend, err := s.probeDispatch(ctx, m, "/completions", payload)
	if err != nil {
		p.Error = unreachable(err)
		return p
	}
	defer func() { _ = resp.Body.Close() }()
	p.Backend, p.Reachable, p.Status = backend, true, resp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	p.TotalMS = time.Since(start).Milliseconds()
	if resp.StatusCode >= 300 {
		p.Error = upstreamMessage(resp)
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
	p.Warnings = chatWarnings(p, m)
	return p
}

func (s *Server) checkEmbedding(ctx context.Context, m policy.Model, p Probe) Probe {
	payload, _ := json.Marshal(map[string]any{"model": m.BackendModel, "input": "keera"})
	start := time.Now()
	resp, backend, err := s.probeDispatch(ctx, m, "/embeddings", payload)
	if err != nil {
		p.Error = unreachable(err)
		return p
	}
	defer func() { _ = resp.Body.Close() }()
	p.Backend, p.Reachable, p.Status = backend, true, resp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	p.TotalMS = time.Since(start).Milliseconds()
	if resp.StatusCode >= 300 {
		p.Error = upstreamMessage(resp)
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
	p.Warnings = chatWarnings(p, m)
	return p
}

// probeDispatch is dispatch, plus which backend answered, so a check can name
// the URL it reached.
func (s *Server) probeDispatch(ctx context.Context, m policy.Model, path string,
	payload []byte) (*http.Response, string, error) {
	resp, err := s.dispatch(ctx, m, path, payload)
	if err != nil {
		return nil, "", err
	}
	backend := ""
	if resp.Request != nil && resp.Request.URL != nil {
		backend = strings.TrimSuffix(resp.Request.URL.String(), path)
	}
	return resp, backend, nil
}

// readSSE feeds each data payload of a server-sent event stream to fn.
func readSSE(body io.Reader, fn func([]byte)) {
	buf := make([]byte, 0, 8<<10)
	chunk := make([]byte, 4<<10)
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := strings.Index(string(buf), "\n\n")
				if i < 0 {
					break
				}
				for line := range strings.SplitSeq(string(buf[:i]), "\n") {
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if data == "" || data == "[DONE]" {
						continue
					}
					fn([]byte(data))
				}
				buf = buf[i+2:]
			}
		}
		if err != nil {
			return
		}
	}
}

// upstreamMessage turns a refusal from the inference plane into the sentence an
// operator should read, preferring what the backend itself said.
func upstreamMessage(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return "the backend answered " + upstreamComplaint(body, resp.StatusCode)
}

// upstreamComplaint renders a refusal from the inference plane as a status
// and, if the backend gave one, its message. The caller adds the subject ("the
// backend", "its model").
func upstreamComplaint(body []byte, status int) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)
	msg := envelope.Error.Message
	if msg == "" {
		msg = envelope.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(sample(string(body)))
	}
	if msg == "" {
		return strconv.Itoa(status)
	}
	return strconv.Itoa(status) + ": " + msg
}

// unreachable explains a connection that never happened. Usually it is a
// first start: vLLM loads weights for minutes before it listens, and saying so
// saves a needless restart.
func unreachable(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		return "the backend did not answer within 30s. On a first start the inference plane " +
			"is still downloading and loading weights, which takes minutes - wait rather " +
			"than restart. Otherwise: " + err.Error()
	}
	return "the backend could not be reached: " + err.Error()
}

func sample(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
