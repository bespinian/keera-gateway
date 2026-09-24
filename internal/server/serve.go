package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/config"
	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/forge"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/metrics"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/sandbox"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
	"github.com/bespinian/keera-gateway/internal/usage"
	"github.com/redis/go-redis/v9"
)

// isGoogle reports whether an issuer is Google's. Google's ID tokens carry no
// groups claim at all.
func isGoogle(issuer string) bool {
	return strings.Contains(issuer, "accounts.google.com")
}

// shutdownGrace is how long in-flight requests get to finish. It is generous
// because an in-flight request is often an editor mid-completion.
const shutdownGrace = 45 * time.Second

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.MaxDBConns)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := prepareStore(ctx, st, cfg, log); err != nil {
		return err
	}

	secrets, err := buildSecrets(cfg, log)
	if err != nil {
		return err
	}

	reg, err := buildRegistry(ctx, st, cfg, secrets, log)
	if err != nil {
		return err
	}

	mreg := metrics.New()
	recorder := usage.NewRecorder(st, usage.Options{}, log)

	limiter, closeLimiter, err := buildLimiter(ctx, cfg, mreg, log)
	if err != nil {
		return err
	}
	defer closeLimiter()

	gw := gateway.New(reg, reg.Budgets(), limiter, recorder, mreg, gateway.Options{
		MaxBodyBytes:          cfg.MaxBodyBytes,
		MaxResponseBytes:      cfg.MaxResponseBytes,
		UpstreamHeaderTimeout: cfg.UpstreamHeaderTimeout,
		APIKeys:               config.APIKey,
		Currency:              cfg.Currency,
		// Where a refusal points the developer. Empty unless the deployment
		// declares a panel a browser can reach.
		PanelURL: cfg.PublicURL,
	}, log)

	providers, err := buildProviders(ctx, cfg, log)
	if err != nil {
		return err
	}

	sandboxes, err := buildSandboxes(ctx, st, reg, cfg, log)
	if err != nil {
		return err
	}

	ctl := control.New(st, reg, mreg, recorder, control.Options{
		OperatorKey: cfg.OperatorKey,
		// Only reads /metrics, so a scrape config does not need the operator key.
		MetricsToken:     cfg.MetricsToken,
		Secrets:          secrets,
		Currency:         cfg.Currency,
		Providers:        providers,
		OIDCAdoptByEmail: cfg.OIDCAdoptByEmail,
		ServeUI:          cfg.UI,
		SecureCookies:    cfg.SecureCookies,
		// The browser's origin. Editors send inference to the same origin.
		PublicURL: cfg.PublicURL,
		// The playground uses the real data plane, so it tests the real path.
		Gateway: gw,
		// How long an agent conversation may go quiet before the next request
		// starts a new task.
		SessionGap: cfg.SessionGap,
		// Nil when the deployment lends out no sandboxes.
		Sandboxes: sandboxes,
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
	wg.Go(func() { sweep(bg, reg, limiter, ctl, st, log) })
	wg.Go(func() { retain(bg, st, cfg, log) })
	// Its own loop, because it talks to a cluster and the cache sweep must
	// never wait on one.
	wg.Go(func() { sweepSandboxes(bg, sandboxes) })

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           routes(gw, ctl),
		ReadHeaderTimeout: 15 * time.Second,
		// No write timeout: it would cut off a streamed completion or the
		// panel's live feed. The client's context and the upstream header
		// timeout bound requests instead.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return bg },
	}
	// A live request stream never ends by itself, and Shutdown waits for it.
	// Ending the streams when shutdown begins avoids waiting out the grace.
	srv.RegisterOnShutdown(ctl.Drain)

	if err := serveUntilDone(ctx, srv, cfg.Addr, log); err != nil {
		return err
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

// prepareStore applies the migrations and the catalogue files.
func prepareStore(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	applied, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if len(applied) > 0 {
		log.Info("applied migrations", "versions", applied)
	}

	if cfg.Sandbox.File != "" {
		if err := applySandboxCatalogue(ctx, st, cfg, log); err != nil {
			return err
		}
	}

	if cfg.ModelsFile != "" {
		c, err := catalog.Apply(ctx, st, cfg.ModelsFile)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(c.Models))
		for _, m := range c.Models {
			names = append(names, m.Alias)
		}
		servers := make([]string, 0, len(c.MCPServers))
		for _, m := range c.MCPServers {
			servers = append(servers, m.Alias)
		}
		log.Info("applied model catalogue", "file", cfg.ModelsFile, "models", names,
			"mcp_servers", servers)
	}
	return nil
}

// applySandboxCatalogue applies the sandbox classes and warns about settings
// that look like they work but do not.
func applySandboxCatalogue(ctx context.Context, st *store.Store, cfg config.Config,
	log *slog.Logger,
) error {
	classes, err := catalog.ApplySandboxes(ctx, st, cfg.Sandbox.File)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(classes))
	for _, c := range classes {
		names = append(names, c.Name)
	}
	log.Info("applied sandbox catalogue", "file", cfg.Sandbox.File, "classes", names)

	// Without a driver the classes show on the panel and none can start, which
	// looks broken rather than unconfigured.
	if !cfg.Sandbox.Enabled() {
		log.Warn("a sandbox catalogue is declared but no driver is configured, so these "+
			"classes are listed and none of them can be started; set "+
			"KEERA_SANDBOX_DRIVER to 'kubernetes' or 'podman'",
			"file", cfg.Sandbox.File, "classes", len(classes))
	}
	// Nothing applies per-class egress, and from the outside that is invisible,
	// so it is said on every start.
	for _, c := range classes {
		if len(c.Egress) > 0 {
			log.Warn("a sandbox class declares egress, and no driver applies per-class "+
				"egress rules; what constrains a sandbox's network is this deployment's "+
				"own NetworkPolicy over the sandbox namespace - and on the podman driver, "+
				"nothing does",
				"class", c.Name, "egress", c.Egress)
		}
	}
	return nil
}

