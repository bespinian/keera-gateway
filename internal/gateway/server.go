// Package gateway is Keera Gateway's inference data plane: it authenticates a
// request, enforces the guardrails attached to the key that made it, forwards it to
// the inference plane, and accounts for what it cost.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Budgeter is the gateway's view of spend.
type Budgeter interface {
	Allow(scopes []policy.Scope, now time.Time) error
	Charge(scopes []policy.Scope, micros int64, now time.Time)
	// Spent reports what one budget window has been charged so far. It feeds
	// the budget headers, so a client sees a limit coming before it is refused.
	Spent(t policy.ScopeType, id string, p policy.Period, now time.Time) int64
	// AllowCredit reports whether an organisation may use the deployment's
	// provider keys, which it may pay for in advance. See docs/billing.md.
	AllowCredit(orgID string) error
	// HoldCredit is AllowCredit for a request, which holds the most it may
	// cost until ReleaseCredit. It returns what it held.
	HoldCredit(orgID string, micros int64) (int64, error)
	ReleaseCredit(orgID string, micros int64)
	// ChargeCredit takes a request's use of those keys from the credit.
	ChargeCredit(orgID string, micros int64)
}

// Limiter is the token buckets a request is decided against.
//
// It is an interface because the buckets live in memory or in Redis. How the
// two differ, including what happens when Redis is down, is the limiter's
// business, not this package's.
type Limiter interface {
	// Admit decides every requirement at once, charging the ones that take a
	// unit only if all of them hold, and returns the index of the first that
	// failed, or -1.
	Admit(reqs []ratelimit.Requirement, now time.Time) int
	// ChargeAll takes n units from every bucket after the fact. Token limits
	// are paid this way, because the cost is only known once the request is
	// over. Take is not used.
	ChargeAll(reqs []ratelimit.Requirement, n float64, now time.Time)
	// Remaining and Retry feed the response headers on every request, so they
	// must be cheap.
	Remaining(key string, perMinute int, now time.Time) int
	Retry(key string, perMinute int, now time.Time) time.Duration
}

// Sink receives one event per completed request.
type Sink interface {
	Record(store.Event)
}

// Options tunes the data plane.
type Options struct {
	// MaxBodyBytes caps a request. Coding agents send large contexts, so this
	// is generous by default and exists to stop a client exhausting memory.
	MaxBodyBytes int64
	// MaxResponseBytes caps a buffered upstream response, and one event of a
	// streamed one.
	MaxResponseBytes int64
	// UpstreamHeaderTimeout bounds how long the inference plane may take to
	// begin responding. It must not bound the response itself: a streamed
	// completion legitimately runs for minutes.
	UpstreamHeaderTimeout time.Duration
	// UpstreamDeny is the addresses the gateway never connects to. Nil denies
	// nothing; the setting's default is DefaultUpstreamDeny. See egress.go.
	UpstreamDeny []netip.Prefix
	// LimitPrivate keeps every organisation from addresses inside the network,
	// except through the names in PrivateHosts. See egress.go.
	LimitPrivate bool
	PrivateHosts []string
	// Currency labels the budget headers. All money is integer micro-units of
	// it.
	Currency string
	// PanelURL is the control panel as a developer's browser reaches it. When
	// set, a refusal says where to go and look. Without a panel, it says
	// nothing rather than name an address that does not answer.
	PanelURL string
	// ClaudeSubscriptions lets subscription keys through. Off, they are
	// refused, and so is every subscription model, which only they reach.
	ClaudeSubscriptions bool
}

// The defaults of the settings behind Options. internal/config reads them
// from here, so the environment and a zero Options mean the same.
const (
	DefaultMaxBodyBytes          = 32 << 20
	DefaultMaxResponseBytes      = 64 << 20
	DefaultUpstreamHeaderTimeout = 2 * time.Minute
)

// dialTimeout bounds establishing a connection to a backend.
const dialTimeout = 5 * time.Second

func (o *Options) setDefaults() {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if o.UpstreamHeaderTimeout <= 0 {
		o.UpstreamHeaderTimeout = DefaultUpstreamHeaderTimeout
	}
}

// Server is the inference listener.
type Server struct {
	src     policy.Source
	budgets Budgeter
	limiter Limiter
	sink    Sink
	metrics *metrics.Registry
	client  *http.Client
	// publicClient is client for an organisation that signed itself up and
	// has not paid yet: it also refuses private addresses, so such an
	// organisation cannot reach the deployment's own network, even through a
	// public name that resolves into it.
	publicClient *http.Client
	// keyClient is the client for a model on the deployment's key. A hosted
	// provider sends nothing until a whole answer is done, and gives up on
	// one that runs too long itself, so it is given as long as the gateway
	// reads an answer whose client left. Cutting it short would lose an
	// answer the deployment pays for.
	keyClient *http.Client
	log       *slog.Logger
	opts      Options

	rr sync.Map // model key -> *atomic.Uint64, for round-robin over backends
	// load is what this process has seen of each destination, for the
	// latency and least-busy routers. See load.go.
	load *loads
	// patterns caches compiled pattern filter rules. See pattern.go.
	patterns patternCache
	// noLogprobs names the models whose backend turned out not to serve
	// logprobs, so routers ask them for a name instead and gates read only
	// the words. Every model starts out absent: a self-hosted plane serves
	// logprobs, and finding out otherwise costs one wasted request per
	// process. See choice.go.
	noLogprobs sync.Map // model key -> struct{}
}

