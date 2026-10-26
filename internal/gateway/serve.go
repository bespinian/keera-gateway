package gateway

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/store"
)

// maxAliasBytes caps the 'model' field. It is free text that ends up in a
// usage row and a refusal message.
const maxAliasBytes = 128

// call is one inference request on its way through serve.
type call struct {
	w    http.ResponseWriter
	r    *http.Request
	res  *policy.Resolved
	surf surface
	tr   *trace
	// ev is the usage row. It exists from the start, since a refused request
	// is still recorded.
	ev store.Event

	// work is when the gateway's own work began, for the overhead metric.
	work time.Time
	// now is when the limits were checked. Budgets are charged against it.
	now time.Time

	// alias and model are the model that answers. A routed request changes
	// them twice: to the route step's choice, then to the destination that
	// answered.
	alias string
	model policy.Model
	// route is the route step that chose the model, when routed.
	route policy.Route
	// router is the router the client named, if any.
	router policy.Router
	// scopes is every scope the request is charged to: the key's levels, then
	// each limit of a router it passed.
	scopes []policy.Scope
	// prompts are the system prompts of the router steps passed, in order.
	prompts []string

	decision routeDecision
	// earlier is what the route steps before the last one in a router
	// spent. The last one's is decision.
	earlier hookSpend
	filters filterRun
	// chain is the models to try, in order: the one the client or a route
	// step chose, or a trying step's list.
	chain []policy.Model

	// native is the body in the client's own API, for destinations that get
	// it untranslated (see native.go); nil when none do. translated says some
	// get the translation. Until then the OpenAI-shaped body is nil.
	native     *body
	translated bool

	stream        bool
	injectedUsage bool

	received int
	// held is the organisation credit held until the request ends. See
	// creditHold.
	held int64
}

// sent is the body that goes out: the OpenAI-shaped one, or the client's own
// when nothing is translated.
func (c *call) sent(b *body) *body {
	if b != nil {
		return b
	}
	return c.native
}

// hookMicros is what the request's routers and filters spent on their own
// models.
func (c *call) hookMicros() int64 {
	return c.filters.micros + c.decision.micros + c.earlier.micros
}

// hookBills is the calls the request's routers and filters made on the
// deployment's key.
func (c *call) hookBills() []policy.BillLine {
	return slices.Concat(c.filters.bills, c.decision.bills, c.earlier.bills)
}

func (c *call) viaRouter() bool { return c.router.Alias != "" }

// promptAdded is why a body with no place for the system prompt is refused:
// forwarding it would skip the prompt.
const promptAdded = "a guardrail or a router step adds a system prompt to this request"

// systemPrompt goes before the client's messages: the key's levels' prompts,
// outermost first, then the router steps'. The more specific comes last.
func (c *call) systemPrompt() string {
	out := c.res.SystemPrompt
	for _, p := range c.prompts {
		out = policy.JoinPrompts(out, p)
	}
	return out
}

// hookSpend is what a router's earlier route steps spent deciding.
type hookSpend struct {
	micros int64
	bills  []policy.BillLine
	took   time.Duration
}

