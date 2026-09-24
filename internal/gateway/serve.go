package gateway

import (
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
	if c.routed && !s.useRouter(c, b) {
		return
	}
	c.chain = s.destinations(c.model, c.decision)
	if !s.fits(c, b) || !s.useNative(c) {
		return
	}
	if !s.useFilters(c, b) || !s.prepare(c, b) {
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
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.refuse(c, refusal{
				status: http.StatusRequestEntityTooLarge,
				typ:    "invalid_request_error", code: "context_length_exceeded",
				msg: "the request body exceeds the gateway's limit",
			})
		}
		return nil, false // otherwise the client went away mid-upload
	}
	return raw, true
}

// decode turns the body into the OpenAI shape and parses it. Everything after
// this is written against that one shape, except what is sent to a provider
// in the client's own API, which keeps the body as it came in c.native.
func (s *Server) decode(c *call, raw []byte) (*body, bool) {
	translated, err := c.surf.shape.decode(raw)
	if err != nil {
		s.refuse(c, invalidBody(err.Error()))
		return nil, false
	}
	b, err := parseBody(translated)
	if err != nil {
		s.refuse(c, invalidBody(err.Error()))
		return nil, false
	}
	if c.surf.dialect != nil {
		// The shape has read it already, so this cannot fail. Whether any
		// destination is sent it is decided once the destinations are known.
		c.native, _ = parseBody(raw)
	}
	// Set before anything can refuse, so a session cut short by a budget
	// shows the refusal too.
	c.ev.SessionKey = sessionKey(c.r, c.res.Key.ID, b, c.surf.kind)
	return b, true
}

// invalidBody is the refusal for a body the gateway cannot use.
func invalidBody(msg string) refusal {
	return refusal{
		status: http.StatusBadRequest,
		typ:    "invalid_request_error", code: "invalid_body", msg: msg,
	}
}

