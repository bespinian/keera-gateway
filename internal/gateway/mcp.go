package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The MCP proxy: the gateway in front of the MCP servers an agent calls, as it
// is in front of the models.
//
// A tool call is where an agent's data leaves for somewhere other than a
// model: an issue tracker, a chat channel, a cloud API. So a client reaches an
// MCP server through /api/mcp/{alias} with its Keera key, and the gateway
//
//   - lists only the tools the key may call, and refuses the others,
//   - runs the key's filters over a call's arguments before they leave,
//   - presents the server's credential, which never reaches a laptop,
//   - and records every call: which tool, what came of it, how long it took
//     and how much went each way, but never what.
//
// It speaks MCP's Streamable HTTP transport and passes everything else
// through: sessions, notifications, the server's own requests, resources and
// prompts.

// mcpClientHeaders are the client's headers the server is sent. Everything
// else stays behind, above all the client's Authorization, which is its Keera
// key.
var mcpClientHeaders = []string{
	"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id",
}

// JSON-RPC error codes the proxy answers with itself.
const (
	rpcParseError    = -32700
	rpcInvalidParams = -32602
)

// rpcMessage is one JSON-RPC message: a request, a notification or a response.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (m rpcMessage) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

func (m rpcMessage) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// idKey is a request id as a map key. A string id and a number id are
// different ids, and stay so.
func idKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

// mcpExchange is one HTTP request to an MCP server, and what the gateway waits
// to hear back about.
type mcpExchange struct {
	s   *Server
	w   http.ResponseWriter
	r   *http.Request
	res *policy.Resolved
	srv policy.MCPServer
	// session is the session the client named in a header, hashed, or empty.
	session string
	client  string
	// pending is every request the answer will carry that the gateway acts on,
	// by id.
	pending map[string]*pendingRPC
}

// pendingRPC is one request waiting for its response.
type pendingRPC struct {
	method   string
	tool     string
	start    time.Time
	argBytes int
	filters  filterRun
}

// mcpBegin authenticates the caller and finds the server. A server the key
// may not call a single tool of is reported as missing, like a model.
func (s *Server) mcpBegin(w http.ResponseWriter, r *http.Request) (*mcpExchange, bool) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return nil, false
	}
	alias := r.PathValue("alias")
	srv, found := s.src.MCPServer(alias)
	if !found || !srv.Enabled || !res.AllowsServer(alias) {
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "mcp_server_not_found",
			s.advise("the MCP server '"+alias+"' does not exist or this key may not use it"))
		return nil, false
	}
	x := &mcpExchange{
		s: s, w: w, r: r, res: res, srv: srv,
		client:  connect.Identify(r.Header.Get(connect.ClientHeader), r.UserAgent()),
		pending: map[string]*pendingRPC{},
	}
	if stated := statedSession(r); stated != "" {
		x.session = store.StatedSessionKeyFor(res.Key.ID, stated)
	}
	return x, true
}

// mcpPost carries the client's messages to the server, and the answer back.
func (s *Server) mcpPost(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpBegin(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				"request_too_large", "the request body exceeds the gateway's limit")
		}
		return
	}
	out, forward := x.inspect(raw)
	if !forward {
		return
	}
	resp, err := s.mcpSend(r.Context(), x.srv, http.MethodPost, out, r.Header)
	if err != nil {
		x.unreachable(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	x.relay(resp)
}

// mcpForward carries a GET, which opens the server's own event stream, or a
// DELETE, which ends a session. Neither has a body to inspect.
func (s *Server) mcpForward(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpBegin(w, r)
	if !ok {
		return
	}
	resp, err := s.mcpSend(r.Context(), x.srv, r.Method, nil, r.Header)
	if err != nil {
		x.unreachable(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	x.relay(resp)
}

// mcpSend sends one request to the server, with its credential.
func (s *Server) mcpSend(ctx context.Context, srv policy.MCPServer, method string,
	payload []byte, client http.Header,
) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, srv.URL, body)
	if err != nil {
		return nil, err
	}
	for _, h := range mcpClientHeaders {
		if v := client.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if payload != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential := srv.Credential(s.opts.APIKeys); credential != "" {
		if srv.AuthHeader == "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		} else {
			req.Header.Set(srv.AuthHeader, credential)
		}
	}
	return s.client.Do(req)
}

// ----------------------------------------------------------- the way out

// inspect reads the client's messages, answers the ones the gateway refuses
// itself, and returns what to forward. forward is false when the answer has
// already been written.
//
// Every message is forwarded as the gateway read it, re-encoded, so a server
// that reads JSON differently - the first of two repeated keys, say - cannot
// be sent a call other than the one that was checked.
func (x *mcpExchange) inspect(raw []byte) (out []byte, forward bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return x.inspectBatch(trimmed)
	}
	var msg rpcMessage
	if json.Unmarshal(trimmed, &msg) != nil {
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: rpcParseError, Message: "the body is not a JSON-RPC message"}})
		return nil, false
	}
	if refuseUnanswerable(x, msg) {
		return nil, false
	}
	if msg.isRequest() {
		switch msg.Method {
		case "tools/call":
			if !x.toolCall(&msg) {
				return nil, false
			}
		case "tools/list":
			x.pending[idKey(msg.ID)] = &pendingRPC{method: msg.Method, start: time.Now()}
		}
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: msg.ID,
			Error: &rpcError{Code: rpcParseError, Message: "the message could not be re-encoded"}})
		return nil, false
	}
	return encoded, true
}

