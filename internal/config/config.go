// Package config reads Keera Gateway's settings from the environment.
//
// Every setting is an environment variable. The model and sandbox catalogues
// can also be given as files, so a NixOS or GitOps deployment can declare them
// with the rest of the system.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Config is the whole of Keera Gateway's configuration.
type Config struct {
	DatabaseURL string
	MaxDBConns  int32

	// Addr is the one listener. The panel is served at /, inference under
	// /api and the control API under /control.
	Addr string

	// OperatorKey authenticates the control API. It is the one credential not
	// stored in the database, because it is how you reach an empty one.
	OperatorKey string

	// SecretKey encrypts credentials stored through the control plane, such as
	// a hosted model's API key. Empty turns that off, and models take their
	// credentials from the environment instead.
	SecretKey string

	// MetricsToken reads /metrics and nothing else, so a scrape configuration
	// does not have to hold the operator key. Empty leaves the route to
	// operators and the operator key.
	MetricsToken string

	// ModelsFile declares the model catalogue. It is applied on every start
	// and is idempotent, so the deployment owns it rather than the API.
	ModelsFile string

	LogLevel  string
	LogFormat string

	MaxBodyBytes          int64
	MaxResponseBytes      int64
	UpstreamHeaderTimeout time.Duration
	CacheTTL              time.Duration
	SpendRefresh          time.Duration

	// UsageRetention and AuditRetention are how long usage events and audit
	// entries are kept. Zero, the default, keeps them for ever, because billing
	// and audits depend on them. See docs/sizing.md.
	UsageRetention time.Duration
	AuditRetention time.Duration

	// SessionGap is how long an agent conversation may go quiet before the
	// next request counts as a new task. Zero leaves the store's default of
	// half an hour. Nothing is stored per session, so changing it re-cuts past
	// sessions too. See docs/sessions.md.
	SessionGap time.Duration

	// Currency is a label carried into reports. All money is stored as integer
	// micro-units of it.
	Currency string

	// RedisURL is the Redis the replicas share their rate limits through.
	// Empty, the default, keeps limits per process, so N replicas allow N
	// times the limit. Nothing in Redis has to survive. See docs/gateway.md.
	RedisURL string
	// RedisPrefix namespaces the keys, so two deployments can share one Redis.
	RedisPrefix string

	// UI serves the control panel from the listener's root.
	UI bool
	// PublicURL is the gateway's origin as a browser sees it. It is used for
	// the redirect after sign-out and for the address under "Connect a
	// client". Empty uses the request's Host header.
	PublicURL string
	// SecureCookies marks the session cookie Secure. It defaults to on when
	// the public or redirect URL is https; turning it off lets a plain-http
	// demo on localhost sign in.
	SecureCookies bool

	// Sandbox is the machines this deployment lends out. Off by default.
	Sandbox SandboxConfig

	// OIDC is every identity provider, in the order the sign-in screen shows
	// them. Empty leaves the operator key as the only way in.
	OIDC []authn.OIDCConfig
	// OIDCAdoptByEmail lets a sign-in take over a person already bound to a
	// different provider's subject. See store.Link.
	OIDCAdoptByEmail bool
}

