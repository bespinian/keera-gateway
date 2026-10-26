package gateway

import (
	"bytes"
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

// The MCP proxy sits in front of the MCP servers an agent calls, since a tool
// call sends an agent's data somewhere other than a model. A client reaches a
// server through /api/mcp/{alias} with its Keera key, and the gateway
//
//   - lists only the tools the key may call, and refuses the others,
//   - runs the key's filters over a call's arguments,
//   - presents the server's credential, which never reaches a laptop,
//   - and records every call, but never its content.
//
// It speaks MCP's Streamable HTTP transport and passes everything else
// through.

// mcpClientHeaders are the client's headers the server is sent, with every
// Mcp-Param-* header. The rest stay behind, above all Authorization, which
// holds the Keera key.
var mcpClientHeaders = []string{
	"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Mcp-Method", "Mcp-Name",
	"Last-Event-Id",
}

// mcpParamPrefix starts the headers that copy a tool call's arguments, added
// in MCP 2026-07-28.
const mcpParamPrefix = "Mcp-Param-"

const (
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcHeaderMismatch = -32020
)

// partialMethods are the requests a key limited to some of a server's tools
// may send. Resources, prompts and completions read data outside any tool, and
// listing tasks would show other keys' tasks, since the server sees one
// identity. New MCP methods are refused until added here.
var partialMethods = []string{
	"initialize", "ping", "tools/list", "tools/call", "logging/setLevel",
	"tasks/get", "tasks/result", "tasks/cancel",
}

// hiddenCapabilities are hidden from a key limited to some tools, so the
// client does not ask for them.
var hiddenCapabilities = []string{"resources", "prompts", "completions"}

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

// idKey is a request id as a map key. The string id "1" and number id 1 stay
// different.
func idKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

// mcpExchange is one HTTP request to an MCP server.
type mcpExchange struct {
	s   *Server
	w   http.ResponseWriter
	r   *http.Request
	res *policy.Resolved
	srv policy.MCPServer
	// session is the hashed session the client named in a header, or empty.
	session string
	client  string
	// header is what the server is sent of the client's headers.
	header http.Header
	// pending is the requests whose answers the gateway acts on, by id.
	pending map[string]*pendingRPC
	// held is the credit the tool calls' filters hold, released once every
	// call is recorded.
	held int64
	// counted is the tool call counted against the key's daily or monthly
	// limits until the exchange ends. A POST carries one at most.
	counted *pendingRPC
}

type pendingRPC struct {
	method   string
	tool     string
	start    time.Time
	argBytes int
	filters  filterRun
	// admitted means the call counts against the key's daily or monthly limits.
	admitted bool
}

// mcpBegin authenticates the caller and finds the server. A server the key
// may not call any tool of is reported as missing.
func (s *Server) mcpBegin(w http.ResponseWriter, r *http.Request) (*mcpExchange, bool) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return nil, false
	}
	alias := r.PathValue("alias")
	srv, found := s.src.MCPServer(res.Key.OrgID, alias)
	// A subscription key, which sits in a settings file, reaches no MCP server,
	// since a server holds the organisation's credentials.
	if !found || !srv.Enabled || !res.AllowsServer(alias) || res.Key.Subscription() {
		httpx.WriteNotFound(w, "mcp_server_not_found",
			s.advise("the MCP server '"+alias+"' does not exist or this key may not use it"))
		return nil, false
	}
	if srv.Locked {
		httpx.WriteError(w, http.StatusForbidden, "permission_error", "org_limited",
			"the MCP server '"+alias+"' is in a private network; "+policy.LockedMessage(srv.OrgName))
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

func (s *Server) mcpPost(w http.ResponseWriter, r *http.Request) {
	x, ok := s.mcpBegin(w, r)
	if !ok {
		return
	}
	defer func() { s.budgets.ReleaseCredit(x.res.Key.OrgID, x.held) }()
	defer func() {
		if x.counted != nil {
			s.budgets.EndRequest(x.res.Quotas, x.counted.start)
		}
	}()
	raw, ok := readBody(w, r, s.opts.MaxBodyBytes, func(status int, code, msg string) {
		httpx.WriteError(w, status, "invalid_request_error", code, msg)
	})
	if !ok {
		return
	}
	if out, ok := x.inspect(raw); ok {
		x.forward(http.MethodPost, out)
	}
}

// mcpForward carries a GET, which opens the server's event stream, or a
// DELETE, which ends a session.
func (s *Server) mcpForward(w http.ResponseWriter, r *http.Request) {
	if x, ok := s.mcpBegin(w, r); ok {
		x.forward(r.Method, nil)
	}
}

// forward sends one request to the server with its credential, and relays the
// answer.
func (x *mcpExchange) forward(method string, payload []byte) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(x.r.Context(), method, x.srv.URL, body)
	if err != nil {
		x.unreachable(err)
		return
	}
	req.Header = x.header.Clone()
	if payload != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	setCredential(req.Header, x.srv.AuthHeader, x.srv.APIKey)
	resp, err := x.s.clientFor(x.srv.Limited).Do(req)
	if err != nil {
		x.unreachable(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	x.relay(resp)
}

// inspect reads the client's messages, answers the ones it refuses, and
// returns what to forward. forward is false when the answer was written.
//
// Messages are re-encoded as the gateway read them, so a server that reads
// JSON differently, such as repeated keys, gets exactly the call that was
// checked.
func (x *mcpExchange) inspect(raw []byte) (out []byte, forward bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return x.inspectBatch(trimmed)
	}
	var msg rpcMessage
	if json.Unmarshal(trimmed, &msg) != nil {
		x.rpcFail(http.StatusOK, nil, rpcParseError, "the body is not a JSON-RPC message")
		return nil, false
	}
	if x.refuseUnanswerable(msg) || !x.headersMatch(msg) || !x.mayRequest(msg) {
		return nil, false
	}
	if msg.isRequest() && msg.Method == "tools/call" {
		if !x.toolCall(&msg) {
			return nil, false
		}
	} else {
		x.await(msg)
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		x.rpcFail(http.StatusOK, msg.ID, rpcParseError, "the message could not be re-encoded")
		return nil, false
	}
	return encoded, true
}

// inspectBatch handles a batch, which MCP allowed before 2025-06-18. A tool
// call must come alone, so a refusal can be the whole answer.
func (x *mcpExchange) inspectBatch(raw []byte) ([]byte, bool) {
	var msgs []rpcMessage
	if json.Unmarshal(raw, &msgs) != nil {
		x.rpcFail(http.StatusOK, nil, rpcParseError, "the body is not a JSON-RPC batch")
		return nil, false
	}
	for _, m := range msgs {
		if x.refuseUnanswerable(m) || !x.mayRequest(m) {
			return nil, false
		}
		if m.isRequest() && m.Method == "tools/call" {
			x.rpcFail(http.StatusOK, m.ID, rpcInvalidParams, "the gateway takes a tool call only "+
				"on its own, not in a batch; send it as a request of its own")
			return nil, false
		}
		x.await(m)
	}
	encoded, err := json.Marshal(msgs)
	if err != nil {
		x.rpcFail(http.StatusOK, nil, rpcParseError, "the batch could not be re-encoded")
		return nil, false
	}
	return encoded, true
}

// await notes a request whose answer the gateway reads.
func (x *mcpExchange) await(m rpcMessage) {
	if m.isRequest() && (m.Method == "tools/list" || m.Method == "initialize") {
		x.pending[idKey(m.ID)] = &pendingRPC{method: m.Method, start: time.Now()}
	}
}

// mayRequest refuses what a key limited to some tools may not send. It
// reports whether the message may go on.
func (x *mcpExchange) mayRequest(m rpcMessage) bool {
	if !m.isRequest() || x.res.AllowsWholeServer(x.srv.Alias) ||
		slices.Contains(partialMethods, m.Method) {
		return true
	}
	x.rpcFail(http.StatusOK, m.ID, rpcMethodNotFound, x.s.advise("Method not found: "+m.Method+
		". This key may call only some of the tools of '"+x.srv.Alias+
		"', so the gateway does not forward "+m.Method+" to it"))
	return false
}

// refuseUnanswerable refuses a tool call without an id, which would get no
// answer to record. It reports whether it answered.
func (x *mcpExchange) refuseUnanswerable(m rpcMessage) bool {
	if m.Method != "tools/call" || len(m.ID) > 0 {
		return false
	}
	x.rpcFail(http.StatusOK, nil, rpcInvalidParams, "a tool call needs an id; the gateway "+
		"does not forward one sent as a notification")
	return true
}

// headersMatch checks the Mcp-Method and Mcp-Name headers against the body, as
// MCP asks, since a load balancer behind the gateway may route on them. Older
// clients send neither. On a mismatch it answers and reports false.
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
	x.rpcFail(http.StatusBadRequest, msg.ID, rpcHeaderMismatch, "Header mismatch: "+why)
	return false
}