// target works out what the 'model' field names: a model from the shared
// catalogue, or one of this organisation's routers.
func (s *Server) target(c *call, b *body) bool {
	alias, hasAlias := b.str("model")
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

	// The catalogue is asked first and wins. An alias is shared by every
	// tenant, so one organisation's router must not be able to shadow it.
	model, found := s.src.Model(alias)
	if !found {
		c.router, c.routed = s.src.Router(c.res.Key.OrgID, alias)
		// Routers exist only on chat. The allow-list covers the router, not
		// its destinations: allowing a router allows where it sends.
		if c.routed && (c.surf.kind != policy.KindChat || !c.res.AllowsModel(alias)) {
			c.routed = false
		}
	}
	// A model the key may not use is reported as missing, so other teams'
	// models cannot be discovered through 403s. Routers answer the same way.
	if !c.routed && (!found || !model.Enabled || model.Kind != c.surf.kind || !c.res.AllowsModel(alias)) {
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
	err := s.budgets.Allow(c.res.Scopes, c.now)
	if err == nil {
		return true
	}
	var exceeded *policy.ErrBudgetExceeded
	if !errors.As(err, &exceeded) {
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
	// A router that decides is counted now; one that tries is counted after
	// forwarding. A refusal is counted either way, since nothing else would
	// show a router that has stopped placing traffic.
	if ref != nil || rt.Decides() {
		s.metrics.RouterRun(rt.Alias, c.res.Key.OrgID, string(d.outcome),
			d.alias, d.took.Seconds(), d.micros)
	}
	if d.why != "" {
		// The request may still succeed at the fallback, and then this is the
		// only record that it was placed by default.
		c.ev.Error = "the router " + strconv.Quote(rt.Alias) + " could not choose: " + d.why
	}
	if ref != nil {
		c.ev.CostMicros = d.micros
		s.refuse(c, *ref)
		return false
	}
	if c.r.Context().Err() != nil {
		return false // the client hung up while the router was deciding
	}

	// From here on the request is about the chosen model. That it was chosen
	// is carried by ev.Router alone.
	c.alias = d.alias
	model, found := s.src.Model(c.alias)
	if !found || !model.Enabled || model.Kind != c.surf.kind || len(model.Backends) == 0 {
		// Only offered destinations can be chosen, so this is an unchecked
		// fallback, or a model deleted a moment ago.
		c.ev.RouterOutcome = store.RouterError
		s.refuse(c, refusal{
			status: http.StatusServiceUnavailable,
			typ:    "server_error", code: "router_destination_unavailable",
			msg: fmt.Sprintf("the router %q placed this request on '%s', which cannot "+
				"serve it: the model is missing, disabled, of another kind, or has no "+
				"backend. Nothing was sent to a model", rt.Alias, c.alias),
			advise: true,
		})
		return false
	}
	c.model = model
	c.ev.Alias = c.alias
	return true
}

// useNative decides which destinations are sent the client's own API rather
// than the translation, and keeps the body for them only if some are.
func (s *Server) useNative(c *call) bool {
	if c.native == nil {
		return true
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
			return false
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
	if !native {
		c.native = nil
	}
	return true
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
		c.ev.CostMicros = c.hookMicros()
		s.refuse(c, *ref)
		return false
	}
	if c.r.Context().Err() != nil {
		return false // the client hung up while a filter was running
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
	s.refuse(c, refusal{
		status: http.StatusBadGateway,
		typ:    "server_error", code: "filter_failed",
		msg: "the filtered request could not be rebuilt, so nothing was sent " +
			"to the model: " + err.Error(),
		advise: true,
	})
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
		if !b.prependSystem(c.res.SystemPrompt) {
			s.refuse(c, invalidBody("the 'messages' field must be an array; a guardrail on this key "+
				"adds a system message to every chat request and has nothing here to add it to"))
			return false
		}
		if c.native != nil {
			if err := c.surf.dialect.addSystem(c.native, c.res.SystemPrompt); err != nil {
				s.refuse(c, invalidBody(err.Error()))
				return false
			}
		}
	}
	if c.res.BlockHostedTools {
		// On chat completions a hosted search is a field rather than a tool.
		removed := []string{}
		if b.remove("web_search_options") {
			removed = append(removed, "web_search_options")
		}
		if c.native != nil {
			removed = append(removed, c.surf.dialect.stripHostedTools(c.native)...)
		}
		removedToolsHeader(c.w, removed)
	}
	// Embeddings have no output length to limit.
	if c.res.MaxOutputTokens > 0 && c.surf.kind != policy.KindEmbedding {
		clampOutputTokens(b, c.res.MaxOutputTokens)
		if c.native != nil {
			c.surf.dialect.clamp(c.native, c.res.MaxOutputTokens)
		}
	}
	c.stream, _ = b.boolean("stream")
	if c.stream {
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
		return // the client hung up while we were connecting
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
			return outbound{
				path: c.surf.dialect.path(), payload: c.native.encode(),
				auth: c.surf.dialect.auth(c.r.Header),
			}
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
func (s *Server) upstreamUnreachable(c *call, fw forwarded) {
	s.metrics.UpstreamError(c.alias)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxResponseBytes))
	// Token counts are read before translation, from what the plane sent.
	var usage *tokenUsage
	if err == nil && resp.StatusCode < 300 {
		usage = usageFromResponse(body)
	}
	// The shape picks the status, because a success it cannot translate has
	// to become an error.
	out, status := c.surf.shape.encode(body, c.alias, resp.StatusCode)
	if msg := bufferedError(err, body, resp.StatusCode, status); msg != "" {
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

// bufferedError is what the usage row says went wrong with a buffered answer,
// or empty when nothing did.
func bufferedError(readErr error, body []byte, upstream, status int) string {
	switch {
	case readErr != nil:
		return "the answer could not be read from the inference plane: " + readErr.Error()
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
	s.budgets.Charge(res.Scopes, ev.CostMicros, now)
	ev.CostMicros += hookMicros

	if tokens := float64(ev.InputTokens + ev.OutputTokens); tokens > 0 {
		reqs := make([]ratelimit.Requirement, 0, len(res.Scopes))
		for _, sc := range res.Scopes {
			reqs = append(reqs, ratelimit.Requirement{Key: bucketKey(sc, "tpm"), PerMinute: sc.TPM})
		}
		s.limiter.ChargeAll(reqs, tokens, now)
	}
	s.sink.Record(ev)
	s.metrics.Observe(ev.Alias, ev.OrgID, ev.Status, ev.Latency.Seconds(),
		ev.InputTokens+ev.OutputTokens)
	// Feeds the latency router. Only real answers count, because a fast
	// refusal would post the best score. Stamped with the current time, not
	// the start, so a long stream does not expire the reading at once.
	if ev.Status < 400 && !ev.Canceled {
		s.load.observe(ev.Alias, ev.TTFT, time.Now())
	}
}

// estimateInputTokens is used only when a client disconnected before the
// upstream reported real counts. Four bytes per token is the usual rule of
// thumb for code, and the JSON envelope makes it a slight over-estimate, which
// is the right way to err for a guardrail.
func estimateInputTokens(payload []byte) int { return len(payload) / 4 }

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