// buildSecrets builds the key that decrypts hosted-provider credentials. It is
// built early, so a bad value fails the start rather than the first request.
func buildSecrets(cfg config.Config, log *slog.Logger) (*secret.Box, error) {
	secrets, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("KEERA_SECRET_KEY: %w", err)
	}
	if !secrets.Enabled() {
		log.Info("no credential encryption key is configured; hosted models take their " +
			"credentials from the environment and none can be set in the panel " +
			"(set KEERA_SECRET_KEY to change that)")
	}
	return secrets, nil
}

// buildRegistry loads the control-plane state the gateway serves from.
func buildRegistry(ctx context.Context, st *store.Store, cfg config.Config,
	secrets *secret.Box, log *slog.Logger,
) (*registry.Registry, error) {
	reg, err := registry.New(ctx, st, registry.Options{
		TTL:          cfg.CacheTTL,
		SpendRefresh: cfg.SpendRefresh,
		Secrets:      secrets,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("load control-plane state: %w", err)
	}
	if len(reg.Models()) == 0 {
		log.Warn("the model catalogue is empty; every request will be refused until a model exists")
	}
	return reg, nil
}

// buildProviders discovers the identity providers. Discovery runs at start, so
// a typo in an issuer URL fails the start rather than the first sign-in.
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
	case cfg.UI:
		log.Warn("no identity provider configured; the control panel accepts only " +
			"the operator key, and audit entries cannot name a person")
	}
	return providers, nil
}

// warnGoogleGroups warns about a group mapping on Google Workspace, which
// issues no groups claim, so a mapped group matches nobody. An admin group is
// worse: it stops roles being assigned anywhere else, so nobody can ever be
// made an administrator.
func warnGoogleGroups(p *authn.OIDC, log *slog.Logger) {
	if !isGoogle(p.Issuer()) {
		return
	}
	switch {
	case p.Mapping().DecidesAdmin():
		log.Warn("KEERA_OIDC_ADMIN_GROUPS is set for a Google Workspace provider, "+
			"which issues no groups claim; it will match nobody, and setting it "+
			"stops roles being assigned in the panel, the CLI and the API - "+
			"unset it and make administrators with `keera user role`",
			"provider", p.Name())
	case p.Mapping().UsesGroups():
		log.Warn("KEERA_OIDC_OPERATOR_GROUPS is set for a Google Workspace "+
			"provider, which issues no groups claim; it will match nobody - "+
			"name operators in KEERA_OPERATORS instead",
			"provider", p.Name())
	}
}

// serveUntilDone serves until the listener fails or ctx is done. It returns
// the listener's error, or nil when it is time to shut down.
func serveUntilDone(ctx context.Context, srv *http.Server, addr string, log *slog.Logger) error {
	errs := make(chan error, 1)
	go func() { errs <- listen(srv, addr, log) }()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down", "grace", shutdownGrace)
		return nil
	}
}

// routes is the listener's routing table: the panel at the root, inference
// under httpx.InferencePrefix and the control API under httpx.ControlPrefix,
// so a deployment publishes one port.
//
// The control handler owns / and the probes. Each handler carries its own
// middleware, so nothing is wrapped again here.
func routes(gw *gateway.Server, ctl *control.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(httpx.InferencePrefix+"/", gw.Handler())
	mux.Handle("/", ctl.Handler())
	return mux
}

