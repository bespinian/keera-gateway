package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
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

// mcpClientHeaders are the client's headers the server is sent, with every
// Mcp-Param-* header. Everything else stays behind, above all the client's
// Authorization, which is its Keera key.
var mcpClientHeaders = []string{
	"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Mcp-Method", "Mcp-Name",
	"Last-Event-Id",
}

// mcpParamPrefix starts the headers that copy a tool call's arguments, which
// MCP added in its 2026-07-28 version.
const mcpParamPrefix = "Mcp-Param-"

// JSON-RPC error codes the proxy answers with itself.
const (
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcHeaderMismatch = -32020
)

// partialMethods are the requests a key that may call only some of a server's
// tools may send it. Resources, prompts and completions read the server's data
// outside any tool. Listing tasks would show other keys' tasks, because the
// server sees one identity for all of them. A method MCP adds later is refused
// until it is added here.
var partialMethods = []string{
	"initialize", "ping", "tools/list", "tools/call", "logging/setLevel",
	"tasks/get", "tasks/result", "tasks/cancel",
}

// hiddenCapabilities are what a server is not shown to offer a key that may
// call only some of its tools, so the client does not ask for them.
var hiddenCapabilities = []string{"resources", "prompts", "completions"}

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
	// header is what the server is sent of the client's headers.
	header http.Header
	// pending is every request the answer will carry that the gateway acts on,
	// by id.
	pending map[string]*pendingRPC
	// held is the credit its tool calls' filters hold. It is given back once
	// every call has been recorded, which charges what they spent.
	held int64
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
	srv, found := s.src.MCPServer(res.Key.OrgID, alias)
	// A subscription key reaches no MCP server: a server holds the
	// organisation's credentials, and the key sits in a settings file.
	if !found || !srv.Enabled || !res.AllowsServer(alias) || res.Key.Subscription() {
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "mcp_server_not_found",
			s.advise("the MCP server '"+alias+"' does not exist or this key may not use it"))
		return nil, false
	}
	if srv.Locked {
		httpx.WriteError(w, http.StatusForbidden, "permission_error", "org_limited",
			"the MCP server '"+alias+"' is inside this deployment's network, and "+policy.LockedMessage)
		return nil, false
	}
	x := &mcpExchange{
		s: s, w: w, r: r, res: res, srv: srv,
		client:  connect.Identify(r.Header.Get(connect.ClientHeader), r.UserAgent()),
		header:  http.Header{},
		pending: map[string]*pendingRPC{},
	}
	for _, h := range mcpClientHeaders {
		if v := r.Header.Get(h); v != "" {
			x.header.Set(h, v)
		}
	}
	for h, v := range r.Header {
		if strings.HasPrefix(h, mcpParamPrefix) {
			x.header[h] = v
		}
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
	defer func() { s.budgets.ReleaseCredit(x.res.Key.OrgID, x.held) }()
	raw, ok := readBody(w, r, s.opts.MaxBodyBytes, func(msg string) {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			"request_too_large", msg)
	})
	if !ok {
		return
	}
	out, forward := x.inspect(raw)
	if !forward {
		return
	}
	resp, err := s.mcpSend(r.Context(), x.srv, http.MethodPost, out, x.header)
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
	resp, err := s.mcpSend(r.Context(), x.srv, r.Method, nil, x.header)
	if err != nil {
		x.unreachable(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	x.relay(resp)
}

// mcpSend sends one request to the server, with the client's headers it may
// see and the server's credential.
func (s *Server) mcpSend(ctx context.Context, srv policy.MCPServer, method string,
	payload []byte, header http.Header,
) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, srv.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	if payload != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	setCredential(req.Header, srv.AuthHeader, srv.APIKey)
	return s.clientFor(srv.Limited).Do(req)
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
	if refuseUnanswerable(x, msg) || !x.headersMatch(msg) {
		return nil, false
	}
	if msg.isRequest() {
		if !x.mayRequest(msg) {
			return nil, false
		}
		switch msg.Method {
		case "tools/call":
			if !x.toolCall(&msg) {
				return nil, false
			}
		case "tools/list", "initialize":
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
		if m.isRequest() && !x.mayRequest(m) {
			return nil, false
		}
		switch {
		case m.isRequest() && m.Method == "tools/call":
			x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{
				Code: rpcInvalidParams, Message: "the gateway takes a tool call only on its own, " +
					"not in a batch; send it as a request of its own",
			}})
			return nil, false
		case m.isRequest() && (m.Method == "tools/list" || m.Method == "initialize"):
			x.pending[idKey(m.ID)] = &pendingRPC{method: m.Method, start: time.Now()}
		}
	}
	encoded, err := json.Marshal(msgs)
	if err != nil {
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: rpcParseError, Message: "the batch could not be re-encoded"}})
		return nil, false
	}
	return encoded, true
}

