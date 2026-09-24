// Package control is Keera Gateway's control plane: the tenancy, guardrails and
// reporting API that the web panel and the CLI both use, the sign-in flow
// behind it, and the panel itself.
//
// The inference API and the control API never share a route: every
// developer's editor reaches the one, and the other decides what those
// editors may do.
package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
	"github.com/bespinian/keera-gateway/internal/usage"
	"github.com/bespinian/keera-gateway/internal/webui"
)

// Options configures the control plane.
type Options struct {
	// OperatorKey is the credential for automation, and the way in before
	// there are users. Empty means there is none, and nothing can sign in
	// with it.
	OperatorKey string
	// MetricsToken reads /metrics and nothing else, so a scrape configuration
	// does not need the operator key. Empty leaves /metrics to operators.
	MetricsToken string
	// Secrets seals the credentials of hosted models. Nil means there is no
	// KEERA_SECRET_KEY, and the panel says so instead of storing them.
	Secrets  *secret.Box
	Currency string
	// Providers are the identity providers, in the order the sign-in screen
	// shows them. Empty means the operator key is the only way in.
	Providers authn.Providers
	// OIDCAdoptByEmail lets a sign-in take over a person already bound to a
	// different provider's subject. See store.Link.
	OIDCAdoptByEmail bool
	// ServeUI serves the control panel from this listener's root.
	ServeUI bool
	// SecureCookies marks the session cookie Secure.
	SecureCookies bool
	// PublicURL is this gateway as a browser sees it. It is also the origin
	// put into the client configurations the panel hands out. Empty falls back
	// to the request's Host header.
	PublicURL string
	// Gateway is the data plane the playground and the checks send through.
	// Nil means there is no inference listener, and those routes say so.
	Gateway *gateway.Server
	// SessionGap is how long an agent conversation may go quiet before the
	// next request starts a new task. Zero is store.DefaultSessionGap.
	SessionGap time.Duration
	// Sandboxes lends out machines. Nil means there is no sandbox driver: the
	// sandbox catalogue can still be read and edited, and everything else
	// answers that a driver has to be switched on.
	Sandboxes *sandbox.Manager
}

// handler is a route that has already been authenticated.
type handler func(http.ResponseWriter, *http.Request, *authn.Principal)

// Server is the control listener.
type Server struct {
	st       *store.Store
	reg      *registry.Registry
	metrics  *metrics.Registry
	recorder *usage.Recorder
	opts     Options
	log      *slog.Logger

	// hasOperatorKey guards operatorHash: without it, a deployment with no
	// operator key would accept sha256(""), which anyone can send.
	operatorHash   [32]byte
	hasOperatorKey bool
	// metricsHash is the same for the scrape token.
	metricsHash     [32]byte
	hasMetricsToken bool

	// signIn throttles the routes that work for a caller who has not signed
	// in yet, keyed by client address.
	signIn *ratelimit.Limiter

	// feed tells every open request stream that the usage log has grown.
	// Run drives it.
	feed *requestFeed
}

// New builds a control server.
func New(st *store.Store, reg *registry.Registry, m *metrics.Registry, rec *usage.Recorder,
	opts Options, log *slog.Logger) *Server {
	return &Server{
		st: st, reg: reg, metrics: m, recorder: rec, opts: opts,
		operatorHash:    sha256.Sum256([]byte(opts.OperatorKey)),
		hasOperatorKey:  opts.OperatorKey != "",
		metricsHash:     sha256.Sum256([]byte(opts.MetricsToken)),
		hasMetricsToken: opts.MetricsToken != "",
		signIn:          ratelimit.New(),
		feed:            newRequestFeed(),
		log:             log,
	}
}

// Sweep bounds the memory the sign-in throttle holds: one bucket per address
// that has tried to sign in.
func (s *Server) Sweep(now time.Time) { s.signIn.Sweep(throttleIdle, now) }

