package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/config"
	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/payment"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
	"github.com/bespinian/keera-gateway/internal/usage"
	"github.com/redis/go-redis/v9"
)

// shutdownGrace is how long in-flight requests get to finish. Generous, as one
// is often an editor mid-completion.
const shutdownGrace = 45 * time.Second

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	setMemoryLimit(log)

	// Read before any migration runs, so a mistake stops the start first.
	template, err := loadTemplate(cfg, log)
	if err != nil {
		return err
	}
	secrets, err := buildSecrets(cfg)
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.MaxDBConns)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := prepareStore(ctx, st, log); err != nil {
		return err
	}

	resealSecrets(ctx, st, secrets, log)

	reg, err := buildRegistry(ctx, st, cfg, secrets, log)
	if err != nil {
		return err
	}

	mreg := metrics.New()
	recorder := usage.NewRecorder(st, usage.Options{}, log)

	limiter, closeLimiter := buildLimiter(ctx, cfg, mreg, log)
	defer closeLimiter()

	gw := gateway.New(reg, reg.Budgets(), limiter, recorder, mreg, gateway.Options{
		MaxBodyBytes:          cfg.MaxBodyBytes,
		MaxResponseBytes:      cfg.MaxResponseBytes,
		UpstreamHeaderTimeout: cfg.UpstreamHeaderTimeout,
		UpstreamDeny:          cfg.UpstreamDeny,
		LimitPrivate:          cfg.LimitPrivate,
		PrivateHosts:          cfg.PrivateHosts,
		PanelURL:              panelURL(cfg),
		ClaudeSubscriptions:   cfg.ClaudeSubscriptions,
	}, log)

	providers, err := buildProviders(ctx, cfg, log)
	if err != nil {
		return err
	}

	if cfg.Passkeys != nil {
		log.Info("passkeys enabled", "rp_id", cfg.Passkeys.ID())
	}

	ctl := control.New(st, reg, mreg, recorder, control.Options{
		OperatorKey:         cfg.OperatorKey,
		MetricsToken:        cfg.MetricsToken,
		Secrets:             secrets,
		Providers:           providers,
		Passkeys:            cfg.Passkeys,
		ServeUI:             cfg.UI,
		SecureCookies:       cfg.SecureCookies,
		PublicURL:           cfg.PublicURL,
		TrustedProxies:      cfg.TrustedProxies,
		ChatURL:             cfg.ChatURL,
		AgentsURL:           cfg.AgentsURL,
		AgentsServiceKey:    cfg.AgentsServiceKey,
		Gateway:             gw,
		SessionGap:          cfg.SessionGap,
		Template:            template,
		Platform:            cfg.Platform,
		GatewayFee:          cfg.GatewayFee,
		Payments:            buildPayments(cfg),
		ClaudeSubscriptions: cfg.ClaudeSubscriptions,
	}, log)

	// The background context outlives the signal, so a request being served is
	// not cancelled the moment SIGTERM arrives.
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()

	var wg sync.WaitGroup
	wg.Go(func() { reg.Run(bg) })
	wg.Go(func() { recorder.Run(bg) })
	// Lets the panel show a request as soon as it is recorded.
	wg.Go(func() { ctl.Run(bg) })
	// Charges saved cards that ran low, and settles payments.
	wg.Go(func() { ctl.RunPayments(bg) })
	wg.Go(func() { sweep(bg, reg, limiter, ctl, st, log) })
	wg.Go(func() { retain(bg, st, cfg, log) })

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           routes(gw, ctl),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: it would cut off streams. The client's context and
		// the upstream header timeout bound requests instead.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return bg },
	}
	// Live request streams never end by themselves, so end them when shutdown
	// begins rather than wait out the grace.
	srv.RegisterOnShutdown(ctl.Drain)

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "panel", "/",
			"inference", httpx.InferencePrefix, "control", httpx.ControlPrefix)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("listener: %w", err)
		}
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down", "grace", shutdownGrace)
	}

	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	_ = srv.Shutdown(shutCtx)

	// Stop the workers only now, so usage from requests that finished during
	// the grace period is still written.
	stopBG()
	wg.Wait()
	recorder.Wait()
	if dropped := recorder.Dropped(); dropped > 0 {
		log.Error("usage events were dropped during this run", "count", dropped)
	}
	log.Info("stopped")
	return nil
}