// mayRequest refuses a request a key that may call only some of the server's
// tools may not send. It reports whether the request may go on.
func (x *mcpExchange) mayRequest(m rpcMessage) bool {
	if x.res.AllowsWholeServer(x.srv.Alias) || slices.Contains(partialMethods, m.Method) {
		return true
	}
	x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{
		Code: rpcMethodNotFound, Message: x.s.advise("Method not found: " + m.Method +
			". This key may call only some of the tools of '" + x.srv.Alias +
			"', so the gateway does not forward " + m.Method + " to it"),
	}})
	return false
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

// headersMatch checks the Mcp-Method and Mcp-Name headers against the message,
// as MCP asks of anything that reads the body. The gateway decides on the
// body, but a load balancer behind it may route on the headers. A client of a
// version before 2026-07-28 sends neither. It reports whether they match, and
// answers when they do not.
func (x *mcpExchange) headersMatch(msg rpcMessage) bool {
	why := ""
	if m := x.r.Header.Get("Mcp-Method"); m != "" && m != msg.Method {
		why = "the Mcp-Method header '" + m + "' is not the message's method '" + msg.Method + "'"
	} else if n := x.r.Header.Get("Mcp-Name"); n != "" {
		if got, ok := decodeHeaderValue(n); !ok || got != mcpName(msg) {
			why = "the Mcp-Name header '" + n + "' is not the name or URI in the message"
		}
	}
	if why == "" {
		return true
	}
	id := msg.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	x.writeRPCStatus(http.StatusBadRequest, rpcMessage{JSONRPC: "2.0", ID: id,
		Error: &rpcError{Code: rpcHeaderMismatch, Message: "Header mismatch: " + why}})
	return false
}

// mcpName is what the Mcp-Name header carries for a message: the name of the
// tool or prompt, the URI of the resource, or nothing.
func mcpName(msg rpcMessage) string {
	var p struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	switch msg.Method {
	case "tools/call", "prompts/get":
		return p.Name
	case "resources/read":
		return p.URI
	}
	return ""
}

// The markers around a header value MCP carries in Base64, because it is not
// plain ASCII.
const (
	base64Open  = "=?base64?"
	base64Close = "?="
)

func inBase64(v string) bool {
	return len(v) >= len(base64Open)+len(base64Close) &&
		strings.HasPrefix(v, base64Open) && strings.HasSuffix(v, base64Close)
}

// decodeHeaderValue reads an Mcp-Name or Mcp-Param-* value.
func decodeHeaderValue(v string) (string, bool) {
	if !inBase64(v) {
		return v, true
	}
	raw, err := base64.StdEncoding.DecodeString(v[len(base64Open) : len(v)-len(base64Close)])
	return string(raw), err == nil
}

// encodeHeaderValue writes a value as MCP puts it in a header.
func encodeHeaderValue(v string) string {
	plain := v != "" && v == strings.TrimSpace(v) && !inBase64(v)
	for i := 0; plain && i < len(v); i++ {
		plain = v[i] >= 0x20 && v[i] <= 0x7e
	}
	if plain {
		return v
	}
	return base64Open + base64.StdEncoding.EncodeToString([]byte(v)) + base64Close
}

// syncParams keeps the Mcp-Param-* headers true to arguments a filter
// rewrote, so that a value taken out of the body does not leave in a header.
// Which argument a header copies is in the tool's schema, which the gateway
// does not hold, so it finds the argument by the value the client sent. A
// value still in the arguments where it was is left as it is.
func (x *mcpExchange) syncParams(before, after json.RawMessage) {
	var old, cur any
	if json.Unmarshal(before, &old) != nil || json.Unmarshal(after, &cur) != nil {
		return
	}
	for h, vs := range x.header {
		if !strings.HasPrefix(h, mcpParamPrefix) || len(vs) == 0 {
			continue
		}
		v, ok := decodeHeaderValue(vs[0])
		if !ok {
			continue
		}
		paths := stringPaths(old, v, nil, nil)
		if len(paths) == 0 || slices.ContainsFunc(paths, func(p []string) bool {
			s, _ := valueAt(cur, p).(string)
			return s == v
		}) {
			continue
		}
		if s, ok := valueAt(cur, paths[0]).(string); ok {
			x.header.Set(h, encodeHeaderValue(s))
		} else {
			x.header.Del(h)
		}
	}
}