// New builds a gateway.
func New(src policy.Source, budgets Budgeter, limiter Limiter, sink Sink,
	m *metrics.Registry, opts Options, log *slog.Logger) *Server {
	opts.setDefaults()
	return &Server{
		src:     src,
		budgets: budgets,
		limiter: limiter,
		sink:    sink,
		metrics: m,
		log:     log,
		opts:    opts,
		load:    newLoads(),
		client:  upstreamClient(opts, orgDial(opts)),
		publicClient: upstreamClient(opts, newDialer(
			denyDial(append(slices.Clone(opts.UpstreamDeny), privateRanges...),
				"this organisation can only reach public addresses until it has bought credit once"),
		).DialContext),
		keyClient: upstreamClient(withHeaderTimeout(opts, readOnFor),
			newDialer(denyDial(opts.UpstreamDeny, "KEERA_UPSTREAM_DENY blocks it")).DialContext),
	}
}

// orgDial is how an organisation's requests connect, as KEERA_UPSTREAM_DENY
// and KEERA_UPSTREAM_PRIVATE say.
func orgDial(opts Options) func(context.Context, string, string) (net.Conn, error) {
	if opts.LimitPrivate {
		return privateDial(opts.UpstreamDeny, opts.PrivateHosts)
	}
	return newDialer(denyDial(opts.UpstreamDeny, "KEERA_UPSTREAM_DENY blocks it")).DialContext
}

func newDialer(deny func(string, string, syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: deny}
}

func withHeaderTimeout(opts Options, d time.Duration) Options {
	opts.UpstreamHeaderTimeout = d
	return opts
}

// upstreamClient is the client requests leave through, connecting with dial.
func upstreamClient(opts Options, dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	return &http.Client{
		// No client timeout: a stream may run longer than any value worth
		// setting. The request context cancels it when the client leaves.
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         dial,
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
			// Compression would buffer, which defeats streaming.
			DisableCompression:    true,
			ResponseHeaderTimeout: opts.UpstreamHeaderTimeout,
		},
		// A redirect is answered, not followed. Followed, it would take the
		// request and its credential to an address nobody configured.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// modelClient is the client a request to m leaves through.
func (s *Server) modelClient(m policy.Model) *http.Client {
	if m.PlatformKey {
		return s.keyClient
	}
	return s.clientFor(m.Limited)
}

// clientFor is the client for a request of an organisation, limited or not.
func (s *Server) clientFor(limited bool) *http.Client {
	if limited {
		return s.publicClient
	}
	return s.client
}

// Handler returns the inference routes. They carry httpx.InferencePrefix, so
// the handler can be mounted on the root mux as-is and r.URL.Path stays the
// path the client actually asked for.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+httpx.InferencePrefix+"/v1/chat/completions", s.inference(chatSurface))
	mux.Handle("POST "+httpx.InferencePrefix+"/v1/completions", s.inference(completionSurface))
	mux.Handle("POST "+httpx.InferencePrefix+"/v1/embeddings", s.inference(embeddingSurface))
	// The Anthropic-shaped surface: the same chat models and guardrails in
	// another format. Clients post to /api/v1/messages?beta=true, and the
	// query string is not part of the match.
	mux.Handle("POST "+httpx.InferencePrefix+"/v1/messages", s.inference(messagesSurface))
	mux.HandleFunc("POST "+httpx.InferencePrefix+"/v1/messages/count_tokens", s.countTokens)
	mux.Handle("POST "+httpx.InferencePrefix+"/v1/responses", s.inference(responsesSurface))
	// MCP servers, behind the same keys. See mcp.go.
	mux.HandleFunc("POST "+httpx.InferencePrefix+"/mcp/{alias}", s.mcpPost)
	mux.HandleFunc("GET "+httpx.InferencePrefix+"/mcp/{alias}", s.mcpForward)
	mux.HandleFunc("DELETE "+httpx.InferencePrefix+"/mcp/{alias}", s.mcpForward)
	mux.HandleFunc("GET "+httpx.InferencePrefix+"/v1/models", s.listModels)
	mux.HandleFunc("GET "+httpx.InferencePrefix+"/v1/models/{alias}", s.getModel)
	// A warm-up probe some Anthropic-shaped clients send. The path is doubled
	// because the client appends /api/hello to a base URL that already ends
	// in /api. Answering it keeps a meaningless 404 out of the access log.
	mux.HandleFunc("HEAD "+httpx.InferencePrefix+"/api/hello", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Clients parse JSON errors. Go's own 404 and 405 are plain text, which
	// an editor shows as a blank failure.
	mux.HandleFunc(httpx.InferencePrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_found",
			"the inference API has no route "+r.Method+" "+r.URL.Path)
	})
	return httpx.Middleware(s.log, mux)
}