// Load reads the environment.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:           env("KEERA_DATABASE_URL", ""),
		MaxDBConns:            int32(envInt("KEERA_MAX_DB_CONNS", 16)),
		Addr:                  env("KEERA_ADDR", ":8080"),
		OperatorKey:           env("KEERA_OPERATOR_KEY", ""),
		SecretKey:             env("KEERA_SECRET_KEY", ""),
		MetricsToken:          env("KEERA_METRICS_TOKEN", ""),
		ModelsFile:            env("KEERA_MODELS_FILE", ""),
		LogLevel:              env("KEERA_LOG_LEVEL", "info"),
		LogFormat:             env("KEERA_LOG_FORMAT", "text"),
		MaxBodyBytes:          int64(envInt("KEERA_MAX_BODY_BYTES", 32<<20)),
		MaxResponseBytes:      int64(envInt("KEERA_MAX_RESPONSE_BYTES", 64<<20)),
		UpstreamHeaderTimeout: envDuration("KEERA_UPSTREAM_HEADER_TIMEOUT", 2*time.Minute),
		CacheTTL:              envDuration("KEERA_CACHE_TTL", 30*time.Second),
		SpendRefresh:          envDuration("KEERA_SPEND_REFRESH", 10*time.Second),
		UsageRetention:        envDuration("KEERA_USAGE_RETENTION", 0),
		AuditRetention:        envDuration("KEERA_AUDIT_RETENTION", 0),
		SessionGap:            envDuration("KEERA_SESSION_GAP", 0),
		Currency:              env("KEERA_CURRENCY", "CHF"),
		RedisURL:              env("KEERA_REDIS_URL", ""),
		RedisPrefix:           env("KEERA_REDIS_PREFIX", "keera"),
		Sandbox:               sandboxConfig(),
		UI:                    envBool("KEERA_UI", true),
		PublicURL:             strings.TrimRight(env("KEERA_PUBLIC_URL", ""), "/"),
		OIDC:                  oidcProviders(),
		OIDCAdoptByEmail:      envBool("KEERA_OIDC_ADOPT_BY_EMAIL", false),
	}
	c.SecureCookies = envBool("KEERA_SECURE_COOKIES", c.servedOverHTTPS())
	return c, c.validate()
}

// servedOverHTTPS reports whether the public URL or any sign-in redirect is
// https, which is when cookies should be Secure by default.
func (c Config) servedOverHTTPS() bool {
	if strings.HasPrefix(c.PublicURL, "https://") {
		return true
	}
	for _, p := range c.OIDC {
		if strings.HasPrefix(p.RedirectURL, "https://") {
			return true
		}
	}
	return false
}

func (c Config) validate() error {
	if err := c.validateCredentials(); err != nil {
		return err
	}
	if err := c.validateRetention(); err != nil {
		return err
	}
	if err := c.validateSessionGap(); err != nil {
		return err
	}
	if err := c.Sandbox.validate(); err != nil {
		return err
	}
	return c.validateOIDC()
}

func (c Config) validateCredentials() error {
	if c.DatabaseURL == "" {
		return errors.New("KEERA_DATABASE_URL is required")
	}
	if c.OperatorKey == "" {
		return errors.New("KEERA_OPERATOR_KEY is required; generate one with: openssl rand -hex 32")
	}
	if len(c.OperatorKey) < 16 {
		return errors.New("KEERA_OPERATOR_KEY is too short to be a credential")
	}
	if c.MetricsToken == "" {
		return nil
	}
	if len(c.MetricsToken) < 16 {
		return errors.New("KEERA_METRICS_TOKEN is too short to be a credential")
	}
	// The scrape token exists so a Prometheus configuration need not hold the
	// operator key. Using the same value would defeat that.
	if c.MetricsToken == c.OperatorKey {
		return errors.New("KEERA_METRICS_TOKEN must differ from KEERA_OPERATOR_KEY; " +
			"it exists so a scrape configuration does not have to hold the operator key")
	}
	return nil
}

// validateRetention sets a floor of a day. Retention deletes rows for good,
// and a stray "10m" would silently discard the day's billing data.
func (c Config) validateRetention() error {
	for _, r := range []struct {
		name string
		d    time.Duration
	}{{"KEERA_USAGE_RETENTION", c.UsageRetention}, {"KEERA_AUDIT_RETENTION", c.AuditRetention}} {
		if r.d < 0 {
			return fmt.Errorf("%s cannot be negative", r.name)
		}
		if r.d > 0 && r.d < 24*time.Hour {
			return fmt.Errorf("%s is %s; retention deletes rows for good, so the "+
				"shortest window it accepts is 24h", r.name, r.d)
		}
	}
	return nil
}