// inspectBatch handles an array of messages, which MCP versions before
// 2025-06-18 allowed. A tool call must come on its own, so that a refusal can
// be the whole answer.
func (x *mcpExchange) inspectBatch(raw []byte) ([]byte, bool) {
	var msgs []rpcMessage
	if json.Unmarshal(raw, &msgs) != nil {
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: rpcParseError, Message: "the body is not a JSON-RPC batch"}})
		return nil, false
	}
	for _, m := range msgs {
		if refuseUnanswerable(x, m) {
			return nil, false
		}
		switch {
		case m.isRequest() && m.Method == "tools/call":
			x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{
				Code: rpcInvalidParams, Message: "the gateway takes a tool call only on its own, " +
					"not in a batch; send it as a request of its own",
			}})
			return nil, false
		case m.isRequest() && m.Method == "tools/list":
			x.pending[idKey(m.ID)] = &pendingRPC{method: m.Method, start: time.Now()}
		}
	}
	encoded, err := json.Marshal(msgs)
	return encoded, err == nil
}

// refuseUnanswerable refuses a tool call sent as a notification, without an
// id. It would get no answer to trim or record, and a server that ran it
// anyway would run a tool nobody checked. It reports whether it answered.
func refuseUnanswerable(x *mcpExchange, m rpcMessage) bool {
	if m.Method != "tools/call" || len(m.ID) > 0 {
		return false
	}
	x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{
		Code: rpcInvalidParams, Message: "a tool call needs an id; the gateway does not forward " +
			"one sent as a notification",
	}})
	return true
}

// toolCall decides one tool call: whether the key may make it, and what the
// filters make of its arguments. It returns false when the call was answered
// here.
func (x *mcpExchange) toolCall(msg *rpcMessage) bool {
	p := &pendingRPC{method: msg.Method, start: time.Now()}
	params, err := parseBody(msg.Params)
	if err == nil {
		name, ok := params.str("name")
		if !ok || name == "" {
			err = errors.New("the 'name' field is required")
		}
		p.tool = name
	}
	if err != nil {
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{
			Code: rpcInvalidParams, Message: "the tool call could not be read: " + err.Error(),
		}})
		return false
	}
	args, _ := params.value("arguments")
	p.argBytes = len(args)

	// A tool the key may not call is hidden from its list, so to the client it
	// is a tool that does not exist, which MCP answers with this error.
	if !x.res.AllowsTool(x.srv.Alias, p.tool) {
		why := "Unknown tool: " + p.tool + ". This key may not call '" + x.srv.Alias + "/" +
			p.tool + "' through the gateway"
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{
			Code: rpcInvalidParams, Message: x.s.advise(why),
		}})
		x.record(p, store.ToolDenied, why, 0)
		return false
	}

	if len(x.res.Filters) > 0 {
		run, ref := x.s.applyFilters(x.r.Context(), x.res, params, argumentsText, nil)
		p.filters = run
		if run.micros > 0 {
			x.s.budgets.Charge(x.res.Scopes, run.micros, time.Now())
		}
		if ref != nil {
			// A refusal is the tool's answer, so the model reads why and can
			// carry on, as it would after any failed tool.
			outcome := store.ToolRefused
			if ref.code != "filter_refused" {
				outcome = store.ToolError
			}
			x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: toolErrorResult(x.s.advise(ref.msg))})
			x.record(p, outcome, ref.msg, 0)
			return false
		}
		if x.r.Context().Err() != nil {
			return false
		}
		if run.rewrote {
			args, _ = params.value("arguments")
			p.argBytes = len(args)
		}
	}
	// Re-encoded from what was read, rewritten or not. See inspect.
	msg.Params = params.encode()
	x.pending[idKey(msg.ID)] = p
	return true
}

