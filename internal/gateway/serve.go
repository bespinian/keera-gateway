package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/store"
)

// maxAliasBytes bounds the 'model' field a client may send. Real aliases are
// short; the limit exists because the value is free text that ends up in a
// usage row and in a refusal message.
const maxAliasBytes = 128

// call is one inference request on its way through serve.
type call struct {
	w    http.ResponseWriter
	r    *http.Request
	res  *policy.Resolved
	surf surface
	tr   *trace
	// ev is the request's usage row, filled in as the request goes. It exists
	// from the start, because a refused request is still recorded.
	ev store.Event

	// work is when the gateway's own work began, for the overhead metric.
	work time.Time
	// now is when the limits were checked. Budgets are charged against it.
	now time.Time

	// alias and model are the model that will answer. On a routed request they
	// change twice: to the router's choice, and to the destination that answered.
	alias  string
	model  policy.Model
	router policy.Router
	routed bool

	decision routeDecision
	filters  filterRun
	// chain is the models to try, in order: the one the client or a router
	// chose, or a trying router's list.
	chain []policy.Model

	// native is the body in the client's own API, for the destinations that
	// are sent that API rather than a translation (see native.go). It is nil
	// when none are. translated says some destinations get the translation.
	// Until then, or when none do, the OpenAI-shaped body is nil.
	native     *body
	translated bool

	stream        bool
	injectedUsage bool
}

// hookMicros is what the request's router and filters spent on their own
// models.
func (c *call) hookMicros() int64 {
	return c.filters.micros + c.decision.micros
}

// serve is everything after authentication: enforce, forward, account.
//
// Each step returns false when the request is over, either because it has
// been refused or because the client has gone.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, res *policy.Resolved,
	surf surface, tr *trace,
) {
	c := &call{w: w, r: r, res: res, surf: surf, tr: tr, ev: store.Event{
		TS: tr.start, OrgID: res.Key.OrgID, TeamID: res.Key.TeamID, UserID: res.Key.UserID,
		KeyID: res.Key.ID, Scopes: res.Scopes,
		// Read now, before the headers are replaced by the ones the backend gets.
		Client: connect.Identify(r.Header.Get(connect.ClientHeader), r.UserAgent()),
	}}

	raw, ok := s.receive(c)
	if !ok {
		return
	}
	// The gateway's own work starts here. Everything before is the client
	// sending its body, and a slow link should not count as gateway overhead.
	c.work = time.Now()
	tr.seal("")

	// All of the gateway's own checks are one step: together they take a
	// fraction of a millisecond, and one bar says that more clearly than five.
	tr.open(store.SpanAdmit, "")
	b, ok := s.decode(c, raw)
	if !ok || !s.target(c, b) || !s.admit(c) {
		return
	}
	tr.seal("")

	// The router runs before the filters, so it reads what the client sent. A
	// router after a redaction filter could send a cleaned-up prompt outside
	// the cluster, so its own model has to be local, like a filter's.
	if c.routed {
		if b, ok = s.translate(c, b); !ok || !s.useRouter(c, b) {
			return
		}
	}
	c.chain = s.destinations(c.res.Key.OrgID, c.model, c.decision)
	if b, ok = s.useNative(c, b); !ok {
		return
	}
	if !s.fits(c, b) || !s.useFilters(c, b) || !s.prepare(c, b) {
		return
	}
	s.answer(c, b)
}

// receive reads the request body off the client.
func (s *Server) receive(c *call) ([]byte, bool) {
	// Opened rather than timed, so a body refused halfway (over the size cap)
	// is still drawn as the step it was refused in.
	c.tr.open(store.SpanReceive, "")
	raw, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, s.opts.MaxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			s.refuse(c, refusal{
				status: http.StatusRequestEntityTooLarge,
				typ:    "invalid_request_error", code: "request_too_large",
				msg: "the request body exceeds the gateway's limit",
			})
		}
		return nil, false // otherwise the client went away mid-upload
	}
	return raw, true
}