// mcpName is what Mcp-Name should carry: a tool or prompt name, a resource
// URI, or nothing.
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

// The markers around a header value MCP sends in Base64 because it is not
// plain ASCII.
const (
	base64Open  = "=?base64?"
	base64Close = "?="
)

func inBase64(v string) bool {
	return len(v) >= len(base64Open)+len(base64Close) &&
		strings.HasPrefix(v, base64Open) && strings.HasSuffix(v, base64Close)
}

func decodeHeaderValue(v string) (string, bool) {
	if !inBase64(v) {
		return v, true
	}
	raw, err := base64.StdEncoding.DecodeString(v[len(base64Open) : len(v)-len(base64Close)])
	return string(raw), err == nil
}

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

// syncParams updates the Mcp-Param-* headers after a filter rewrote the
// arguments, so a value removed from the body does not leave in a header. The
// gateway lacks the tool's schema, so it finds the argument by its old value.
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

// stringPaths finds where a string is in decoded arguments. A header copies
// only values reached through object keys, never arrays.
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

// toolCall checks whether the key may make a tool call and runs the filters
// over its arguments. It returns false when the call was answered here.
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
		x.rpcFail(http.StatusOK, msg.ID, rpcInvalidParams, "the tool call could not be read: "+err.Error())
		return false
	}
	args, _ := params.value("arguments")
	p.argBytes = len(args)

	// The tool is hidden from the key's list, so answer as for an unknown tool.
	if !x.res.AllowsTool(x.srv.Alias, p.tool) {
		why := "Unknown tool: " + p.tool + ". This key may not call '" + x.srv.Alias + "/" +
			p.tool + "' through the gateway"
		x.rpcFail(http.StatusOK, msg.ID, rpcInvalidParams, x.s.advise(why))
		x.record(p, store.ToolDenied, why, 0)
		return false
	}

	held, why := x.admit(len(args))
	x.held += held
	if why != "" {
		x.toolFailed(p, msg.ID, store.ToolDenied, why)
		return false
	}
	p.admitted = true
	x.s.budgets.StartRequest(x.res.Quotas, p.start)
	x.counted = p

	if len(x.res.Filters) > 0 {
		run, ref := x.s.applyFilters(x.r.Context(), x.res.Key.OrgID, x.res.Filters,
			guardrailFilters, params, argumentsText, nil)
		p.filters = run
		if run.micros > 0 {
			x.s.budgets.Charge(x.res.Scopes, run.micros, time.Now())
		}
		if ref != nil {
			// The refusal is the tool's answer, so the model can carry on.
			outcome := store.ToolRefused
			if ref.code != codeFilterRefused {
				outcome = store.ToolNoResult
			}
			x.toolFailed(p, msg.ID, outcome, ref.msg)
			return false
		}
		if x.r.Context().Err() != nil {
			// Recorded anyway, or what the filters spent would be lost.
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
	// See inspect.
	msg.Params = params.encode()
	x.pending[idKey(msg.ID)] = p
	return true
}

// argumentsText is what filters read in a tool call: every string value in
// its arguments.
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

// addStrings collects every string in a decoded value. Keys are sorted, so a
// filter sees the same order each time.
func addStrings(d *treeDoc, v any) {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			e := v[k]
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

// admit holds a tool call to the key's rate limits. A call costs nothing
// unless filters read it, so only then does a budget apply. It returns why a
// call is refused, or "".
func (x *mcpExchange) admit(argBytes int) (held int64, why string) {
	now := time.Now()
	if ref, ok := x.s.checkRates(x.res.Scopes, now); !ok {
		return 0, ref.msg
	}
	if ref, ok := x.s.checkQuotas(x.res.Quotas, now); !ok {
		return 0, ref.msg
	}
	if len(x.res.Filters) == 0 {
		return 0, ""
	}
	if err := x.s.budgets.Allow(x.res.Scopes, now); err != nil {
		return 0, err.Error()
	}
	if !x.s.filtersOnKey(x.res.Key.OrgID, x.res.Filters) {
		return 0, ""
	}
	held, err := x.s.budgets.HoldCredit(x.res.Key.OrgID,
		x.s.filtersHold(x.res.Key.OrgID, x.res.Filters, argBytes/bytesPerToken))
	if err != nil {
		return 0, err.Error()
	}
	return held, ""
}

// toolFailed answers a tool call with an error result and records it.
func (x *mcpExchange) toolFailed(p *pendingRPC, id json.RawMessage, outcome store.ToolOutcome, why string) {
	result, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": x.s.advise(why)}},
		"isError": true,
	})
	httpx.WriteJSON(x.w, http.StatusOK, rpcMessage{JSONRPC: "2.0", ID: id, Result: result})
	x.record(p, outcome, why, 0)
}