// argumentsText is what filters read in a tool call: every string anywhere in
// its arguments. Keys are not text, and numbers and booleans are left as they
// are.
func argumentsText(b *body) (textDoc, error) {
	d := newTreeDoc()
	args, err := d.root(b, "arguments")
	if err != nil {
		return nil, err
	}
	if s, ok := args.(string); ok {
		d.add(s, func(v string) { d.roots["arguments"] = v })
	}
	addStrings(d, args)
	return d, nil
}

// addStrings collects every string inside a decoded value.
func addStrings(d *treeDoc, v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok {
				d.add(s, func(n string) { v[k] = n })
				continue
			}
			addStrings(d, e)
		}
	case []any:
		for i, e := range v {
			if s, ok := e.(string); ok {
				d.add(s, func(n string) { v[i] = n })
				continue
			}
			addStrings(d, e)
		}
	}
}

// toolErrorResult is a tool result that tells the model the call failed.
func toolErrorResult(text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"isError": true,
	})
	return raw
}

// writeRPC answers the client's POST with one message of the gateway's own.
func (x *mcpExchange) writeRPC(msg rpcMessage) {
	raw, _ := json.Marshal(msg)
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(http.StatusOK)
	_, _ = x.w.Write(raw)
}

// ------------------------------------------------------------ the way back

// relay writes the server's answer to the client, reading every message in it
// the gateway was waiting for.
func (x *mcpExchange) relay(resp *http.Response) {
	status := resp.StatusCode
	// The server refusing the gateway's credential is not the client's fault.
	// Passed on, it would read as a bad Keera key, and the client would start
	// signing in to the server itself.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		msg := "the MCP server '" + x.srv.Alias + "' refused the gateway's credential for it " +
			"(it answered " + http.StatusText(status) + "); an operator has to set it again"
		x.failPending(msg)
		httpx.WriteError(x.w, http.StatusBadGateway, "server_error", "mcp_credential_refused", msg)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		x.w.Header().Set("Content-Type", forwardableContentType(ct))
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		x.w.Header().Set("Mcp-Session-Id", id)
	}

	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		x.w.Header().Set("Cache-Control", "no-cache")
		x.w.Header().Set("X-Accel-Buffering", "no")
		x.w.WriteHeader(status)
		flusher := http.NewResponseController(x.w)
		_, _ = pipeNative(x.w, func() { _ = flusher.Flush() }, resp.Body, mcpEvents{x},
			x.s.opts.MaxResponseBytes)
	case strings.HasPrefix(ct, "application/json"):
		raw, err := io.ReadAll(io.LimitReader(resp.Body, x.s.opts.MaxResponseBytes))
		if err == nil {
			if out := x.serverMessage(raw); out != nil {
				raw = out
			}
		}
		x.w.WriteHeader(status)
		_, _ = x.w.Write(raw)
	default:
		x.w.WriteHeader(status)
		_, _ = io.Copy(x.w, io.LimitReader(resp.Body, x.s.opts.MaxResponseBytes))
	}
	x.failPending("the MCP server answered " + http.StatusText(status) + " without a result")
}

// unreachable answers a request the server could not be reached for.
func (x *mcpExchange) unreachable(err error) {
	if x.r.Context().Err() != nil {
		return
	}
	x.s.log.Error("MCP server unreachable", "server", x.srv.Alias, "error", err,
		"request_id", httpx.RequestID(x.r.Context()))
	msg := "the MCP server '" + x.srv.Alias + "' could not be reached"
	x.failPending(msg + ": " + err.Error())
	httpx.WriteError(x.w, http.StatusBadGateway, "server_error", "mcp_unavailable", msg)
}