// Handler returns the root handler: the control routes, the probes and, if
// enabled, the panel. The inference plane is mounted in front of it, under
// httpx.InferencePrefix.
//
// /healthz, /readyz and /metrics stay at the root rather than under
// httpx.ControlPrefix, because that is where orchestrators and scrapers look.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// route registers an authenticated route; pattern is "METHOD /path",
	// with the path under httpx.ControlPrefix.
	route := func(pattern string, h handler) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.Handle(method+" "+httpx.ControlPrefix+path, s.authenticated(h))
	}

	// Unauthenticated: liveness and readiness are for the orchestrator.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", s.metricsRoute)

	// The sign-in flow. Nobody has signed in yet, so the routes that do work
	// are throttled per address. /auth/config reads and decides nothing, so it
	// is not.
	mux.HandleFunc("GET "+httpx.ControlPrefix+"/auth/config", s.authConfig)
	mux.Handle("GET "+httpx.ControlPrefix+"/auth/login", s.throttle("login", flowRPM, s.login))
	mux.Handle("GET "+httpx.ControlPrefix+"/auth/callback", s.throttle("callback", flowRPM, s.callback))
	mux.Handle("POST "+httpx.ControlPrefix+"/auth/local", s.throttle("local", signInRPM, s.localLogin))
	// The last step of `keera login`: a one-time code redeemed for a token.
	mux.Handle("POST "+httpx.ControlPrefix+"/auth/cli/token",
		s.throttle("cli_token", signInRPM, s.cliToken))
	route("POST /auth/logout", s.logout)

	route("GET /v1/me", s.me)
	// The one read shaped for the developer: their own keys, limits and
	// refusals.
	route("GET /v1/access", s.access)

	route("POST /v1/orgs", s.createOrg)
	route("GET /v1/orgs", s.listOrgs)
	route("PATCH /v1/orgs/{id}", s.updateOrg)
	route("DELETE /v1/orgs/{id}", s.deleteOrg)

	route("POST /v1/teams", s.createTeam)
	route("GET /v1/teams", s.listTeams)
	route("PATCH /v1/teams/{id}", s.updateTeam)
	route("DELETE /v1/teams/{id}", s.deleteTeam)

	route("POST /v1/users", s.upsertUser)
	route("GET /v1/users", s.listUsers)
	route("PATCH /v1/users/{id}", s.updateUser)
	route("POST /v1/users/{id}/disable", s.disableUser)
	route("POST /v1/users/{id}/enable", s.enableUser)

	route("POST /v1/keys", s.createKey)
	route("GET /v1/keys", s.listKeys)
	route("DELETE /v1/keys/{id}", s.revokeKey)

	route("GET /v1/guardrails/{scope}/{id}", s.getGuardrails)
	route("GET /v1/guardrails/{scope}/{id}/effective", s.effectiveGuardrails)
	route("PUT /v1/guardrails/{scope}/{id}", s.putGuardrails)

	route("GET /v1/filters", s.listFilters)
	route("PUT /v1/filters/{alias}", s.putFilter)
	route("DELETE /v1/filters/{alias}", s.deleteFilter)
	route("POST /v1/filters/{alias}/check", s.checkFilter)
	route("GET /v1/filters/{alias}/report", s.filterReport)

	route("GET /v1/routers", s.listRouters)
	route("PUT /v1/routers/{alias}", s.putRouter)
	route("DELETE /v1/routers/{alias}", s.deleteRouter)
	route("POST /v1/routers/{alias}/check", s.checkRouter)
	route("GET /v1/routers/{alias}/report", s.routerReport)

	route("GET /v1/models", s.listModels)
	route("GET /v1/providers", s.listProviders)
	route("PUT /v1/models/{alias}", s.putModel)
	route("DELETE /v1/models/{alias}", s.deleteModel)
	route("POST /v1/models/{alias}/check", s.checkModel)

	// MCP servers the gateway stands in front of, and their tool calls.
	route("GET /v1/mcp-servers", s.listMCPServers)
	route("PUT /v1/mcp-servers/{alias}", s.putMCPServer)
	route("DELETE /v1/mcp-servers/{alias}", s.deleteMCPServer)
	route("GET /v1/tool-calls", s.toolCalls)

	route("POST /v1/playground/chat", s.playgroundChat)

	// What a developer puts into their editor. Read by the panel and by
	// `keera connect`.
	route("GET /v1/connect", s.listConnect)

	route("GET /v1/diagnostics", s.diagnostics)
	route("GET /v1/setup", s.setup)
	route("GET /v1/overview", s.overview)
	route("GET /v1/map", s.trafficMap)
	route("GET /v1/usage", s.usage)
	route("GET /v1/spend", s.spend)
	route("GET /v1/audit", s.audit)
	route("GET /v1/failures", s.failures)
	route("GET /v1/requests", s.requests)
	// The request log grouped into tasks. A session is named by the id of any
	// request in it.
	route("GET /v1/sessions", s.sessions)
	route("GET /v1/sessions/{id}", s.session)
	// The request log as it is written. It takes the same query as
	// /v1/requests, so what arrives live matches what a reload shows.
	route("GET /v1/requests/stream", s.requestStream)

	route("GET /v1/sandbox-classes", s.listSandboxClasses)
	route("PUT /v1/sandbox-classes/{name}", s.putSandboxClass)
	route("DELETE /v1/sandbox-classes/{name}", s.deleteSandboxClass)
	route("GET /v1/sandboxes", s.listSandboxes)
	route("POST /v1/sandboxes", s.createSandbox)
	route("GET /v1/sandboxes/{ref}", s.getSandbox)
	route("DELETE /v1/sandboxes/{ref}", s.terminateSandbox)
	route("POST /v1/sandboxes/{ref}/extend", s.extendSandbox)
	route("POST /v1/sandboxes/{ref}/suspend", s.suspendSandbox)
	route("POST /v1/sandboxes/{ref}/resume", s.resumeSandbox)
	route("GET /v1/sandbox-usage", s.sandboxUsage)

	// A byte stream into a sandbox. It is a path on this listener, not a port
	// of its own, so a deployment still has one address and one certificate.
	// See attach.go.
	s.attachRoutes(mux)

	if s.opts.ServeUI {
		mux.Handle("/", webui.Handler())
	}
	// Only the control plane is compressed. The inference plane streams
	// tokens, which must not wait in a buffer.
	return httpx.Middleware(s.log, httpx.Compress(mux))
}