func prepareStore(ctx context.Context, st *store.Store, log *slog.Logger) error {
	applied, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if len(applied) > 0 {
		log.Info("applied migrations", "versions", applied)
	}
	return nil
}

// loadTemplate reads what every new organisation starts with: the models in
// the template file.
func loadTemplate(cfg config.Config, log *slog.Logger) (store.OrgTemplate, error) {
	var t store.OrgTemplate
	if cfg.ModelsFile == "" {
		log.Warn("KEERA_MODELS_FILE is not set, so a new organisation starts with no models")
	} else {
		models, err := catalog.LoadModels(cfg.ModelsFile)
		if err != nil {
			return t, err
		}
		names := make([]string, 0, len(models))
		for i, m := range models {
			if m.Subscription && !cfg.ClaudeSubscriptions {
				return t, fmt.Errorf("%s: '%s' is paid by Claude subscriptions, which are off; "+
					"set KEERA_CLAUDE_SUBSCRIPTIONS=true or take it out", cfg.ModelsFile, m.Alias)
			}
			// Refused as the control API refuses it: the gateway cannot bill it.
			if err := cfg.Platform.Lock(&models[i]); err != nil {
				return t, fmt.Errorf("%s: '%s': %w", cfg.ModelsFile, m.Alias, err)
			}
			models[i].PriceAtFee(cfg.GatewayFee)
			names = append(names, m.Alias)
		}
		log.Info("read model template", "file", cfg.ModelsFile, "models", names)
		t.Models = models
	}
	return t, nil
}

// buildSecrets builds the box that encrypts stored credentials.
func buildSecrets(cfg config.Config) (*secret.Box, error) {
	var previous []string
	if cfg.SecretKeyPrevious != "" {
		previous = append(previous, cfg.SecretKeyPrevious)
	}
	secrets, err := secret.New(cfg.SecretKey, previous...)
	if err != nil {
		return nil, fmt.Errorf("KEERA_SECRET_KEY: %w", err)
	}
	return secrets, nil
}

// resealSecrets moves every stored credential to KEERA_SECRET_KEY while
// KEERA_SECRET_KEY_PREVIOUS is set, so the previous key can be dropped. A
// failure is only logged: the previous key still works.
func resealSecrets(ctx context.Context, st *store.Store, secrets *secret.Box, log *slog.Logger) {
	if !secrets.Rotating() {
		return
	}
	unreadable := 0
	n, err := st.ResealSecrets(ctx, func(v store.SealedValue) []byte {
		var name string
		switch v.Kind {
		case store.SealedModel:
			name = registry.ModelSecretName(v.OrgID, v.Alias)
		case store.SealedMCP:
			name = registry.MCPSecretName(v.OrgID, v.Alias)
		default:
			name = control.RefreshTokenName(v.ExternalID)
		}
		fresh, err := secrets.Reseal(name, v.Sealed)
		if err != nil {
			unreadable++
		}
		return fresh
	})
	if err != nil {
		log.Error("moving stored credentials to the new KEERA_SECRET_KEY failed; "+
			"keep KEERA_SECRET_KEY_PREVIOUS set and restart", "error", err)
		return
	}
	log.Info("moved stored credentials to the new KEERA_SECRET_KEY; once every replica runs "+
		"with it, restart once more and then remove KEERA_SECRET_KEY_PREVIOUS",
		"moved", n, "unreadable", unreadable)
}