// decode parses the body. Everything after this is written against the OpenAI
// shape, except what is sent to a provider in the client's own API. A body in
// such an API is kept as it came, in c.native, and is translated only once
// something needs the OpenAI shape (see translate). Until then b is nil.
func (s *Server) decode(c *call, raw []byte) (*body, bool) {
	d := c.surf.dialect
	if d == nil {
		var err error
		if raw, err = c.surf.shape.decode(raw); err != nil {
			s.refuse(c, invalidBody(err.Error()))
			return nil, false
		}
	}
	b, err := parseBody(raw)
	if err != nil {
		s.refuse(c, invalidBody(err.Error()))
		return nil, false
	}
	// Set before anything can refuse, so a session cut short by a budget
	// shows the refusal too.
	if d == nil {
		c.ev.SessionKey = sessionKey(c.r, c.res.Key.ID, c.surf.kind, b.firstUserMessage)
		return b, true
	}
	c.native = b
	c.ev.SessionKey = sessionKey(c.r, c.res.Key.ID, c.surf.kind,
		func() (json.RawMessage, bool) { return d.opening(b) })
	return nil, true
}

// translate makes the OpenAI-shaped body from the client's own, the first
// time a router or a destination that is not the client's provider needs it.
// A body that is in the OpenAI shape already is returned as it is.
func (s *Server) translate(c *call, b *body) (*body, bool) {
	if b != nil {
		return b, true
	}
	raw, err := c.surf.shape.decode(c.native.encode())
	if err == nil {
		if b, err = parseBody(raw); err == nil {
			return b, true
		}
	}
	s.refuse(c, invalidBody(err.Error()))
	return nil, false
}

// invalidBody is the refusal for a body the gateway cannot use.
func invalidBody(msg string) refusal {
	return refusal{
		status: http.StatusBadRequest,
		typ:    "invalid_request_error", code: "invalid_body", msg: msg,
	}
}

// unreadableBody refuses a request a router or a filter had to read and could
// not. who says what needed to read it, and why.
func unreadableBody(who string, err error) *refusal {
	r := invalidBody(who + ", and this request could not be read: " + err.Error())
	r.advise = true
	return &r
}

// noDestination refuses a request none of a router's destinations can serve.
func noDestination(msg string) *refusal {
	return &refusal{
		status: http.StatusServiceUnavailable,
		typ:    "server_error", code: "router_destination_unavailable",
		msg: msg + ". Nothing was sent to a model", advise: true,
	}
}

// notRebuilt refuses a request whose filtered text could not be put back.
func notRebuilt(err error) *refusal {
	return &refusal{
		status: http.StatusBadGateway,
		typ:    "server_error", code: "filter_failed",
		msg: "the filtered request could not be rebuilt, so nothing was sent " +
			"to the model: " + err.Error(),
		advise: true,
	}
}