// rpcFail answers with a JSON-RPC error. A message without an id gets a null
// one, as JSON-RPC asks.
func (x *mcpExchange) rpcFail(status int, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	httpx.WriteJSON(x.w, status, rpcMessage{
		JSONRPC: "2.0", ID: id,
		Error: &rpcError{Code: code, Message: msg},
	})
}

// relay writes the server's answer to the client, reading the messages the
// gateway waits for.
func (x *mcpExchange) relay(resp *http.Response) {
	status := resp.StatusCode
	// Passed on, the server refusing the gateway's credential would read as a
	// bad Keera key, and the client would try to sign in to the server itself.
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

	switch httpx.MediaType(ct) {
	case "text/event-stream":
		streamHeaders(x.w.Header())
		x.w.WriteHeader(status)
		flusher := http.NewResponseController(x.w)
		_, _ = pipeNative(x.w, func() { _ = flusher.Flush() }, resp.Body, mcpEvents{x},
			x.s.opts.MaxResponseBytes)
	case "application/json":
		raw, err := readCapped(resp.Body, x.s.opts.MaxResponseBytes)
		if err != nil {
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
		// Not MCP, so only the status is passed on.
		x.w.Header().Del("Content-Type")
		x.w.WriteHeader(status)
	}
	x.failPending("the MCP server answered " + http.StatusText(status) + " without a result")
}

func (x *mcpExchange) unreachable(err error) {
	if x.r.Context().Err() != nil {
		// Recorded anyway, or what the filters spent would be lost.
		x.failPending("the client hung up before the MCP server answered")
		return
	}
	x.s.log.Error("MCP server unreachable", "server", x.srv.Alias, "error", err,
		httpx.LogIDs(x.r.Context()))
	msg := "the MCP server '" + x.srv.Alias + "' could not be reached"
	x.failPending(msg + ": " + err.Error())
	httpx.WriteError(x.w, http.StatusBadGateway, "server_error", "mcp_unavailable", msg)
}

type mcpEvents struct{ x *mcpExchange }

func (e mcpEvents) event(payload []byte) []byte { return e.x.serverMessage(payload) }

func (e mcpEvents) counts() (*tokenUsage, int, bool) { return nil, 0, false }

// failure is nil, since no MCP event ends a stream.
func (e mcpEvents) failure() error { return nil }

// serverMessage reads a message or batch from the server. It returns it
// rewritten, or nil to send it unchanged.
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
		// An answer to a request from another connection, as on a resumed
		// stream. A tool list is still trimmed.
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

// capabilities removes what the key may not use from an initialize result,
// or returns nil if nothing changes. See partialMethods.
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

// publicScope marks a list any caller may be served from a cache.
var publicScope = json.RawMessage(`"public"`)

// toolList removes the tools the key may not call from a tools/list result,
// or returns nil if nothing changes. A public list is marked private, since
// it depends on the key and a shared cache would serve it to other keys.
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

// allowedTools removes the tools the key may not call, or returns nil if
// there are none.
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

// toolOutcome reads a tool result's outcome. Since MCP 2026-07-28, a server
// may ask for user input; the client's second call is a row of its own.
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

// failPending records every waiting tool call as having no result.
func (x *mcpExchange) failPending(msg string) {
	for id, p := range x.pending {
		if p.method == "tools/call" {
			x.record(p, store.ToolNoResult, msg, 0)
		}
		delete(x.pending, id)
	}
}

func (x *mcpExchange) record(p *pendingRPC, outcome store.ToolOutcome, errMsg string, resultBytes int) {
	took := time.Since(p.start)
	k := x.res.Key
	scopes := x.res.Scopes
	if p.admitted {
		scopes = append(slices.Clip(scopes), x.res.Quotas...)
	}
	x.s.record(store.Event{
		TS: p.start, OrgID: k.OrgID, ProjectID: k.ProjectID, UserID: k.UserID, KeyID: k.ID,
		Client: x.client, SessionKey: x.session, Latency: took, Error: errMsg,
		FilterRuns: p.filters.runs, CostMicros: p.filters.micros, Bills: p.filters.bills, Scopes: scopes,
		Tool: &store.ToolCall{
			Server: x.srv.Alias, Tool: p.tool, Outcome: outcome,
			ArgBytes: p.argBytes, ResultBytes: resultBytes,
		},
	})
	x.s.metrics.ToolCall(x.srv.Alias, k.OrgID, string(outcome), took.Seconds())
}