func buildRegistry(ctx context.Context, st *store.Store, cfg config.Config,
	secrets *secret.Box, log *slog.Logger,
) (*registry.Registry, error) {
	reg, err := registry.New(ctx, st, registry.Options{
		TTL:          cfg.CacheTTL,
		SpendRefresh: cfg.SpendRefresh,
		Secrets:      secrets,
		Platform:     cfg.Platform,
		Prepaid:      cfg.Payments != nil,
		GatewayFee:   cfg.GatewayFee,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("load control-plane state: %w", err)
	}
	return reg, nil
}

// buildProviders discovers the identity providers at start, so a typo in an
// issuer URL fails the start rather than the first sign-in.
func buildProviders(ctx context.Context, cfg config.Config, log *slog.Logger) (authn.Providers, error) {
	providers, err := authn.NewProviders(ctx, cfg.OIDC, nil)
	if err != nil {
		return nil, fmt.Errorf("single sign-on: %w", err)
	}
	switch {
	case providers.Enabled():
		for _, p := range providers {
			log.Info("single sign-on enabled", "provider", p.Name(), "issuer", p.Issuer(),
				"admin_groups", p.Mapping().AdminGroups,
				"operator_groups", p.Mapping().OperatorGroups)
			warnGoogleGroups(p, log)
		}
	case cfg.UI && cfg.Passkeys == nil:
		log.Warn("no identity provider configured; the control panel accepts only " +
			"the operator key, and audit entries cannot name a person")
	}
	return providers, nil
}

// warnGoogleGroups warns about a group mapping on Google Workspace, which
// sends no groups claim, so it matches nobody. A mapped admin group means
// nobody can ever become an administrator.
func warnGoogleGroups(p *authn.OIDC, log *slog.Logger) {
	if !p.IsGoogle() {
		return
	}
	setting := config.OIDCEnvPrefix(p.Name())
	switch {
	case p.Mapping().DecidesAdmin():
		log.Warn(setting+"ADMIN_GROUPS is set for a Google Workspace provider, "+
			"which issues no groups claim; it will match nobody, and setting it "+
			"stops roles being assigned in the panel, the CLI and the API - "+
			"unset it and make administrators with `keera user role`",
			"provider", p.Name())
	case p.Mapping().UsesGroups():
		log.Warn(setting+"OPERATOR_GROUPS is set for a Google Workspace "+
			"provider, which issues no groups claim; it will match nobody - "+
			"name operators in KEERA_OPERATORS instead",
			"provider", p.Name())
	}
}

// routes sends inference and MCP to the gateway handler and everything else to
// the control handler, so a deployment publishes one port.
func routes(gw *gateway.Server, ctl *control.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(httpx.InferencePrefix+"/", gw.Handler())
	mux.HandleFunc("/v1/", noInferenceHere)
	mux.Handle("/", ctl.Handler())
	return mux
}

// noInferenceHere answers a client given the panel's address, or the old
// two-port one, without /api, with an error that says where the API is.
func noInferenceHere(w http.ResponseWriter, r *http.Request) {
	msg := "the inference API is at " + httpx.InferencePrefix + r.URL.Path + ", not " + r.URL.Path +
		": add " + httpx.InferencePrefix + " to the end of the base URL this client was given"
	httpx.WriteNotFound(w, "wrong_base_url", msg)
}

// limiter is the token buckets plus the sweep that bounds their memory.
type limiter interface {
	gateway.Limiter
	Sweep(idle time.Duration, now time.Time)
}

// buildLimiter returns the rate-limit buckets and a function that releases
// them. With KEERA_REDIS_URL, replicas share one allowance and memory is the
// fallback.
func buildLimiter(ctx context.Context, cfg config.Config, mreg *metrics.Registry,
	log *slog.Logger,
) (limiter, func()) {
	local := ratelimit.New()
	if cfg.Redis == nil {
		return local, func() {}
	}
	opt := cfg.Redis
	rdb := redis.NewClient(opt)

	// Ping once, so an unreachable Redis shows at start. Only a warning: limits
	// fall back to per replica.
	ping, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ping).Err(); err != nil {
		log.Warn("the Redis configured for rate limiting cannot be reached; limits will "+
			"bind per replica until it is back",
			"addr", opt.Addr, "error", err)
	} else {
		log.Info("rate limiting is coordinated through Redis",
			"addr", opt.Addr, "db", opt.DB, "prefix", cfg.RedisPrefix)
	}

	return ratelimit.NewRedis(rdb, local, ratelimit.RedisOptions{
		Prefix:  cfg.RedisPrefix,
		Metrics: mreg,
	}, log), func() { _ = rdb.Close() }
}