// target works out what the 'model' field names: one of this organisation's
// models, or one of its routers.
func (s *Server) target(c *call, b *body) bool {
	// Before the translation, the model is read from the client's own API,
	// which names it in the same field.
	sent := b
	if sent == nil {
		sent = c.native
	}
	alias, hasAlias := sent.str("model")
	if !hasAlias || alias == "" {
		s.refuse(c, refusal{
			status: http.StatusBadRequest,
			typ:    "invalid_request_error", code: "missing_model",
			msg: "the 'model' field is required; send one of the models from /v1/models",
		})
		return false
	}
	// An oversized name is refused without being quoted back or stored:
	// otherwise a client could put a request-sized string on every usage row.
	if len(alias) > maxAliasBytes {
		s.refuse(c, refusal{
			status: http.StatusNotFound,
			typ:    "invalid_request_error", code: "model_not_found",
			msg: fmt.Sprintf("the 'model' field is %d bytes long; no model is, so this "+
				"names none - send one of the models from /v1/models", len(alias)),
		})
		return false
	}
	c.ev.Alias = alias
	c.alias = alias

	// A model is looked up first. The control plane refuses a router named
	// like a model, and a model named like a router, so the two rarely meet.
	model, found := s.src.Model(c.res.Key.OrgID, alias)
	if !found {
		c.router, c.routed = s.src.Router(c.res.Key.OrgID, alias)
		// Routers exist only on chat. The allow-list covers the router, not
		// its destinations: allowing a router allows where it sends. A
		// subscription key reaches no router, which would place it on models
		// the organisation pays for.
		if c.routed && (c.surf.kind != policy.KindChat || !c.res.AllowsModel(alias) ||
			c.res.Key.Subscription()) {
			c.routed = false
		}
	}
	// A model the key may not use is reported as missing, so other teams'
	// models cannot be discovered through 403s. Routers answer the same way.
	if !c.routed && (!found || !model.Enabled || model.Kind != c.surf.kind || !c.res.MayUse(model)) {
		s.refuse(c, refusal{
			status: http.StatusNotFound,
			typ:    "invalid_request_error", code: "model_not_found",
			msg:    "the model '" + alias + "' does not exist or this key may not use it",
			advise: true,
		})
		return false
	}
	if !c.routed && len(model.Backends) == 0 {
		s.refuse(c, refusal{
			status: http.StatusServiceUnavailable,
			typ:    "server_error", code: "no_backend",
			msg: "the model '" + alias + "' has no configured backend",
		})
		return false
	}
	if model.Subscription {
		if ref := subscriptionRefusal(c); ref != nil {
			s.refuse(c, *ref)
			return false
		}
	}
	c.model = model
	return true
}

// admit checks the rate limits and the budgets.
func (s *Server) admit(c *call) bool {
	c.now = time.Now()
	if ref, ok := s.checkRates(c.res, c.now); !ok {
		s.refuse(c, ref)
		return false
	}
	// A subscription pays for the model, so only filters can spend the
	// organisation's money, and without them no budget applies.
	if c.model.Subscription && len(c.res.Filters) == 0 {
		return true
	}
	err := s.budgets.Allow(c.res.Scopes, c.now)
	if err == nil {
		return true
	}
	if _, ok := errors.AsType[*policy.ErrBudgetExceeded](err); !ok {
		// The budgeter should only ever say yes or no, so anything else is a bug.
		s.log.Error("unexpected budget error", "error", err,
			"request_id", httpx.RequestID(c.r.Context()))
	}
	// 402 rather than 429: a client that reads "slow down" would retry against
	// a limit that does not lift until the period rolls over.
	s.refuse(c, refusal{
		status: http.StatusPaymentRequired,
		typ:    "insufficient_quota", code: "budget_exceeded",
		msg:    err.Error(),
		advise: true,
	})
	return false
}