// validateSessionGap sets a floor of a minute. Below that every request would
// be its own session, which is just the request log.
func (c Config) validateSessionGap() error {
	if c.SessionGap < 0 {
		return errors.New("KEERA_SESSION_GAP cannot be negative")
	}
	if c.SessionGap > 0 && c.SessionGap < time.Minute {
		return fmt.Errorf("KEERA_SESSION_GAP is %s; below a minute every request "+
			"is its own session, which is the request log", c.SessionGap)
	}
	return nil
}

func (c Config) validateOIDC() error {
	seen := map[string]bool{}
	for _, p := range c.OIDC {
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("KEERA_OIDC_PROVIDERS names %q twice", p.Name)
		}
		seen[p.Name] = true
		if !p.Mapping.Default.Valid() {
			return fmt.Errorf("the default role for the %s identity provider "+
				"must be operator, admin or member", p.Name)
		}
	}
	return nil
}

// singleProviderName is the name of the provider the unprefixed KEERA_OIDC_*
// settings configure. A deployment with one directory need not name it, but
// every external ID stores a name; migration 0024 gave the existing ones this.
const singleProviderName = "sso"

// oidcPrefix is where the unprefixed provider's settings live, and where every
// named provider falls back to.
const oidcPrefix = "KEERA_OIDC_"

// oidcProviders reads however many identity providers are configured.
//
// KEERA_OIDC_PROVIDERS lists the names. Provider N reads KEERA_OIDC_<N>_* and
// falls back to the unprefixed setting where sharing one makes sense, such as
// the redirect URL. With no list, the unprefixed settings configure a single
// provider.
func oidcProviders() []authn.OIDCConfig {
	names := envList("KEERA_OIDC_PROVIDERS")
	if len(names) == 0 {
		if env(oidcPrefix+"ISSUER", "") == "" && env(oidcPrefix+"CLIENT_ID", "") == "" {
			return nil
		}
		return []authn.OIDCConfig{providerFrom(oidcPrefix, singleProviderName)}
	}
	out := make([]authn.OIDCConfig, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		out = append(out, providerFrom(oidcPrefix+envSegment(name)+"_", name))
	}
	return out
}

// providerFrom reads one provider's settings from prefix, falling back to the
// unprefixed ones where noted.
func providerFrom(prefix, name string) authn.OIDCConfig {
	shared := func(key, def string) string {
		return env(prefix+key, env(oidcPrefix+key, def))
	}
	sharedList := func(key string) []string {
		if v := envList(prefix + key); len(v) > 0 {
			return v
		}
		return envList(oidcPrefix + key)
	}
	return authn.OIDCConfig{
		Name:        name,
		DisplayName: shared("LABEL", providerLabel(name)),
		// The issuer and client are never shared: two providers on one client
		// would sign people in to the wrong directory instead of failing.
		IssuerURL:    env(prefix+"ISSUER", ""),
		ClientID:     env(prefix+"CLIENT_ID", ""),
		ClientSecret: env(prefix+"CLIENT_SECRET", ""),
		RedirectURL:  shared("REDIRECT_URL", ""),
		Scopes:       sharedList("SCOPES"),
		GroupsClaim:  shared("GROUPS_CLAIM", "groups"),
		Mapping: authn.RoleMapping{
			// Group names usually differ per directory, but may be shared so a
			// deployment using the same names everywhere need not repeat them.
			OperatorGroups: sharedList("OPERATOR_GROUPS"),
			AdminGroups:    sharedList("ADMIN_GROUPS"),
			// An address is the same whichever directory vouched for it, so
			// there is one operator list for the deployment.
			OperatorEmails: envList("KEERA_OPERATORS"),
			Default:        authn.Role(shared("DEFAULT_ROLE", string(authn.RoleMember))),
		},
	}
}