// sweep bounds the caches' memory. Without it, a public endpoint keeps one
// entry per token anyone ever sent.
func sweep(ctx context.Context, reg *registry.Registry, limiter limiter,
	ctl *control.Server, st *store.Store, log *slog.Logger,
) {
	every(ctx, time.Minute, false, func(now time.Time) {
		reg.Sweep()
		limiter.Sweep(control.BucketIdle, now)
		// One sign-in bucket per address, unbounded for the same reason.
		ctl.Sweep(now)
		// Every lookup filters on expiry anyway, so a failure only logs.
		if !myTurn(ctx, st, log, "purge-expired", time.Minute) {
			return
		}
		if err := st.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
			log.Warn("purging expired sessions failed", "error", err)
		}
	})
}

// every runs fn every d until ctx is done, and also at the start when first is
// set.
func every(ctx context.Context, d time.Duration, first bool, fn func(time.Time)) {
	if first {
		fn(time.Now())
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			fn(now)
		}
	}
}

// retentionInterval is how often retention runs: hourly is plenty for rows kept
// for months.
const retentionInterval = time.Hour

// retain enforces the retention windows, if any are set. All default to
// keeping everything: that is for finance and compliance to decide.
func retain(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	if cfg.UsageRetention == 0 && cfg.AuditRetention == 0 {
		log.Info("no retention window is configured; usage events and audit entries " +
			"are kept indefinitely (KEERA_USAGE_RETENTION, KEERA_AUDIT_RETENTION)")
		return
	}
	log.Info("retention configured", "usage", retentionLabel(cfg.UsageRetention),
		"audit", retentionLabel(cfg.AuditRetention))

	// Once at start too, so a wrong window shows in the log right away.
	every(ctx, retentionInterval, true, func(time.Time) { purgeOld(ctx, st, cfg, log) })
}

// purgeOld deletes usage events and audit entries past their windows, and
// keys that ended before the usage window. Daily usage totals are
// kept for ever: they are small, and long reports read them.
func purgeOld(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	if !myTurn(ctx, st, log, "retention", retentionInterval) {
		return
	}
	now := time.Now()
	if cfg.UsageRetention > 0 {
		n, err := st.PurgeUsage(ctx, now.Add(-cfg.UsageRetention))
		logPurge(ctx, log, "usage past its retention window", n, err)
		n, err = st.PurgeEnded(ctx, now.Add(-cfg.UsageRetention))
		logPurge(ctx, log, "keys that ended before the usage retention window", n, err)
	}
	if cfg.AuditRetention > 0 {
		n, err := st.PurgeAudit(ctx, now.Add(-cfg.AuditRetention))
		logPurge(ctx, log, "audit entries past their retention window", n, err)
	}
}

// logPurge reports one purge. A failure caused by shutting down is not one.
func logPurge(ctx context.Context, log *slog.Logger, what string, n int64, err error) {
	switch {
	case err != nil && ctx.Err() == nil:
		log.Error("purging "+what+" failed", "error", err, "deleted", n)
	case n > 0:
		log.Info("purged "+what, "deleted", n)
	}
}

// retentionLabel renders a window for the startup line, so zero reads as
// "kept indefinitely" rather than "0s".
func retentionLabel(d time.Duration) string {
	if d == 0 {
		return "kept indefinitely"
	}
	return d.String()
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	// Config refused any level slog does not know.
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// myTurn reports whether this replica runs a background job now. See
// store.TakeTurn.
func myTurn(ctx context.Context, st *store.Store, log *slog.Logger, job string,
	every time.Duration,
) bool {
	ok, err := st.TakeTurn(ctx, job, every)
	if err != nil && ctx.Err() == nil {
		log.Warn("asking whether to run a background job failed", "job", job, "error", err)
	}
	return ok
}

// panelURL is where a refusal points the developer: empty unless the
// deployment serves a panel and declares an address a browser can reach.
func panelURL(cfg config.Config) string {
	if !cfg.UI {
		return ""
	}
	return cfg.PublicURL
}

// buildPayments is the PostFinance Checkout account organisations pay into,
// or nil without one.
func buildPayments(cfg config.Config) *control.Payments {
	if cfg.Payments == nil {
		return nil
	}
	return &control.Payments{
		Client: payment.New(cfg.Payments.PostFinance),
		VATBP:  cfg.Payments.VATBP,
	}
}