// useRouter lets the router the client named choose the model. It runs after
// the budget check because deciding costs money.
func (s *Server) useRouter(c *call, b *body) bool {
	rt := c.router
	routeAt := time.Now()
	d, ref := s.route(c.r.Context(), rt, b, c.surf.kind)
	c.decision = d
	if d.micros > 0 {
		s.budgets.Charge(c.res.Scopes, d.micros, c.now)
	}
	// Recorded on every outcome, refusals included: a router that can no
	// longer decide shows up in exactly those rows.
	c.ev.Router, c.ev.RouterOutcome = rt.Alias, d.outcome
	c.ev.RouterMS = d.took.Milliseconds()
	// A fallback router reads nothing, so its cost is the failed attempts,
	// which are drawn on their own. It is drawn only when it has a reason
	// to show.
	if rt.Decides() || ref != nil || d.why != "" {
		c.tr.since(store.SpanRoute, rt.Alias, routeAt, routeNote(d, ref))
	}
	// A router that decides is counted here; one that tries is counted after
	// forwarding. A refusal is counted either way, since nothing else would
	// show a router that has stopped placing traffic.
	count := func(outcome store.RouterOutcome) {
		s.metrics.RouterRun(rt.Alias, c.res.Key.OrgID, string(outcome),
			d.alias, d.took.Seconds(), d.micros)
	}
	if d.why != "" {
		// The request may still succeed at the fallback, and then this is the
		// only record that it was placed by default.
		c.ev.Error = "the router " + strconv.Quote(rt.Alias) + " could not choose: " + d.why
	}
	if ref != nil {
		count(d.outcome)
		s.refuse(c, *ref)
		return false
	}
	if c.r.Context().Err() != nil {
		if rt.Decides() {
			count(d.outcome)
		}
		s.hungUp(c, "while the router was deciding", "")
		return false
	}

	// From here on the request is about the chosen model. That it was chosen
	// is carried by ev.Router alone.
	c.alias = d.alias
	model, ok := s.serveable(c.res.Key.OrgID, c.alias)
	if !ok || model.Kind != c.surf.kind {
		// Only offered destinations can be chosen, so this is an unchecked
		// fallback, or a model deleted a moment ago.
		c.ev.RouterOutcome = store.RouterError
		count(store.RouterError)
		s.refuse(c, *noDestination(fmt.Sprintf("the router %q placed this request on '%s', "+
			"which cannot serve it: the model is missing, disabled, of another kind, or has "+
			"no backend", rt.Alias, c.alias)))
		return false
	}
	if rt.Decides() {
		count(d.outcome)
	}
	c.model = model
	c.ev.Alias = c.alias
	return true
}