// envSegment turns a provider name into the middle of an environment variable.
func envSegment(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// providerLabel is what the sign-in button says when no label is set. People
// type vendor names but read product names ("Microsoft", not "Entra");
// anything unknown gets its own name capitalised.
func providerLabel(name string) string {
	switch name {
	case "google", "workspace", "gsuite":
		return "Google"
	case "entra", "entraid", "azure", "azuread", "microsoft", "m365":
		return "Microsoft"
	case "okta":
		return "Okta"
	case "keycloak":
		return "Keycloak"
	case singleProviderName:
		return "single sign-on"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// envList reads a comma-separated setting.
func envList(key string) []string {
	raw := env(key, "")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(env(key, ""))
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(env(key, ""))
	if err != nil {
		return def
	}
	return d
}

// APIKey resolves a model's api_key_env against the process environment.
func APIKey(name string) string { return os.Getenv(name) }

/* ------------------------------------------------------------------ sandboxes */

// SandboxConfig holds the sandbox settings, off by default.
//
// A gateway is useful without sandboxes, and one that tried to reach a
// Kubernetes API server on every start would fail over a feature nobody asked
// for. Switching them on is one variable; the rest defaults to what the Helm
// chart installs.
type SandboxConfig struct {
	// Driver is "kubernetes", "podman", or empty for no sandboxes.
	Driver string
	// File declares the sandbox catalogue, applied on every start like the
	// model catalogue.
	File string
	// Namespace is where the Kubernetes driver creates sandboxes. It should
	// not be the gateway's own: a sandbox's API key is in its pod spec, so
	// anyone who can read pods there can read every sandbox's credential.
	Namespace string
	// Runtimes maps an isolation tier to this cluster's or host's name for it.
	// A class asking for a tier with no name is refused rather than quietly
	// given weaker isolation.
	Runtimes map[policy.Isolation]string

	StorageClass     string
	ServiceAccount   string
	ImagePullSecrets []string

	// PublicURL is the gateway as a sandbox sees it, usually the in-cluster
	// Service rather than the ingress. Empty falls back to the deployment's
	// PublicURL, which suits a single host.
	PublicURL string
	// Model is the alias a sandbox's agent is pointed at.
	Model string
	// IdleSuspend is how long a sandbox may sit with nobody attached before it
	// is suspended: the volume kept, the compute released. Zero leaves it
	// running until it expires.
	IdleSuspend time.Duration

	// The Kube settings override the in-cluster defaults, which the Helm
	// chart's install does not need.
	KubeServer    string
	KubeTokenFile string
	KubeCAFile    string
	KubeInsecure  bool

	// Warm switches warm pools on: a few sandboxes of each class kept started
	// so asking for one is instant. It is a separate switch because it needs
	// the agent-sandbox warm-pool extension; without it, every reconcile would
	// fail over a missing CRD.
	Warm bool

	// PodmanBinary and PodmanNetwork configure the single-host driver.
	PodmanBinary  string
	PodmanNetwork string

	// Git is where sandboxes get their repository credentials.
	Git GitConfig
}

// GitConfig names the forge sandboxes check out from, and the deployment's
// own credential there. Secrets are read from files, so they can come from a
// mounted secret and stay out of the process environment.
type GitConfig struct {
	// Forge is "github", "gitlab", or empty for none. With none, a sandbox
	// cannot be created with a repository in it.
	Forge string
	// URL is GitHub's API (https://<host>/api/v3 for Enterprise Server) or the
	// GitLab instance. Empty is GitHub.com or GitLab.com.
	URL string
	// AppID and AppKeyFile are the GitHub App's id and private key.
	AppID      string
	AppKeyFile string
	// TokenFile holds the GitLab token that creates project access tokens.
	TokenFile string
}

// Enabled reports whether this deployment lends out sandboxes.
func (s SandboxConfig) Enabled() bool { return s.Driver != "" }

func sandboxConfig() SandboxConfig {
	return SandboxConfig{
		Driver:           strings.ToLower(env("KEERA_SANDBOX_DRIVER", "")),
		File:             env("KEERA_SANDBOXES_FILE", ""),
		Namespace:        env("KEERA_SANDBOX_NAMESPACE", ""),
		Runtimes:         sandboxRuntimes(),
		StorageClass:     env("KEERA_SANDBOX_STORAGE_CLASS", ""),
		ServiceAccount:   env("KEERA_SANDBOX_SERVICE_ACCOUNT", ""),
		ImagePullSecrets: envList("KEERA_SANDBOX_IMAGE_PULL_SECRETS"),
		PublicURL:        strings.TrimRight(env("KEERA_SANDBOX_PUBLIC_URL", ""), "/"),
		Model:            env("KEERA_SANDBOX_MODEL", ""),
		IdleSuspend:      envDuration("KEERA_SANDBOX_IDLE_SUSPEND", 0),
		KubeServer:       env("KEERA_SANDBOX_KUBE_SERVER", ""),
		KubeTokenFile:    env("KEERA_SANDBOX_KUBE_TOKEN_FILE", ""),
		KubeCAFile:       env("KEERA_SANDBOX_KUBE_CA_FILE", ""),
		KubeInsecure:     envBool("KEERA_SANDBOX_KUBE_INSECURE", false),
		Warm:             envBool("KEERA_SANDBOX_WARM", false),
		PodmanBinary:     env("KEERA_SANDBOX_PODMAN_BINARY", "podman"),
		PodmanNetwork:    env("KEERA_SANDBOX_PODMAN_NETWORK", ""),
		Git: GitConfig{
			Forge:      strings.ToLower(env("KEERA_SANDBOX_GIT_FORGE", "")),
			URL:        env("KEERA_SANDBOX_GIT_URL", ""),
			AppID:      env("KEERA_SANDBOX_GIT_APP_ID", ""),
			AppKeyFile: env("KEERA_SANDBOX_GIT_APP_KEY_FILE", ""),
			TokenFile:  env("KEERA_SANDBOX_GIT_TOKEN_FILE", ""),
		},
	}
}

// sandboxRuntimes reads one variable per tier rather than one list, so the
// order of a list cannot silently map the wrong tier.
func sandboxRuntimes() map[policy.Isolation]string {
	out := map[policy.Isolation]string{}
	for _, tier := range policy.Isolations {
		if v := env("KEERA_SANDBOX_RUNTIME_"+strings.ToUpper(string(tier)), ""); v != "" {
			out[tier] = v
		}
	}
	return out
}

func (s SandboxConfig) validate() error {
	switch s.Driver {
	case "", "kubernetes", "podman":
	default:
		return fmt.Errorf("KEERA_SANDBOX_DRIVER is %q; it is 'kubernetes', 'podman', or unset "+
			"for a deployment that lends out no sandboxes", s.Driver)
	}
	// The rest configures a driver. A stray setting with sandboxes off does
	// nothing, so it must not stop the gateway from starting.
	if !s.Enabled() {
		return nil
	}
	if s.IdleSuspend < 0 {
		return errors.New("KEERA_SANDBOX_IDLE_SUSPEND cannot be negative")
	}
	// A floor of five minutes: an attached editor sends nothing while its user
	// reads, and suspending a sandbox in use is the worst thing this can do.
	if s.IdleSuspend > 0 && s.IdleSuspend < 5*time.Minute {
		return fmt.Errorf("KEERA_SANDBOX_IDLE_SUSPEND is %s; below five minutes it would "+
			"suspend a sandbox somebody is still working in, because an attached editor "+
			"sends nothing while its user is reading", s.IdleSuspend)
	}
	return s.Git.validate()
}

func (g GitConfig) validate() error {
	switch g.Forge {
	case "":
		return nil
	case "github":
		if g.AppID == "" || g.AppKeyFile == "" {
			return errors.New("KEERA_SANDBOX_GIT_FORGE is github, which needs " +
				"KEERA_SANDBOX_GIT_APP_ID and KEERA_SANDBOX_GIT_APP_KEY_FILE")
		}
	case "gitlab":
		if g.TokenFile == "" {
			return errors.New("KEERA_SANDBOX_GIT_FORGE is gitlab, which needs " +
				"KEERA_SANDBOX_GIT_TOKEN_FILE")
		}
	default:
		return fmt.Errorf("KEERA_SANDBOX_GIT_FORGE is %q; it is 'github', 'gitlab', or unset", g.Forge)
	}
	return nil
}