// stringPaths finds where a string is in decoded arguments, first path first.
// A header copies only a value reached through object keys, never through an
// array.
func stringPaths(v any, want string, at []string, found [][]string) [][]string {
	switch v := v.(type) {
	case string:
		if v == want {
			found = append(found, slices.Clone(at))
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			found = stringPaths(v[k], want, append(at, k), found)
		}
	}
	return found
}

// valueAt is the value at a path of object keys, or nil.
func valueAt(v any, path []string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
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

	held, ref, ok := x.admit(len(args))
	x.held += held
	if !ok {
		x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: toolErrorResult(x.s.advise(ref.msg))})
		x.record(p, store.ToolDenied, ref.msg, 0)
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
				outcome = store.ToolNoResult
			}
			x.writeRPC(rpcMessage{JSONRPC: "2.0", ID: msg.ID, Result: toolErrorResult(x.s.advise(ref.msg))})
			x.record(p, outcome, ref.msg, 0)
			return false
		}
		if x.r.Context().Err() != nil {
			// Recorded all the same, or what the filters spent would be lost.
			x.record(p, store.ToolNoResult, "the client hung up while a filter was running", 0)
			return false
		}
		if run.rewrote {
			before := args
			args, _ = params.value("arguments")
			p.argBytes = len(args)
			x.syncParams(before, args)
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

// admit holds a tool call to the key's rate limits, as a model request is. A
// call costs nothing unless filters read it, so only then does a budget stop
// it. Filters on the deployment's key hold the most they may cost, as a
// request's do. The refusal is a failed tool result, so the model reads why.
func (x *mcpExchange) admit(argBytes int) (int64, refusal, bool) {
	now := time.Now()
	if ref, ok := x.s.checkRates(x.res, now); !ok {
		return 0, ref, false
	}
	if len(x.res.Filters) == 0 {
		return 0, refusal{}, true
	}
	if err := x.s.budgets.Allow(x.res.Scopes, now); err != nil {
		return 0, refusal{msg: err.Error()}, false
	}
	if !x.s.filtersOnKey(x.res.Key.OrgID, x.res.Filters) {
		return 0, refusal{}, true
	}
	held, err := x.s.budgets.HoldCredit(x.res.Key.OrgID,
		x.s.filtersHold(x.res.Key.OrgID, x.res.Filters, argBytes/bytesPerToken))
	if err != nil {
		return 0, refusal{msg: err.Error()}, false
	}
	return held, refusal{}, true
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
func (x *mcpExchange) writeRPC(msg rpcMessage) { x.writeRPCStatus(http.StatusOK, msg) }

func (x *mcpExchange) writeRPCStatus(status int, msg rpcMessage) {
	httpx.WriteJSON(x.w, status, msg)
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
			"(it answered " + http.StatusText(status) + "); an administrator has to " +
			"set it again"
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
		streamHeaders(x.w.Header())
		x.w.WriteHeader(status)
		flusher := http.NewResponseController(x.w)
		_, _ = pipeNative(x.w, func() { _ = flusher.Flush() }, resp.Body, mcpEvents{x},
			x.s.opts.MaxResponseBytes)
	case strings.HasPrefix(ct, "application/json"):
		raw, err := readCapped(resp.Body, x.s.opts.MaxResponseBytes)
		if err != nil {
			// Cut JSON would reach the client as the server's own answer.
			msg := "the answer from the MCP server '" + x.srv.Alias + "' could not be read"
			x.failPending(msg + ": " + err.Error())
			httpx.WriteError(x.w, http.StatusBadGateway, "server_error", "mcp_unavailable", msg)
			return
		}
		if out := x.serverMessage(raw); out != nil {
			raw = out
		}
		x.w.WriteHeader(status)
		_, _ = x.w.Write(raw)
	default:
		// Anything else is not MCP, so only its status is passed on. The body
		// could be any page the server's address reaches.
		x.w.Header().Del("Content-Type")
		x.w.WriteHeader(status)
	}
	x.failPending("the MCP server answered " + http.StatusText(status) + " without a result")
}

// unreachable answers a request the server could not be reached for.
func (x *mcpExchange) unreachable(err error) {
	if x.r.Context().Err() != nil {
		// Recorded all the same, or what the filters spent would be lost.
		x.failPending("the client hung up before the MCP server answered")
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
	case "initialize":
		if result := x.capabilities(msg.Result); result != nil {
			msg.Result = result
			out, _ := json.Marshal(msg)
			return out
		}
	case "tools/list":
		if list := x.toolList(msg.Result); list != nil {
			msg.Result = list
			out, _ := json.Marshal(msg)
			return out
		}
	case "tools/call":
		if msg.Error != nil {
			x.record(p, store.ToolNoResult, msg.Error.Message, 0)
		} else {
			x.record(p, toolOutcome(msg.Result), "", len(msg.Result))
		}
	}
	return nil
}

// capabilities takes what the key may not use out of an initialize result,
// and returns nil when it needs no change. See partialMethods.
func (x *mcpExchange) capabilities(result json.RawMessage) json.RawMessage {
	if x.res.AllowsWholeServer(x.srv.Alias) {
		return nil
	}
	var r map[string]json.RawMessage
	var caps map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil || json.Unmarshal(r["capabilities"], &caps) != nil {
		return nil
	}
	n := len(caps)
	for _, c := range hiddenCapabilities {
		delete(caps, c)
	}
	if len(caps) == n {
		return nil
	}
	r["capabilities"], _ = json.Marshal(caps)
	out, _ := json.Marshal(r)
	return out
}

// publicScope is how a server marks a list any caller may be served from a
// cache.
var publicScope = json.RawMessage(`"public"`)

// toolList takes the tools the key may not call out of a tools/list result,
// and returns nil when it needs no change.
//
// What the gateway lists depends on the key, so a list the server marks
// public is marked private. A shared cache in front of the gateway would
// otherwise serve one key's list to another.
func (x *mcpExchange) toolList(result json.RawMessage) json.RawMessage {
	var r map[string]json.RawMessage
	if len(result) == 0 || json.Unmarshal(result, &r) != nil {
		return nil
	}
	changed := false
	if bytes.Equal(bytes.TrimSpace(r["cacheScope"]), publicScope) {
		r["cacheScope"], changed = json.RawMessage(`"private"`), true
	}
	if kept := x.allowedTools(r["tools"]); kept != nil {
		r["tools"], changed = kept, true
	}
	if !changed {
		return nil
	}
	out, _ := json.Marshal(r)
	return out
}

// allowedTools takes the tools the key may not call out of a list of tools,
// and returns nil when there is nothing to take out.
func (x *mcpExchange) allowedTools(list json.RawMessage) json.RawMessage {
	var tools []json.RawMessage
	if x.res.AllowedTools == nil || json.Unmarshal(list, &tools) != nil {
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
	out, _ := json.Marshal(kept)
	return out
}

// toolOutcome reads what a tool result says came of the call. Since MCP's
// 2026-07-28 version a server that needs the user's input answers with a
// request for it, and the client sends the call again with the answers. That
// second call is a row of its own.
func toolOutcome(result json.RawMessage) store.ToolOutcome {
	var r struct {
		IsError    bool   `json:"isError"`
		ResultType string `json:"resultType"`
	}
	_ = json.Unmarshal(result, &r)
	switch {
	case r.ResultType == "input_required":
		return store.ToolInputRequired
	case r.IsError:
		return store.ToolFailed
	}
	return store.ToolOK
}

// failPending records every tool call still waiting as one that got no result.
func (x *mcpExchange) failPending(msg string) {
	for id, p := range x.pending {
		if p.method == "tools/call" {
			x.record(p, store.ToolNoResult, msg, 0)
		}
		delete(x.pending, id)
	}
}

// record writes one tool call's row.
func (x *mcpExchange) record(p *pendingRPC, outcome store.ToolOutcome, errMsg string, resultBytes int) {
	took := time.Since(p.start)
	k := x.res.Key
	x.s.record(store.Event{
		TS: p.start, OrgID: k.OrgID, ProjectID: k.ProjectID, UserID: k.UserID, KeyID: k.ID,
		Client: x.client, SessionKey: x.session, Latency: took, Error: errMsg,
		FilterRuns: p.filters.runs, CostMicros: p.filters.micros, Bills: p.filters.bills, Scopes: x.res.Scopes,
		Tool: &store.ToolCall{
			Server: x.srv.Alias, Tool: p.tool, Outcome: outcome,
			ArgBytes: p.argBytes, ResultBytes: resultBytes,
		},
	})
	x.s.metrics.ToolCall(x.srv.Alias, k.OrgID, string(outcome), took.Seconds())
}