// useNative decides which destinations are sent the client's own API rather
// than the translation, and keeps the body for them only if some are. It
// returns the translation if any destination needs it.
func (s *Server) useNative(c *call, b *body) (*body, bool) {
	if c.native == nil {
		return b, true
	}
	d := c.surf.dialect
	// A body only the provider itself can read goes nowhere else, so the
	// other destinations are dropped from the chain.
	if why := d.needsNative(c.native); why != "" {
		var kept []policy.Model
		for _, m := range c.chain {
			if speaksNative(d, m) {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			s.refuse(c, refusal{
				status: http.StatusBadRequest,
				typ:    "invalid_request_error", code: "unsupported_parameter",
				msg: why + ". The model '" + c.alias + "' is not one of them",
			})
			return nil, false
		}
		c.chain = kept
	}
	native := false
	for _, m := range c.chain {
		if speaksNative(d, m) {
			native = true
		} else {
			c.translated = true
		}
	}
	if c.translated {
		var ok bool
		if b, ok = s.translate(c, b); !ok {
			return nil, false
		}
	}
	if !native {
		c.native = nil
	}
	return b, true
}

// useFilters runs the key's filters over the request.
//
// They run before anything the gateway adds, so they only rewrite what the
// client sent, never the administrator's system prompt. What they spent is
// charged even when they fail, so breaking a filter never makes it free.
func (s *Server) useFilters(c *call, b *body) bool {
	// The filters read the body that will be sent. When that is the client's
	// own, the translation is made again from what they left.
	target, extract := b, func(b *body) (textDoc, error) { return extractText(b, c.surf.kind) }
	if c.native != nil {
		target, extract = c.native, c.surf.dialect.text
	}
	run, ref := s.applyFilters(c.r.Context(), c.res, target, extract, c.tr)
	c.filters = run
	if run.micros > 0 {
		s.budgets.Charge(c.res.Scopes, run.micros, c.now)
	}
	// Kept on every outcome: a filter refusal is what the log is read for most.
	c.ev.FilterRuns = run.runs
	if ref != nil {
		s.refuse(c, *ref)
		return false
	}
	if c.r.Context().Err() != nil {
		s.hungUp(c, "while a filter was running", "")
		return false
	}
	if run.rewrote && c.native != nil && c.translated {
		if !s.retranslate(c, b) {
			return false
		}
	}
	filterHeader(c.w, run.applied)
	return true
}

// retranslate makes the translated body again from the client's own, after a
// filter rewrote that.
func (s *Server) retranslate(c *call, b *body) bool {
	raw, err := c.surf.shape.decode(c.native.encode())
	if err == nil {
		var fresh *body
		if fresh, err = parseBody(raw); err == nil {
			*b = *fresh
			return true
		}
	}
	s.refuse(c, *notRebuilt(err))
	return false
}

// prepare adds what the gateway adds on its own account: the standing system
// prompt, the output ceiling and the streamed usage record.
func (s *Server) prepare(c *call, b *body) bool {
	c.tr.open(store.SpanPrepare, "")

	// The guardrail's system prompt goes first; the client's own system message
	// is kept after it. Only chat has a place for one. A chat body without a
	// messages array is refused, because forwarding it would skip the prompt.
	if c.res.SystemPrompt != "" && c.surf.kind == policy.KindChat {
		if b != nil && !b.prependSystem(c.res.SystemPrompt) {
			s.refuse(c, invalidBody("the 'messages' field must be an array; a guardrail on this key "+
				"adds a system message to every chat request and has nothing here to add it to"))
			return false
		}
		if c.native != nil {
			add := c.surf.dialect.addSystem
			if c.model.Subscription {
				add = appendSystem
			}
			if err := add(c.native, c.res.SystemPrompt); err != nil {
				s.refuse(c, invalidBody(err.Error()))
				return false
			}
		}
	}
	if c.res.BlockHostedTools {
		// On chat completions a hosted search is a field rather than a tool.
		removed := []string{}
		if b != nil && b.remove("web_search_options") {
			removed = append(removed, "web_search_options")
		}
		if c.native != nil {
			removed = append(removed, c.surf.dialect.stripHostedTools(c.native)...)
		}
		removedToolsHeader(c.w, removed)
	}
	// Embeddings have no output length to limit.
	if c.res.MaxOutputTokens > 0 && c.surf.kind != policy.KindEmbedding {
		if b != nil {
			clampOutputTokens(b, c.res.MaxOutputTokens)
		}
		if c.native != nil {
			c.surf.dialect.clamp(c.native, c.res.MaxOutputTokens)
		}
	}
	// Both APIs that can go untranslated name it as the OpenAI shape does.
	sent := b
	if sent == nil {
		sent = c.native
	}
	c.stream, _ = sent.boolean("stream")
	if c.stream && b != nil {
		c.injectedUsage = b.ensureUsageInStream()
	}
	c.ev.Stream = c.stream

	// The router's and the filters' own generations are taken out, so turning
	// on a guardrail does not look like the gateway getting slower.
	s.metrics.Overhead(c.alias, c.res.Key.OrgID,
		(time.Since(c.work) - c.filters.wall - c.decision.took).Seconds())
	c.tr.seal("")
	return true
}

// answer forwards the request, serves what comes back, and accounts for it.
func (s *Server) answer(c *call, b *body) {
	fw := s.forward(c.r.Context(), c.chain, c.outbound(b), c.tr)
	// The destination is busy until the last token has been served, so it is
	// released only when this returns, on every path.
	defer fw.done()
	answered := time.Now()
	if fw.resp != nil {
		defer func() { _ = fw.resp.Body.Close() }()
	}
	if c.r.Context().Err() != nil {
		s.hungUp(c, "before the model answered", fw.note())
		return
	}
	// The destination that answered is the model this request is about now:
	// its tokens, its price, its name in the answer.
	c.model, c.alias = fw.model, fw.model.Alias
	c.ev.Alias = c.alias
	if c.routed {
		s.settleRouter(c, fw)
	}
	if fw.err != nil {
		s.upstreamUnreachable(c, fw)
		return
	}
	resp := fw.resp

	c.ev.Status = resp.StatusCode
	streaming := c.stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	s.copyResponseHeaders(c.w, resp, c.alias, streaming)
	s.limitHeaders(c.w, c.res, c.now)
	if c.model.Subscription {
		passPlanHeaders(c.w, resp.Header)
		c.ev.Plan = planUsage(resp.Header, time.Now())
	}
	native := c.native != nil && speaksNative(c.surf.dialect, c.model)
	switch {
	case streaming && native:
		ns := c.surf.dialect.stream(c.alias)
		s.relayStream(c, resp, answered, fw.payload,
			func(dst io.Writer, flush func(), src io.Reader) (streamStats, error) {
				return pipeNative(dst, flush, src, ns, s.opts.MaxResponseBytes)
			})
	case streaming:
		s.relayStream(c, resp, answered, fw.payload,
			func(dst io.Writer, flush func(), src io.Reader) (streamStats, error) {
				return c.surf.shape.pipe(dst, flush, src, c.alias, c.injectedUsage)
			})
	case native:
		s.relayNativeBuffered(c, resp, answered)
	default:
		s.relayBuffered(c, resp, answered)
	}

	c.ev.Canceled = c.r.Context().Err() != nil
	c.ev.Latency = time.Since(c.tr.start)
	// Added last, so it adds to whatever went wrong later instead of being
	// overwritten. If the request succeeded further down the chain, this is
	// the only sign anything failed.
	c.ev.Error = appendNote(c.ev.Error, fw.note())
	s.finish(c.ev, c.model, c.res, c.now, c.hookMicros(), c.tr)
}

// outbound encodes the request for one destination: in the client's own API
// for the provider whose API it is, translated for every other.
func (c *call) outbound(b *body) func(policy.Model) outbound {
	return func(m policy.Model) outbound {
		if c.native != nil && speaksNative(c.surf.dialect, m) {
			c.native.setString("model", m.BackendModel)
			auth := c.surf.dialect.auth(c.r.Header)
			if m.Subscription {
				auth = subscriptionAuth(c.r.Header)
			}
			return outbound{path: c.surf.dialect.path(), payload: c.native.encode(), auth: auth}
		}
		b.setString("model", m.BackendModel)
		return outbound{path: c.surf.path, payload: b.encode()}
	}
}

// settleRouter records what a trying router's attempts came to, and names the
// router in a header. Both are set only once a model has been reached.
func (s *Server) settleRouter(c *call, fw forwarded) {
	if !c.router.Decides() {
		c.decision.settle(fw)
		c.ev.RouterOutcome, c.ev.RouterMS = c.decision.outcome, c.decision.took.Milliseconds()
		s.metrics.RouterRun(c.router.Alias, c.res.Key.OrgID, string(c.decision.outcome),
			c.decision.alias, c.decision.took.Seconds(), c.decision.micros)
	}
	routerHeader(c.w, c.decision, c.router.Alias)
}

// upstreamUnreachable answers a request no destination could be reached for.
//
// It counts nothing on keera_upstream_errors_total: send already has.
func (s *Server) upstreamUnreachable(c *call, fw forwarded) {
	s.log.Error("inference plane unreachable", "error", fw.err, "model", c.alias,
		"request_id", httpx.RequestID(c.r.Context()))
	c.surf.shape.writeError(c.w, http.StatusBadGateway, "server_error", "upstream_unavailable",
		"the inference plane could not be reached")
	c.ev.Status = http.StatusBadGateway
	// The row keeps the real dial error (which endpoint, refused or timed out),
	// which the client is not told.
	c.ev.Error = appendNote("the inference plane could not be reached: "+fw.err.Error(),
		fw.note())
	c.ev.Latency = time.Since(c.tr.start)
	s.finish(c.ev, c.model, c.res, c.now, c.hookMicros(), c.tr)
}

// relayStream pipes a streamed answer to the client through pipe, which
// translates it or passes it through.
func (s *Server) relayStream(c *call, resp *http.Response, answered time.Time, payload []byte,
	pipe func(dst io.Writer, flush func(), src io.Reader) (streamStats, error),
) {
	c.w.WriteHeader(resp.StatusCode)
	flusher := http.NewResponseController(c.w)
	stats, err := pipe(c.w, func() { _ = flusher.Flush() }, resp.Body)
	if !stats.firstAt.IsZero() {
		c.ev.TTFT = stats.firstAt.Sub(c.tr.start)
		// Waiting for the first token (queues, cold starts) and streaming the
		// rest (answer length) are slow for different reasons, so they are two
		// steps.
		c.tr.add(store.SpanWait, c.alias, answered, stats.firstAt.Sub(answered), "")
		c.tr.since(store.SpanStream, c.alias, stats.firstAt, plural(stats.deltas, "token"))
	} else {
		c.tr.since(store.SpanWait, c.alias, answered, "no tokens")
	}
	if err != nil && c.r.Context().Err() == nil {
		s.log.Warn("stream ended early", "error", err, "model", c.alias,
			"request_id", httpx.RequestID(c.r.Context()))
		// The status stays the 200 already sent. The message is what tells a
		// cut-off stream from a finished one.
		c.ev.Error = "the stream ended before the model was done: " + err.Error()
	}
	if stats.usage != nil {
		setUsage(&c.ev, stats.usage)
		c.ev.Estimated = stats.partial
		return
	}
	// The client left before the usage record arrived. The tokens were still
	// generated, so the request is charged on an estimate; otherwise any
	// client could dodge its budget by cancelling.
	c.ev.InputTokens = estimateInputTokens(payload)
	c.ev.OutputTokens = stats.deltas
	c.ev.Estimated = true
}

// relayBuffered reads a whole answer, translates it into the client's shape
// and writes it.
func (s *Server) relayBuffered(c *call, resp *http.Response, answered time.Time) {
	body, ok := s.readAnswer(c, resp, answered)
	if !ok {
		return
	}
	// Token counts are read before translation, from what the plane sent.
	var usage *tokenUsage
	if resp.StatusCode < 300 {
		usage = usageFromResponse(body)
	}
	// The shape picks the status, because a success it cannot translate has
	// to become an error.
	out, status := c.surf.shape.encode(body, c.alias, resp.StatusCode)
	if msg := bufferedError(body, resp.StatusCode, status); msg != "" {
		c.ev.Error = msg
	}
	if ct := c.surf.shape.contentType(); ct != "" {
		c.w.Header().Set("Content-Type", ct)
	}
	c.ev.Status = status
	c.w.WriteHeader(status)
	if len(out) > 0 {
		_, _ = c.w.Write(out)
	}
	c.ev.TTFT = time.Since(c.tr.start)
	// One step: nothing is visible until the whole body arrives, so waiting
	// and receiving cannot be told apart.
	c.tr.since(store.SpanRespond, c.alias, answered, "")
	if usage != nil {
		setUsage(&c.ev, usage)
	}
}

// readAnswer reads a whole buffered answer, up to MaxResponseBytes.
//
// An answer it cannot read in full is answered with a 502 and false. Passing
// on what arrived would hand the client cut JSON under the plane's 2xx, and
// the usage record at its end would be lost.
func (s *Server) readAnswer(c *call, resp *http.Response, answered time.Time) ([]byte, bool) {
	raw, err := readCapped(resp.Body, s.opts.MaxResponseBytes)
	if err == nil {
		return raw, true
	}
	c.surf.shape.writeError(c.w, http.StatusBadGateway, "server_error", "upstream_error",
		"the answer from the inference plane could not be read")
	c.ev.Status = http.StatusBadGateway
	c.ev.Error = "the answer could not be read from the inference plane: " + err.Error()
	c.ev.TTFT = time.Since(c.tr.start)
	c.tr.since(store.SpanRespond, c.alias, answered, "")
	return nil, false
}

// readCapped reads r to the end, and fails when it holds more than limit
// bytes rather than returning the first limit of them.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	// One byte past the limit tells a long answer from one that fits exactly.
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = fmt.Errorf("it is larger than the limit of %d bytes", limit)
	}
	return raw, err
}