// mcpEvents reads the server's event stream for pipeNative.
type mcpEvents struct{ x *mcpExchange }

func (e mcpEvents) event(payload []byte) []byte { return e.x.serverMessage(payload) }

func (e mcpEvents) counts() (*tokenUsage, int, bool) { return nil, 0, false }

// serverMessage reads one message, or a batch, from the server. It returns it
// rewritten, or nil to send it as it came.
func (x *mcpExchange) serverMessage(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (len(x.pending) == 0 && x.res.AllowedTools == nil) {
		return nil
	}
	if trimmed[0] == '[' {
		var msgs []json.RawMessage
		if json.Unmarshal(trimmed, &msgs) != nil {
			return nil
		}
		changed := false
		for i, m := range msgs {
			if out := x.serverMessage(m); out != nil {
				msgs[i], changed = out, true
			}
		}
		if !changed {
			return nil
		}
		out, _ := json.Marshal(msgs)
		return out
	}
	var msg rpcMessage
	if json.Unmarshal(trimmed, &msg) != nil || !msg.isResponse() {
		return nil
	}
	p, ok := x.pending[idKey(msg.ID)]
	if !ok {
		// An answer to a request made on another connection, as on a stream
		// the client resumed. Its tool call cannot be matched to a row, but a
		// tool list is still trimmed.
		p = &pendingRPC{method: "tools/list"}
	}
	delete(x.pending, idKey(msg.ID))
	switch p.method {
	case "tools/list":
		if list := x.allowedTools(msg.Result); list != nil {
			msg.Result = list
			out, _ := json.Marshal(msg)
			return out
		}
	case "tools/call":
		switch {
		case msg.Error != nil:
			x.record(p, store.ToolError, msg.Error.Message, 0)
		case toolFailed(msg.Result):
			x.record(p, store.ToolFailed, "", len(msg.Result))
		default:
			x.record(p, store.ToolOK, "", len(msg.Result))
		}
	}
	return nil
}

// allowedTools takes the tools the key may not call out of a tools/list
// result, and returns nil when there is nothing to take out.
func (x *mcpExchange) allowedTools(result json.RawMessage) json.RawMessage {
	if x.res.AllowedTools == nil || len(result) == 0 {
		return nil
	}
	var r map[string]json.RawMessage
	var tools []json.RawMessage
	if json.Unmarshal(result, &r) != nil || json.Unmarshal(r["tools"], &tools) != nil {
		return nil
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &named) == nil && x.res.AllowsTool(x.srv.Alias, named.Name) {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(tools) {
		return nil
	}
	r["tools"], _ = json.Marshal(kept)
	out, _ := json.Marshal(r)
	return out
}

// toolFailed reports whether a tool result says the tool itself failed.
func toolFailed(result json.RawMessage) bool {
	var r struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(result, &r) == nil && r.IsError
}

// failPending records every tool call still waiting as one that got no result.
func (x *mcpExchange) failPending(msg string) {
	for id, p := range x.pending {
		if p.method == "tools/call" {
			x.record(p, store.ToolError, msg, 0)
		}
		delete(x.pending, id)
	}
}

// record writes one tool call's row.
func (x *mcpExchange) record(p *pendingRPC, outcome store.ToolOutcome, errMsg string, resultBytes int) {
	took := time.Since(p.start)
	k := x.res.Key
	x.s.sink.Record(store.Event{
		TS: p.start, OrgID: k.OrgID, TeamID: k.TeamID, UserID: k.UserID, KeyID: k.ID,
		Client: x.client, SessionKey: x.session, Latency: took, Error: errMsg,
		FilterRuns: p.filters.runs, CostMicros: p.filters.micros, Scopes: x.res.Scopes,
		Tool: &store.ToolCall{
			Server: x.srv.Alias, Tool: p.tool, Outcome: outcome,
			ArgBytes: p.argBytes, ResultBytes: resultBytes,
		},
	})
	x.s.metrics.ToolCall(x.srv.Alias, k.OrgID, string(outcome), took.Seconds())
}