// serve is everything after authentication: enforce, forward, account. Each
// step returns false when the request is over: refused, or the client left.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, res *policy.Resolved,
	surf surface, tr *trace,
) {
	// Clipped, so a router's limits are appended to a copy, not to the cached
	// key's list.
	c := &call{w: w, r: r, res: res, surf: surf, tr: tr, scopes: slices.Clip(res.Scopes), ev: store.Event{
		TS: tr.start, OrgID: res.Key.OrgID, ProjectID: res.Key.ProjectID, UserID: res.Key.UserID,
		KeyID: res.Key.ID,
		// Read before the headers are replaced for the backend.
		Client:          connect.Identify(r.Header.Get(connect.ClientHeader), r.UserAgent()),
		RequestID:       httpx.RequestID(r.Context()),
		ClientRequestID: httpx.ClientRequestID(r.Context()),
	}}

	raw, ok := s.receive(c)
	if !ok {
		return
	}
	c.received = len(raw)
	// Released after the request is charged, however it ends.
	defer func() { s.budgets.ReleaseCredit(c.res.Key.OrgID, c.held) }()
	// Ends what admit and router limit steps counted. c.scopes is read when
	// this runs, not now.
	defer func() { s.budgets.EndRequest(c.scopes, c.now) }()
	// Overhead starts here, so a slow client upload does not count.
	c.work = time.Now()
	tr.seal("")

	// All checks are one trace step: together they take under a millisecond.
	tr.open(store.SpanAdmit, "")
	b, ok := s.decode(c, raw)
	if !ok || !s.target(c, b) || !s.admit(c, b) {
		return
	}
	tr.seal("")

	// The router runs before the key's filters, so its route steps read the
	// unredacted request. That is why their models should be local.
	if c.viaRouter() {
		if b, ok = s.useRouter(c, b); !ok {
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

func (s *Server) receive(c *call) ([]byte, bool) {
	// Opened rather than timed, so a body refused halfway (over the size cap)
	// still shows as this step.
	c.tr.open(store.SpanReceive, "")
	return readBody(c.w, c.r, s.opts.MaxBodyBytes, func(status int, code, msg string) {
		s.refuse(c, refusal{status: status, typ: "invalid_request_error", code: code, msg: msg})
	})
}

// bodyReadTimeout limits how long a client may take to send its body, so a
// slow upload cannot hold memory forever.
const bodyReadTimeout = 2 * time.Minute

// readBody reads a body of at most limit bytes, compressed or not. A body
// too large or in an unknown encoding is answered by refuse, in the caller's
// shape. Any other error means the client left mid-upload and needs no answer.
func readBody(w http.ResponseWriter, r *http.Request, limit int64,
	refuse func(status int, code, msg string),
) ([]byte, bool) {
	// The deadline is lifted after: the connection is still read while the
	// answer streams, to notice a hang-up. Not every writer supports one.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	var src io.Reader = http.MaxBytesReader(w, r.Body, limit)
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	compressed := enc != "" && enc != "identity"
	switch enc {
	case "", "identity":
	case "gzip", "x-gzip", "deflate":
		var (
			zr  io.ReadCloser
			err error
		)
		if enc == "deflate" {
			zr, err = zlib.NewReader(src)
		} else {
			zr, err = gzip.NewReader(src)
		}
		if err != nil {
			refuse(http.StatusBadRequest, "invalid_body", "the request body is not valid "+enc)
			return nil, false
		}
		defer func() { _ = zr.Close() }()
		// The limit also applies after unpacking, so a small upload cannot
		// fill memory.
		src = io.LimitReader(zr, limit+1)
	default:
		refuse(http.StatusUnsupportedMediaType, "unsupported_encoding",
			"the gateway reads a request body plain, gzip or deflate, not "+enc)
		return nil, false
	}
	raw, err := io.ReadAll(src)
	if err == nil && int64(len(raw)) > limit {
		err = &http.MaxBytesError{Limit: limit}
	}
	if err == nil {
		return raw, true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		refuse(http.StatusRequestEntityTooLarge, "request_too_large",
			"the request body exceeds the gateway's limit")
	} else if compressed {
		// A client that went away mid-upload is sent this too, and never
		// reads it.
		refuse(http.StatusBadRequest, "invalid_body", "the request body could not be decompressed: "+err.Error())
	}
	return nil, false
}

// decode parses the body. A body in a provider's own API is kept as is in
// c.native and translated to the OpenAI shape only when needed (see
// translate). Until then b is nil.
func (s *Server) decode(c *call, raw []byte) (*body, bool) {
	d := c.surf.dialect
	b, err := parseBody(raw)
	if err != nil {
		s.refuse(c, invalidBody(err.Error()))
		return nil, false
	}
	// A backend that reads "true" or 1 as true would stream an answer the
	// gateway treats as whole, without usage, so it would go unbilled.
	if _, present := b.value("stream"); present {
		if _, ok := b.boolean("stream"); !ok {
			s.refuse(c, invalidBody("the 'stream' field must be true or false"))
			return nil, false
		}
	}
	// Set before any refusal, so a session cut short by a budget shows it.
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
// time something needs it. An OpenAI-shaped body is returned as is.
func (s *Server) translate(c *call, b *body) (*body, bool) {
	if b != nil {
		return b, true
	}
	b, err := openAIBody(c)
	if err != nil {
		s.refuse(c, invalidBody(err.Error()))
		return nil, false
	}
	return b, true
}

// openAIBody turns the client's body into the OpenAI shape, and sets the
// call's own copy of the surface to answer in the decoded shape.
func openAIBody(c *call) (*body, error) {
	b, sh, err := c.surf.shape.decodeBody(c.native.encode())
	if err == nil {
		c.surf.shape = sh
	}
	return b, err
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
// models or routers.
func (s *Server) target(c *call, b *body) bool {
	// Before translation the model is read from the client's own body, which
	// uses the same field.
	alias, _ := c.sent(b).str("model")
	if ref := aliasRefusal(alias); ref != nil {
		s.refuse(c, *ref)
		return false
	}
	c.ev.Alias = alias
	c.alias = alias

	n, ref := s.lookupModel(c.res, alias, c.surf.kind)
	// A subscription key may have named the model by another name.
	if n.model.Alias != "" {
		c.alias, c.ev.Alias = n.model.Alias, n.model.Alias
	}
	if ref != nil {
		s.refuse(c, *ref)
		return false
	}
	// A router's destinations are checked when a step chooses one.
	if c.router = n.router; n.viaRouter() {
		return true
	}
	model := n.model
	if len(model.Backends) == 0 {
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

// aliasRefusal refuses a 'model' field that is missing or that could name no
// model.
func aliasRefusal(alias string) *refusal {
	if alias == "" {
		return &refusal{
			status: http.StatusBadRequest,
			typ:    "invalid_request_error", code: "missing_model",
			msg: "the 'model' field is required; send one of the models from /v1/models",
		}
	}
	// An oversized name is not quoted back or stored, so a client cannot put
	// a huge string on every usage row.
	if len(alias) > maxAliasBytes {
		return &refusal{
			status: http.StatusNotFound,
			typ:    "invalid_request_error", code: "model_not_found",
			msg: fmt.Sprintf("the 'model' field is %d bytes long; no model is, so this "+
				"names none - send one of the models from /v1/models", len(alias)),
		}
	}
	// No model name has a control character, and Postgres refuses to store a
	// NUL byte.
	if !printable(alias) {
		return &refusal{
			status: http.StatusNotFound,
			typ:    "invalid_request_error", code: "model_not_found",
			msg: "the 'model' field holds a control character or invalid UTF-8; no model's " +
				"name does - send one of the models from /v1/models",
		}
	}
	return nil
}

// lookupModel finds the model or router alias names, or refuses it if the key
// may not call it. Its model is set only when it may be called.
func (s *Server) lookupModel(res *policy.Resolved, alias string, kind policy.Kind) (named, *refusal) {
	n, ok := s.callable(res, alias, kind)
	if !ok {
		msg, advise := modelNotFound(alias), true
		if path := s.servedAt(res, alias); path != "" {
			msg, advise = "the model '"+alias+"' is not served at this endpoint: send it to "+path, false
		}
		return named{}, &refusal{
			status: http.StatusNotFound,
			typ:    "invalid_request_error", code: "model_not_found",
			msg:    msg,
			advise: advise,
		}
	}
	if !n.viaRouter() && n.model.Locked {
		return n, &refusal{
			status: http.StatusForbidden,
			typ:    "permission_error", code: "org_limited",
			msg: "the model '" + alias + "' " + lockedReason(n.model) + "; " +
				policy.LockedMessage(n.model.OrgName),
		}
	}
	return n, nil
}

// admit checks the rate limits, credit and budgets. A request let through
// counts against the daily and monthly limits.
func (s *Server) admit(c *call, b *body) bool {
	c.now = time.Now()
	if !s.admitted(c, b) {
		return false
	}
	c.scopes = append(c.scopes, c.res.Quotas...)
	s.budgets.StartRequest(c.res.Quotas, c.now)
	return true
}

func (s *Server) admitted(c *call, b *body) bool {
	if ref, ok := s.checkRates(c.res.Scopes, c.now); !ok {
		s.refuse(c, ref)
		return false
	}
	if ref, ok := s.checkQuotas(c.res.Quotas, c.now); !ok {
		s.refuse(c, ref)
		return false
	}
	if s.onKey(c) {
		var err error
		if c.held, err = s.budgets.HoldCredit(c.res.Key.OrgID, s.creditHold(c, c.sent(b))); err != nil {
			s.refuse(c, refusal{
				status: http.StatusPaymentRequired,
				typ:    "insufficient_quota", code: "credit_exhausted",
				msg:    err.Error(),
				advise: true,
			})
			return false
		}
	}
	// A subscription pays for the model, so only filters spend the
	// organisation's money. Without them no budget applies.
	if c.model.Subscription && len(c.res.Filters) == 0 {
		return true
	}
	if ref, ok := s.checkBudgets(c, c.res.Scopes); !ok {
		s.refuse(c, ref)
		return false
	}
	return true
}

// checkBudgets checks the budgets of scopes. Like checkRates, it only decides.
func (s *Server) checkBudgets(c *call, scopes []policy.Scope) (refusal, bool) {
	err := s.budgets.Allow(scopes, c.now)
	if err == nil {
		return refusal{}, true
	}
	if _, ok := errors.AsType[*policy.ErrBudgetExceeded](err); !ok {
		// Any other error from the budgeter is a bug.
		s.log.Error("unexpected budget error", "error", err,
			httpx.LogIDs(c.r.Context()))
	}
	// 402 rather than 429: a client that reads "slow down" would retry against
	// a limit that does not lift until the period rolls over.
	return refusal{
		status: http.StatusPaymentRequired,
		typ:    "insufficient_quota", code: "budget_exceeded",
		msg:    err.Error(),
		advise: true,
	}, false
}

// useRoute lets a route step choose the model. It runs after the budget
// check because deciding costs money.
func (s *Server) useRoute(c *call, b *body) bool {
	rt := c.route
	routeAt := time.Now()
	d, ref := s.route(c.r.Context(), rt, b, c.surf.kind, c.r.Header)
	c.decision = d
	if d.micros > 0 {
		s.budgets.Charge(c.scopes, d.micros, c.now)
	}
	// Recorded on every outcome, including refusals, which is where a step
	// that can no longer decide shows up.
	c.ev.RouterOutcome = d.outcome
	c.ev.RouterMS = d.took.Milliseconds()
	// A fallback step reads nothing and its failed attempts are drawn on
	// their own, so it is drawn only when it has something to show.
	if rt.Chooses() || ref != nil || d.why != "" {
		c.tr.since(store.SpanRoute, rt.Router+"/"+rt.Step, routeAt, routeNote(d, ref))
	}
	// A choosing step is counted here, a trying step after forwarding. A
	// refusal is always counted, or a step that stopped placing traffic
	// would not show.
	count := func(outcome store.RouterOutcome) {
		s.metrics.RouterRun(rt.Router, rt.Step, c.res.Key.OrgID, string(outcome),
			d.alias, d.took.Seconds(), d.micros)
	}
	if d.why != "" {
		// A note, not an error: the fallback may still serve the request.
		// Appended, since an exceeded branch may have left a note already.
		c.ev.Note = appendNote(c.ev.Note, "the router '"+rt.Router+"' could not choose: "+d.why)
	}
	if ref != nil {
		count(d.outcome)
		s.refuse(c, *ref)
		return false
	}
	if c.r.Context().Err() != nil {
		if rt.Chooses() {
			count(d.outcome)
		}
		s.hungUp(c, "while the router was deciding", "")
		return false
	}

	// From here on the request is about the chosen model. Only
	// ev.RouterOutcome records that it was chosen.
	c.alias = d.alias
	model, ok := s.serveable(c.res.Key.OrgID, c.alias)
	if !ok || model.Kind != c.surf.kind {
		// Only offered destinations can be chosen, so this is an unchecked
		// fallback, or a model deleted a moment ago.
		c.ev.RouterOutcome = store.RouterError
		count(store.RouterError)
		s.refuse(c, *noDestination(fmt.Sprintf("the router '%s' placed this request on '%s', "+
			"which cannot serve it: the model is missing, disabled, of another kind, or has "+
			"no backend", rt.Router, c.alias)))
		return false
	}
	if rt.Chooses() {
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
	// A body only the provider can read goes nowhere else, so other
	// destinations are dropped.
	if why := d.needsNative(c.native); why != "" {
		var kept []policy.Model
		for _, m := range c.chain {
			if c.sendsNative(m) {
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
		if c.sendsNative(m) {
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
		return b, true
	}
	if slices.ContainsFunc(c.chain, func(m policy.Model) bool {
		return m.PlatformKey && c.sendsNative(m)
	}) {
		if why := d.onKey(c.native); why != "" {
			s.refuse(c, refusal{
				status: http.StatusBadRequest,
				typ:    "invalid_request_error", code: "unsupported_parameter",
				msg: why,
			})
			return nil, false
		}
	}
	return b, true
}

// sendsNative reports whether m is sent the client's own API.
func (c *call) sendsNative(m policy.Model) bool {
	d := c.surf.dialect
	return c.native != nil && d != nil && m.Provider == d.provider()
}

// useFilters runs the key's filters over the request. They run before the
// gateway adds anything, so they never rewrite the administrator's system
// prompt. Their spend is charged even when they fail, so a broken filter is
// never free.
func (s *Server) useFilters(c *call, b *body) bool {
	// A filter a router step already ran is skipped, like one named at two
	// levels: it would judge the same text twice and be paid twice.
	aliases := slices.DeleteFunc(slices.Clone(c.res.Filters), func(alias string) bool {
		return slices.ContainsFunc(c.filters.runs, func(r store.FilterRun) bool { return r.Filter == alias })
	})
	rewrote, ok := s.runFilters(c, b, aliases, guardrailFilters)
	if !ok {
		return false
	}
	// The translation is made again from what the filters left.
	if rewrote && c.native != nil && c.translated && !s.retranslate(c, b) {
		return false
	}
	// Lists the enforcing filters, so a developer can see what changed or
	// judged their prompt.
	listHeader(c.w, "X-Keera-Filters", c.filters.applied)
	return true
}

// runFilters runs filters over the body as it would be sent now, the
// client's own if still untranslated. It charges their spend, answers the
// client on a refusal or hang-up, and says whether they rewrote the body.
func (s *Server) runFilters(c *call, b *body, aliases []string, by string) (rewrote, ok bool) {
	target, extract := b, func(b *body) (textDoc, error) { return extractText(b, c.surf.kind) }
	if c.native != nil {
		target, extract = c.native, c.surf.dialect.text
	}
	run, ref := s.applyFilters(c.r.Context(), c.res.Key.OrgID, aliases, by, target, extract, c.tr)
	c.filters.add(run)
	if run.micros > 0 {
		s.budgets.Charge(c.scopes, run.micros, c.now)
	}
	// Kept on every outcome: a filter refusal is what the log is read for most.
	c.ev.FilterRuns = c.filters.runs
	if ref != nil {
		s.refuse(c, *ref)
		return false, false
	}
	if c.r.Context().Err() != nil {
		s.hungUp(c, "while a filter was running", "")
		return false, false
	}
	return run.rewrote, true
}

// retranslate makes the translated body again from the client's own, after a
// filter rewrote that.
func (s *Server) retranslate(c *call, b *body) bool {
	fresh, err := openAIBody(c)
	if err != nil {
		s.refuse(c, *notRebuilt(err))
		return false
	}
	*b = *fresh
	return true
}

// prepare adds what the gateway adds on its own account: the standing system
// prompt and the streamed usage record.
func (s *Server) prepare(c *call, b *body) bool {
	c.tr.open(store.SpanPrepare, "")

	// The system prompt goes before the client's own system message. Only
	// chat has a place for one. A chat body without a messages array is
	// refused, since forwarding it would skip the prompt.
	if prompt := c.systemPrompt(); prompt != "" && c.surf.kind == policy.KindChat {
		if b != nil && !b.prependSystem(prompt) {
			s.refuse(c, invalidBody("the 'messages' field must be an array; "+promptAdded))
			return false
		}
		if c.native != nil {
			add := c.surf.dialect.addSystem
			if c.model.Subscription {
				add = appendSystem
			}
			if err := add(c.native, prompt); err != nil {
				s.refuse(c, invalidBody(err.Error()))
				return false
			}
		}
	}
	if c.chainOnKey() {
		// Hosted tools are removed on the deployment's key: the provider
		// charges for them apart from tokens, which is not billed. The header
		// tells the developer why.
		var tools []string
		// On chat completions a hosted search is a field rather than a tool.
		if b != nil && b.remove("web_search_options") {
			tools = append(tools, "web_search_options")
		}
		if c.native != nil {
			tools = append(tools, c.surf.dialect.stripHostedTools(c.native)...)
		}
		listHeader(c.w, "X-Keera-Removed-Tools", tools)

		var fields []string
		for _, sent := range []*body{b, c.native} {
			if sent != nil {
				fields = append(fields, stripPremium(sent)...)
			}
		}
		// Every organisation shares the deployment's OpenAI account, so chat
		// completions are not stored there. The Responses dialect turns store
		// off itself, since leaving it out means on.
		if b != nil {
			if on, _ := b.boolean("store"); on {
				b.remove("store")
				fields = append(fields, "store")
			}
		}
		listHeader(c.w, "X-Keera-Removed-Fields", slices.Compact(slices.Sorted(slices.Values(fields))))
	}
	// Both untranslated APIs use the same 'stream' field as OpenAI.
	c.stream, _ = c.sent(b).boolean("stream")
	if c.stream && b != nil {
		c.injectedUsage = b.ensureUsageInStream()
	}
	c.ev.Stream = c.stream

	// Router and filter generations are left out, so a guardrail does not
	// look like the gateway getting slower.
	s.metrics.Overhead(c.alias, c.res.Key.OrgID,
		(time.Since(c.work) - c.filters.wall - c.decision.took - c.earlier.took).Seconds())
	c.tr.seal("")
	return true
}

// answer forwards the request, serves what comes back, and accounts for it.
func (s *Server) answer(c *call, b *body) {
	ctx := c.r.Context()
	if c.chainOnKey() {
		var cancel context.CancelFunc
		ctx, cancel = outlast(ctx, readOnFor)
		defer cancel()
	}
	fw := s.forward(ctx, c.chain, c.outbound(b), c.tr)
	// The destination is busy until the last token is served, so it is
	// released when this returns.
	defer fw.done()
	answered := time.Now()
	if fw.resp != nil {
		defer func() { _ = fw.resp.Body.Close() }()
	}
	// Settled before the hang-up check, so the row still shows how the route
	// step's attempts ended.
	if c.route.Router != "" {
		s.settleRoute(c, fw)
	}
	if c.r.Context().Err() != nil && !c.chainOnKey() {
		s.hungUp(c, "before the model answered", fw.note())
		return
	}
	// From here on the request is about the destination that answered.
	c.model, c.alias = fw.model, fw.model.Alias
	c.ev.Alias = c.alias
	if fw.err != nil {
		// A request that reached the provider may have been charged for,
		// even if no answer came back in time.
		if !undelivered(fw.err) {
			s.billUnreported(c, b)
		}
		s.upstreamUnreachable(c, fw)
		return
	}
	resp := fw.resp

	c.ev.Status = resp.StatusCode
	streaming := c.stream && httpx.MediaType(resp.Header.Get("Content-Type")) == "text/event-stream"
	copyResponseHeaders(c.w, resp, c.alias, streaming)
	s.limitHeaders(c.w, c.res, c.now)
	if c.model.Subscription {
		passPlanHeaders(c.w, resp.Header)
		c.ev.Plan = planUsage(resp.Header, time.Now())
	}
	native := c.sendsNative(c.model)
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
				return c.surf.shape.pipe(dst, flush, src, c.alias, c.injectedUsage,
					s.opts.MaxResponseBytes)
			})
	default:
		s.relayBuffered(c, resp, answered, native)
	}

	if resp.StatusCode < 400 {
		s.billUnreported(c, b)
	}
	c.ev.Canceled = c.r.Context().Err() != nil
	c.ev.Latency = time.Since(c.tr.start)
	// When a later destination answered, an earlier failure is a note, not
	// an error.
	if c.ev.Status >= 400 {
		c.ev.Error = appendNote(c.ev.Error, fw.note())
	} else {
		c.ev.Note = appendNote(c.ev.Note, fw.note())
	}
	s.finish(c)
	// Feeds latency route steps. Only real answers count, since a fast refusal
	// would score best. Timed from when this destination got the request, so
	// failed destinations, the upload and filters do not count. Stamped now,
	// so a long stream does not expire it at once.
	if c.ev.Status < 400 && !c.ev.Canceled && c.ev.TTFT > 0 {
		s.load.observe(c.model.Key(), c.tr.start.Add(c.ev.TTFT).Sub(fw.sent), time.Now())
	}
}

// outbound encodes the request for one destination: in the client's own API
// for the provider whose API it is, translated for every other.
func (c *call) outbound(b *body) func(policy.Model) outbound {
	clientID := httpx.ClientRequestID(c.r.Context())
	return func(m policy.Model) outbound {
		if c.sendsNative(m) {
			c.native.setString("model", m.BackendModel)
			auth := c.surf.dialect.auth(c.r.Header)
			if m.Subscription {
				auth = subscriptionAuth(c.r.Header)
			}
			return outbound{
				path: c.surf.dialect.path(), payload: c.native.encode(),
				auth: auth, clientID: clientID,
			}
		}
		b.setString("model", m.BackendModel)
		return outbound{path: c.surf.path, payload: b.encode(), clientID: clientID}
	}
}

// readOnFor is how long an answer on the deployment's key is still read after
// the client left: long enough for the model to finish.
const readOnFor = 15 * time.Minute

// chainOnKey says some model in the chain is on the deployment's key. Such an
// answer is read to the end even if the client leaves: only the end says what
// the provider charged, and an estimate would miss unstreamed output such as
// reasoning.
func (c *call) chainOnKey() bool {
	return slices.ContainsFunc(c.chain, func(m policy.Model) bool { return m.PlatformKey })
}

// outlast is a context that ends grace after parent does, rather than with
// it. The returned cancel ends it at once.
func outlast(parent context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, func() { time.AfterFunc(grace, cancel) })
	return ctx, func() {
		stop()
		cancel()
	}
}

// afterHangUp writes to the client until it has gone, and then drops what
// is written, so a stream is read to its usage record all the same.
type afterHangUp struct {
	w    io.Writer
	gone bool
}

func (a *afterHangUp) Write(p []byte) (int, error) {
	if !a.gone {
		if _, err := a.w.Write(p); err != nil {
			a.gone = true
		}
	}
	return len(p), nil
}

// settleRoute records how a trying route step's attempts ended, and names
// where the request went in a header. It runs once the attempts are over.
func (s *Server) settleRoute(c *call, fw forwarded) {
	if !c.route.Chooses() {
		c.decision.settle(fw)
		c.ev.RouterOutcome, c.ev.RouterMS = c.decision.outcome, c.decision.took.Milliseconds()
		s.metrics.RouterRun(c.route.Router, c.route.Step, c.res.Key.OrgID, string(c.decision.outcome),
			c.decision.alias, c.decision.took.Seconds(), c.decision.micros)
	}
	d := c.decision
	// The step's branch may end at a model step naming another model. The
	// header names the one that answered, as X-Keera-Model does.
	if c.route.Chooses() {
		d.alias = c.alias
	}
	routeHeader(c.w, d, c.route.Router)
}

// upstreamUnreachable answers a request no destination could be reached for.
// It does not count keera_upstream_errors_total: send already did.
func (s *Server) upstreamUnreachable(c *call, fw forwarded) {
	s.log.Error("inference plane unreachable", "error", fw.err, "model", c.alias,
		httpx.LogIDs(c.r.Context()))
	s.limitHeaders(c.w, c.res, c.now)
	c.surf.shape.writeError(c.w, http.StatusBadGateway, "server_error", "upstream_unavailable",
		"the inference plane could not be reached")
	c.ev.Status = http.StatusBadGateway
	// The row keeps the real dial error, which the client is not shown.
	c.ev.Error = appendNote("the inference plane could not be reached: "+fw.err.Error(),
		fw.note())
	c.ev.Latency = time.Since(c.tr.start)
	s.finish(c)
}

// relayStream pipes a streamed answer to the client through pipe, which
// translates it or passes it through.
func (s *Server) relayStream(c *call, resp *http.Response, answered time.Time, payload []byte,
	pipe func(dst io.Writer, flush func(), src io.Reader) (streamStats, error),
) {
	c.w.WriteHeader(resp.StatusCode)
	var dst io.Writer = c.w
	if c.chainOnKey() {
		dst = &afterHangUp{w: c.w}
	}
	stream := &flushBeforeRead{src: resp.Body, dst: dst, flusher: http.NewResponseController(c.w)}
	stats, err := pipe(stream, stream.later, stream)
	stream.now()
	if err == nil {
		err = stats.cut
	}
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
			httpx.LogIDs(c.r.Context()))
		// The status stays the 200 already sent. The message is what tells a
		// cut-off stream from a finished one.
		c.ev.Error = "the stream ended before the model was done: " + err.Error()
	}
	if stats.usage != nil {
		setUsage(&c.ev, stats.usage)
		c.ev.Estimated = stats.partial
		return
	}
	// On the deployment's key, a missing record is billed the most the call
	// may have used. See billUnreported.
	if c.model.PlatformKey {
		return
	}
	// The stream ended before its usage record: the client left or it broke
	// off. The tokens were still generated, so they are charged on an
	// estimate, or any client could dodge its budget by cancelling.
	c.ev.InputTokens = estimateInputTokens(payload)
	c.ev.OutputTokens = stats.deltas
	c.ev.Estimated = true
}

// relayBuffered reads a whole answer, translates it into the client's shape
// and writes it. A native answer only gets the alias as its model.
func (s *Server) relayBuffered(c *call, resp *http.Response, answered time.Time, native bool) {
	body, ok := s.readAnswer(c, resp, answered)
	if !ok {
		return
	}
	// Token counts are read before translation, from what the plane sent.
	var usage *tokenUsage
	out, status := body, resp.StatusCode
	if native {
		if status < 300 {
			usage = c.surf.dialect.usage(body)
			out = renameIn(body, "", c.alias)
		}
	} else {
		if status < 300 {
			usage = usageFromResponse(body)
		}
		// The shape picks the status, because a success it cannot translate
		// has to become an error.
		out, status = c.surf.shape.encode(body, c.alias, status)
		if ct := c.surf.shape.contentType(); ct != "" {
			c.w.Header().Set("Content-Type", ct)
		}
	}
	if msg := bufferedError(body, resp.StatusCode, status); msg != "" {
		c.ev.Error = msg
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

func printable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// modelNotFound is the one answer for a model that is missing and for one the
// key may not use, so other projects' models cannot be discovered.
func modelNotFound(alias string) string {
	return "the model '" + alias + "' does not exist or this key may not use it"
}

// readAnswer reads a whole buffered answer, up to MaxResponseBytes. If it
// cannot read it all, it answers 502 and returns false: passing on part would
// give the client cut JSON under a 2xx and lose the usage record.
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

func setUsage(ev *store.Event, u *tokenUsage) {
	ev.InputTokens = u.InputTokens
	ev.CachedInputTokens = u.cached()
	ev.CacheWriteTokens = u.InputDetails.CacheWriteTokens
	ev.CacheWriteHourTokens = u.InputDetails.CacheWriteHourTokens
	ev.OutputTokens = u.OutputTokens
}

// finish charges the request and records it. Filter and router spend is
// added to the row's cost, so a guardrail cannot spend budget unseen; the
// token columns stay the answering model's. That spend was charged to the
// budgets when it happened.
func (s *Server) finish(c *call) {
	ev := c.ev
	ev.Spans = c.tr.steps("")
	used := policy.Tokens{
		Input: ev.InputTokens, CachedInput: ev.CachedInputTokens,
		CacheWrite: ev.CacheWriteTokens, CacheWriteHour: ev.CacheWriteHourTokens,
		Output: ev.OutputTokens,
	}
	cost, bills := answerCost(c.model, used)
	ev.CostMicros, ev.Bills = cost, append(c.hookBills(), bills...)
	// A subscription paid for this. The API price is kept for comparison and
	// charged to no budget.
	if c.model.Subscription {
		ev.ListCostMicros, ev.CostMicros = ev.CostMicros, 0
	}
	s.budgets.Charge(c.scopes, ev.CostMicros, c.now)
	ev.CostMicros += c.hookMicros()
	ev.Scopes = c.scopes

	if tokens := float64(ev.InputTokens + ev.OutputTokens); tokens > 0 {
		reqs := make([]ratelimit.Requirement, 0, len(c.scopes))
		for _, sc := range c.scopes {
			reqs = append(reqs, tpmBucket(sc))
		}
		// Charged as of now, not the start: a stream can run for minutes, and
		// an old timestamp would refill the bucket too much.
		s.limiter.ChargeAll(reqs, tokens, time.Now())
		// Daily and monthly limits count them too, in the window the request
		// started in.
		s.budgets.Tally(c.scopes, policy.MeterTokens, int64(tokens), c.now)
	}
	s.record(ev)
	s.metrics.Observe(ev.Alias, ev.OrgID, ev.Status, ev.Latency.Seconds(),
		ev.InputTokens+ev.OutputTokens)
}