func listen(srv *http.Server, addr string, log *slog.Logger) error {
	log.Info("listening", "addr", addr, "panel", "/",
		"inference", httpx.InferencePrefix, "control", httpx.ControlPrefix)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listener: %w", err)
	}
	return nil
}

// limiter is the token buckets plus the sweep that bounds their memory.
type limiter interface {
	gateway.Limiter
	Sweep(idle time.Duration, now time.Time)
}

// buildLimiter returns the rate-limit buckets and a function that releases
// them.
//
// The buckets are in memory unless KEERA_REDIS_URL is set. With Redis, all
// replicas share one allowance, and the in-memory limiter is the fallback.
func buildLimiter(ctx context.Context, cfg config.Config, mreg *metrics.Registry,
	log *slog.Logger,
) (limiter, func(), error) {
	local := ratelimit.New()
	if cfg.RedisURL == "" {
		return local, func() {}, nil
	}
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("KEERA_REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opt)

	// Ping once, so an unreachable Redis shows up at start. It is a warning,
	// not a refusal: limits fall back to per replica, and Redis stays optional.
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
	}, log), func() { _ = rdb.Close() }, nil
}

// sweep bounds the memory the caches hold. Without it a public endpoint keeps
// one entry per token anyone has ever sent.
func sweep(ctx context.Context, reg *registry.Registry, limiter limiter,
	ctl *control.Server, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			reg.Sweep()
			limiter.Sweep(10*time.Minute, now)
			// One sign-in bucket per address, unbounded for the same reason.
			ctl.Sweep(now)
			// Every lookup filters on expiry anyway, so a failure only logs.
			if err := st.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
				log.Warn("purging expired sessions failed", "error", err)
			}
		}
	}
}

// retentionInterval is how often retention runs. Hourly is plenty for rows
// kept for months, and a big DELETE beside live traffic should be rare.
const retentionInterval = time.Hour

// retain enforces the retention windows, if any are set.
//
// Both default to keeping everything: how long usage and audit rows must be
// kept is a decision for finance and compliance, not for the gateway.
func retain(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	if cfg.UsageRetention == 0 && cfg.AuditRetention == 0 {
		log.Info("no retention window is configured; usage events and audit entries " +
			"are kept indefinitely (KEERA_USAGE_RETENTION, KEERA_AUDIT_RETENTION)")
		return
	}
	log.Info("retention configured", "usage", retentionLabel(cfg.UsageRetention),
		"audit", retentionLabel(cfg.AuditRetention))

	// Once at start, so a deployment that was down catches up now and a wrong
	// window shows in the log right away.
	purgeOld(ctx, st, cfg, log)
	t := time.NewTicker(retentionInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purgeOld(ctx, st, cfg, log)
		}
	}
}