// authenticate resolves the presented key, answering in the shape the caller's
// surface speaks.
//
// The key is read from KeyHeader first, because then Authorization carries the
// caller's Claude sign-in instead. See subscription.go.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, sh shape) (*policy.Resolved, bool) {
	presented := strings.TrimSpace(r.Header.Get(KeyHeader))
	inHeader := presented != ""
	var err error
	switch {
	case inHeader:
	case claudeSignIn(r):
		// Claude Code signed in to a Claude plan, and pointed here without a
		// Keera key: the administrator set the address for everyone.
		msg := "this request carries a Claude sign-in but no Keera key; to use Claude Code " +
			"through Keera Gateway, " + connectHint
		if !s.opts.ClaudeSubscriptions {
			msg = "this request carries a Claude sign-in, and " + subscriptionsOff +
				"; to use a Keera key instead, run `keera connect claude-code`"
		}
		sh.writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing_api_key", msg)
		return nil, false
	default:
		presented, err = auth.FromHeader(r.Header.Get("Authorization"))
		if err != nil {
			// Some clients send the key as X-Api-Key instead of a bearer token,
			// depending on a setting. Reading both saves a 401 that is hard to
			// explain from inside an editor.
			if k := strings.TrimSpace(r.Header.Get("X-Api-Key")); k != "" {
				presented, err = k, nil
			}
		}
	}
	if err != nil {
		sh.writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing_api_key",
			"no API key was provided; send it as 'Authorization: Bearer <key>' "+
				"or as the 'x-api-key' header")
		return nil, false
	}
	res, err := s.src.Resolve(r.Context(), presented)
	switch {
	case err == nil && res.Key.Subscription() && !s.opts.ClaudeSubscriptions:
		sh.writeError(w, http.StatusForbidden, "permission_error", "subscriptions_off",
			"this is a subscription key, and "+subscriptionsOff)
		return nil, false
	case err == nil && res.Key.Subscription() && !inHeader:
		// Without the header, Authorization holds this key, so there is no
		// Claude sign-in to forward.
		sh.writeError(w, http.StatusUnauthorized, "invalid_request_error", "subscription_key",
			"this is a subscription key, for Claude Code signed in to a Claude plan; it goes "+
				"in the "+KeyHeader+" header, and the sign-in in Authorization. To set Claude "+
				"Code up, "+connectHint)
		return nil, false
	case err == nil:
		return res, true
	case errors.Is(err, policy.ErrUnknownKey):
		sh.writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
			"the API key provided is not valid")
	case errors.Is(err, policy.ErrKeyRevoked):
		// Revoked and expired look the same to the developer (it worked
		// yesterday), so both say what to do next.
		sh.writeError(w, http.StatusUnauthorized, "invalid_request_error", "revoked_api_key",
			s.advise("this API key has been revoked; ask whoever administers your "+
				"organisation to issue a new one"))
	case errors.Is(err, policy.ErrKeyExpired):
		sh.writeError(w, http.StatusUnauthorized, "invalid_request_error", "expired_api_key",
			s.advise("this API key has expired; ask whoever administers your "+
				"organisation to issue a new one"))
	case errors.Is(err, context.Canceled):
		return nil, false
	default:
		s.log.Error("resolving key failed", "error", err,
			"request_id", httpx.RequestID(r.Context()))
		sh.writeError(w, http.StatusServiceUnavailable, "server_error", "control_plane_unavailable",
			"the control plane is unavailable")
	}
	return nil, false
}

// inference builds the handler for one API surface.
func (s *Server) inference(surf surface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The clock starts before the key is resolved: fetching a key from the
		// control plane is time the client waits too, and the recorded latency
		// should match what the developer measured.
		tr := newTrace()
		res, ok := s.authenticate(w, r, surf.shape)
		if !ok {
			return
		}
		tr.since(store.SpanAuth, "", tr.start, "")
		s.serve(w, r, res, surf, tr)
	}
}

// ServeChat forwards one chat completion for a key the caller did not present
// itself: the panel's playground, which names one of the user's keys by id. It
// goes through the same rate limits, budgets and billing as that key's own
// requests.
func (s *Server) ServeChat(w http.ResponseWriter, r *http.Request, res *policy.Resolved) {
	// No auth span: the control plane authorised this caller, not this handler.
	s.serve(w, r, res, chatSurface, newTrace())
}