// authenticated resolves the caller and rejects anyone it cannot.
func (s *Server) authenticated(next handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.principal(r)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated",
				"sign in, or send the operator key as 'Authorization: Bearer <key>'")
			return
		}
		s.withPrincipal(w, r, p, next)
	})
}

// withPrincipal applies the checks that depend on how the caller signed in,
// then runs the route.
func (s *Server) withPrincipal(w http.ResponseWriter, r *http.Request,
	p *authn.Principal, next handler) {
	// Another site can make a browser send its cookie, but not a header. Only
	// sessions ride on a cookie, so only they need the CSRF token.
	if p.Via == authn.MethodSession && !safeMethod(r.Method) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.CSRF)) != 1 {
			httpx.WriteError(w, http.StatusForbidden, "invalid_request_error", "csrf",
				"this request is missing its CSRF token; reload the page and try again")
			return
		}
	}
	next(w, r, p)
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// The rates the sign-in routes are held to, per client address. Far above
// what a person signing in needs, far below what a script can do.
const (
	signInRPM = 10
	flowRPM   = 30
	// throttleIdle is how long a bucket outlives the address that made it.
	throttleIdle = 10 * time.Minute
)

// throttle limits one unauthenticated route per client address.
//
// Unthrottled, POST /auth/local would let anyone guess the operator key, and
// GET /auth/login would let anyone fill the login flow table.
//
// It is a brake, not a security boundary: the address comes from
// X-Forwarded-For, which a caller can set. That is why the rates are generous.
func (s *Server) throttle(name string, perMinute int, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := name + "|" + clientIP(r)
		now := time.Now()
		if !s.signIn.Allow(key, perMinute, now) {
			if d := s.signIn.Retry(key, perMinute, now); d > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(max(int(d.Seconds()), 1)))
			}
			httpx.WriteError(w, http.StatusTooManyRequests, "rate_limit_error",
				"rate_limit_exceeded",
				"too many sign-in attempts from this address; wait a moment and try again")
			return
		}
		next(w, r)
	})
}

// principal identifies the caller. The operator key wins over a session, so a
// browser session cannot narrow what an automated call does.
//
// A bearer token is only looked up as a CLI token when it has that prefix, so
// a stray API key costs a string comparison and not a query.
func (s *Server) principal(r *http.Request) (*authn.Principal, error) {
	if presented, err := auth.FromHeader(r.Header.Get("Authorization")); err == nil {
		if s.hasOperatorKey && s.matchesOperatorKey(presented) {
			return &authn.Principal{Via: authn.MethodOperatorKey, Role: authn.RoleOperator}, nil
		}
		if strings.HasPrefix(presented, authn.CLITokenPrefix) {
			return s.cliPrincipal(r, presented)
		}
		return nil, authn.ErrUnauthenticated
	}
	return s.sessionPrincipal(r)
}

// matchesOperatorKey and matchesMetricsToken compare a presented credential
// with the configured one. Check hasOperatorKey or hasMetricsToken first:
// with nothing configured, "" would match.
func (s *Server) matchesOperatorKey(presented string) bool {
	return constantTimeEqual(presented, s.operatorHash)
}

func (s *Server) matchesMetricsToken(presented string) bool {
	return constantTimeEqual(presented, s.metricsHash)
}

func constantTimeEqual(presented string, want [32]byte) bool {
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// forbid writes the standard refusal for an authorisation failure.
func (s *Server) forbid(w http.ResponseWriter, msg string) {
	httpx.WriteError(w, http.StatusForbidden, "invalid_request_error", "forbidden", msg)
}

// badRequest writes the standard refusal for a request that is not valid.
func badRequest(w http.ResponseWriter, msg string) {
	httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "", msg)
}