// bufferedError is what the usage row says went wrong with a buffered answer
// that was read in full, or empty when nothing did.
func bufferedError(body []byte, upstream, status int) string {
	switch {
	case upstream >= 400:
		// The plane's own words: two 400s can mean very different things, and
		// only the message says which.
		return upstreamErrorMessage(body, upstream)
	case status >= 400:
		// The plane answered and the gateway could not translate it. The body
		// is somebody's completion, so it is not quoted.
		return "the inference plane answered, and the gateway could not translate " +
			"its answer into the shape this client asked for"
	}
	return ""
}

// setUsage copies a usage record onto the usage row.
func setUsage(ev *store.Event, u *tokenUsage) {
	ev.InputTokens = u.InputTokens
	ev.CachedInputTokens = u.cached()
	ev.OutputTokens = u.OutputTokens
}

// finish charges the request and records it.
//
// hookMicros is what the filters and the router spent. It is added to the
// row's cost so a guardrail cannot spend budget without showing up, while the
// token columns stay the answering model's own. It was already charged to the
// budgets when it was spent.
func (s *Server) finish(ev store.Event, model policy.Model, res *policy.Resolved,
	now time.Time, hookMicros int64, tr *trace,
) {
	ev.Spans = tr.steps("")
	ev.CostMicros = model.Cost(ev.InputTokens, ev.CachedInputTokens, ev.OutputTokens)
	// A subscription paid for this. What it would have cost on the API is kept
	// to compare against, and charged to no budget.
	if model.Subscription {
		ev.ListCostMicros, ev.CostMicros = ev.CostMicros, 0
	}
	s.budgets.Charge(res.Scopes, ev.CostMicros, now)
	ev.CostMicros += hookMicros

	if tokens := float64(ev.InputTokens + ev.OutputTokens); tokens > 0 {
		reqs := make([]ratelimit.Requirement, 0, len(res.Scopes))
		for _, sc := range res.Scopes {
			reqs = append(reqs, tpmBucket(sc))
		}
		// Charged as of now, not the start: a stream can run for minutes, and
		// a bucket told it is minutes younger than it is refills too much.
		s.limiter.ChargeAll(reqs, tokens, time.Now())
	}
	s.sink.Record(ev)
	s.metrics.Observe(ev.Alias, ev.OrgID, ev.Status, ev.Latency.Seconds(),
		ev.InputTokens+ev.OutputTokens)
	// Feeds the latency router. Only real answers count, because a fast
	// refusal would post the best score. Stamped with the current time, not
	// the start, so a long stream does not expire the reading at once.
	if ev.Status < 400 && !ev.Canceled {
		s.load.observe(model.Key(), ev.TTFT, time.Now())
	}
}

// clampOutputTokens holds a request to the guardrail's ceiling rather than
// refusing it, so an administrator's limit does not break a developer's editor.
func clampOutputTokens(b *body, limit int) {
	clampFields(b, limit, "max_tokens", "max_completion_tokens")
}

// clampFields holds every field of fields the request sets to limit, and sets
// the first when it sets none.
func clampFields(b *body, limit int, fields ...string) {
	asked := false
	for _, field := range fields {
		if _, present := b.value(field); !present {
			continue
		}
		asked = true
		// A value that cannot be read is overwritten too. Leaving it would
		// forward an unbounded number next to the bounded one, and the
		// upstream would pick which to honour.
		if v, ok := b.ceiling(field); !ok || v > float64(limit) {
			b.setInt(field, limit)
		}
	}
	// A request without a ceiling gets the guardrail's.
	if !asked {
		b.setInt(fields[0], limit)
	}
}