// purgeOld deletes the usage events and audit entries past their windows.
func purgeOld(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	now := time.Now()
	if cfg.UsageRetention > 0 {
		n, err := st.PurgeUsage(ctx, now.Add(-cfg.UsageRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("purging old usage events failed", "error", err, "deleted", n)
		case n > 0:
			log.Info("purged usage events past their retention window", "deleted", n)
		}
	}
	if cfg.AuditRetention > 0 {
		n, err := st.PurgeAudit(ctx, now.Add(-cfg.AuditRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("purging old audit entries failed", "error", err, "deleted", n)
		case n > 0:
			log.Info("purged audit entries past their retention window", "deleted", n)
		}
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
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

/* ------------------------------------------------------------------ sandboxes */

// buildSandboxes builds the driver and the manager, or returns nil when no
// driver is configured.
//
// Every failure here fails the start, like OIDC discovery: a missing CRD or a
// missing podman should show up now, not at the first request.
func buildSandboxes(ctx context.Context, st *store.Store, reg *registry.Registry,
	cfg config.Config, log *slog.Logger,
) (*sandbox.Manager, error) {
	if !cfg.Sandbox.Enabled() {
		return nil, nil
	}

	driver, err := buildSandboxDriver(ctx, cfg, log)
	if err != nil {
		return nil, fmt.Errorf("sandbox driver %s: %w", cfg.Sandbox.Driver, err)
	}

	// Where a sandbox sends inference. Inside a cluster this should be the
	// in-cluster Service, not the public name behind the load balancer.
	base := cfg.Sandbox.PublicURL
	if base == "" {
		base = cfg.PublicURL
	}
	if base == "" {
		log.Warn("sandboxes are enabled but no address is configured for them to reach this " +
			"gateway on; sandboxes will be created with no inference configured, which is a " +
			"machine a developer can work in and an agent cannot " +
			"(set KEERA_SANDBOX_PUBLIC_URL to this gateway's in-cluster address)")
	}

	caps := driver.Capabilities()
	log.Info("sandboxes enabled", "driver", driver.Name(),
		"isolation", caps.Isolation, "suspend", caps.Suspend, "warm", caps.Warm,
		"namespace", cfg.Sandbox.Namespace, "idle_suspend", cfg.Sandbox.IdleSuspend)
	// Without a RuntimeClass, sandboxes get the same isolation as every other
	// pod on the node, which is weak for running a model's output.
	if caps.Isolation == policy.IsolationStandard {
		log.Warn("no sandbox RuntimeClass is mapped, so sandboxes run with the node's own " +
			"isolation; a class asking for 'isolated' or 'vm' will be refused rather than " +
			"quietly downgraded (KEERA_SANDBOX_RUNTIME_ISOLATED, KEERA_SANDBOX_RUNTIME_VM)")
	}

	git, err := buildGitMinter(cfg.Sandbox.Git)
	if err != nil {
		return nil, fmt.Errorf("sandbox repository credentials (KEERA_SANDBOX_GIT_*): %w", err)
	}
	if git == nil {
		log.Info("no forge is configured for sandboxes, so they are created without a " +
			"repository (set KEERA_SANDBOX_GIT_FORGE)")
	} else {
		log.Info("sandboxes check out from a forge", "forge", cfg.Sandbox.Git.Forge,
			"url", cfg.Sandbox.Git.URL)
	}

	return sandbox.NewManager(st, driver, sandbox.ManagerOptions{
		PublicURL:    base,
		DefaultModel: cfg.Sandbox.Model,
		IdleSuspend:  cfg.Sandbox.IdleSuspend,
		Git:          git,
		// A new sandbox key must work on every replica at once, and a revoked
		// one must stop at once, not a cache lifetime later.
		OnChange: func() {
			reg.Invalidate()
			if err := st.Notify(context.WithoutCancel(ctx)); err != nil {
				log.Warn("announcing a sandbox key change failed", "error", err)
			}
		},
		Log: log,
	}), nil
}

// buildGitMinter builds the configured forge's minter, or returns nil when
// there is none.
func buildGitMinter(cfg config.GitConfig) (sandbox.GitMinter, error) {
	switch cfg.Forge {
	case "github":
		key, err := os.ReadFile(cfg.AppKeyFile)
		if err != nil {
			return nil, err
		}
		return forge.NewGitHub(forge.GitHubOptions{API: cfg.URL, AppID: cfg.AppID, Key: key})
	case "gitlab":
		token, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, err
		}
		return forge.NewGitLab(forge.GitLabOptions{URL: cfg.URL, Token: string(token)})
	}
	return nil, nil
}

// buildSandboxDriver builds the configured driver.
func buildSandboxDriver(ctx context.Context, cfg config.Config, log *slog.Logger) (sandbox.Driver, error) {
	switch cfg.Sandbox.Driver {
	case "kubernetes":
		return sandbox.NewKubernetes(ctx, sandbox.KubernetesOptions{
			Kube: sandbox.KubeConfig{
				Server:    cfg.Sandbox.KubeServer,
				TokenPath: cfg.Sandbox.KubeTokenFile,
				CAFile:    cfg.Sandbox.KubeCAFile,
				Insecure:  cfg.Sandbox.KubeInsecure,
			},
			Namespace:        cfg.Sandbox.Namespace,
			Runtimes:         cfg.Sandbox.Runtimes,
			StorageClass:     cfg.Sandbox.StorageClass,
			ServiceAccount:   cfg.Sandbox.ServiceAccount,
			ImagePullSecrets: cfg.Sandbox.ImagePullSecrets,
			Warm:             cfg.Sandbox.Warm,
			Log:              log,
		})
	case "podman":
		return sandbox.NewPodman(ctx, sandbox.PodmanOptions{
			Binary:   cfg.Sandbox.PodmanBinary,
			Network:  cfg.Sandbox.PodmanNetwork,
			Runtimes: cfg.Sandbox.Runtimes,
			Log:      log,
		})
	}
	return nil, nil
}

// sandboxSweepInterval is how often the sandbox sweep runs. A minute is the
// granularity of "my sandbox is ready" and "its time is up"; more often would
// be cluster calls for nothing.
const sandboxSweepInterval = time.Minute

// sweepSandboxes runs the sandbox reconcile loop.
func sweepSandboxes(ctx context.Context, m *sandbox.Manager) {
	if m == nil {
		return
	}
	t := time.NewTicker(sandboxSweepInterval)
	defer t.Stop()
	// Once right away, so a restarted gateway reconciles what it left running
	// and tears down what expired while it was down.
	m.Sweep(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.Sweep(ctx, now)
		}
	}
}