// scopeOrg resolves the organisation a request applies to. A caller who names
// an organisation that is not theirs is refused, not silently redirected.
func (s *Server) scopeOrg(w http.ResponseWriter, p *authn.Principal, requested string) (string, bool) {
	orgID, err := p.ScopeOrg(requested)
	if err != nil {
		s.forbid(w, err.Error())
		return "", false
	}
	return orgID, true
}

// requireOrg is scopeOrg for a route that needs exactly one organisation. An
// operator looking at all of them at once is told msg.
func (s *Server) requireOrg(w http.ResponseWriter, p *authn.Principal, requested, msg string) (string, bool) {
	orgID, ok := s.scopeOrg(w, p, requested)
	if !ok {
		return "", false
	}
	if orgID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "org_required", msg)
		return "", false
	}
	return orgID, true
}

// requireOrgAdmin checks that the caller may change an organisation.
func (s *Server) requireOrgAdmin(w http.ResponseWriter, p *authn.Principal, orgID string) bool {
	if !p.CanAdminOrg(orgID) {
		s.forbid(w, "only an administrator of this organisation can do that")
		return false
	}
	return true
}

// requireGateway refuses a route that needs the inference plane when this
// process has none.
func (s *Server) requireGateway(w http.ResponseWriter) bool {
	if s.opts.Gateway == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "server_error", "no_gateway",
			"this process runs no inference listener, so it cannot reach a backend")
		return false
	}
	return true
}

// queryWindow reads a report's time window from ?from, ?to and ?since.
func queryWindow(w http.ResponseWriter, q url.Values) (from, to time.Time, ok bool) {
	from, to, err := timeRange(q.Get("from"), q.Get("to"), q.Get("since"))
	if err != nil {
		badRequest(w, err.Error())
		return from, to, false
	}
	return from, to, true
}

// ready is unauthenticated, so it gives a verdict and not a reason: a driver
// error names the database host and user. The reason goes to the log.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Ping(r.Context()); err != nil {
		s.log.Error("readiness check failed", "error", err,
			"request_id", httpx.RequestID(r.Context()))
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded", "database": "unavailable",
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"usage_written": s.recorder.Written(),
		"usage_dropped": s.recorder.Dropped(),
	})
}

// metricsRoute serves the Prometheus exposition.
//
// It is operator-only, because the metrics are labelled by organisation. A
// scrape config should not hold the operator key, so the scrape token reads
// this route and nothing else.
func (s *Server) metricsRoute(w http.ResponseWriter, r *http.Request) {
	if s.hasMetricsToken {
		if presented, err := auth.FromHeader(r.Header.Get("Authorization")); err == nil &&
			s.matchesMetricsToken(presented) {
			s.writeMetrics(w)
			return
		}
	}
	// A wrong scrape token gets the ordinary refusal, so the answer does not
	// say which kind of credential was sent.
	s.authenticated(s.serveMetrics).ServeHTTP(w, r)
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request, p *authn.Principal) {
	if !p.Unrestricted() {
		s.forbid(w, "metrics span every organisation; only an operator can read them")
		return
	}
	s.writeMetrics(w)
}

func (s *Server) writeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.Write(w)
}

// changed tells every gateway replica to drop its cached policy. The local
// registry is cleared directly, so a single process does not wait for its own
// notification.
func (s *Server) changed(r *http.Request) {
	s.reg.Invalidate()
	if err := s.st.Notify(r.Context()); err != nil {
		s.log.Warn("announcing guardrail change failed", "error", err)
	}
}

// auditf records a control-plane action. Pass an empty orgID only for an
// action that belongs to no tenant.
func (s *Server) auditf(r *http.Request, p *authn.Principal, orgID, action,
	targetType, targetID string, detail any) {
	if err := s.st.Audit(r.Context(), p.Actor(), orgID, action, targetType, targetID, detail); err != nil {
		s.log.Warn("writing audit entry failed", "error", err, "action", action)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
	case errors.Is(err, authn.ErrForbidden):
		s.forbid(w, err.Error())
	default:
		// The detail goes only to the log: a driver error can name tables and
		// hosts. The caller gets the request id, which finds the log line.
		rid := w.Header().Get("X-Request-Id")
		s.log.Error("control request failed", "error", err, "request_id", rid)
		msg := "internal error"
		if rid != "" {
			msg += "; quote request " + rid + " to whoever operates this gateway"
		}
		httpx.WriteError(w, http.StatusInternalServerError, "server_error", "", msg)
	}
}
