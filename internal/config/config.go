// Package config reads the gateway's settings from the environment. The model
// and sandbox files are templates that each new organisation starts from.
package config

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
	"github.com/redis/go-redis/v9"
)

// DefaultTrustedProxies is loopback and the private ranges, where a proxy in
// front usually is.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

// Config is the gateway's whole configuration.
type Config struct {
	DatabaseURL string
	MaxDBConns  int32

	// Addr is the one listener. The panel is served at /, inference and MCP
	// under /api, the control API under /control and the sandbox attach
	// surface under /sandbox.
	Addr string

	// OperatorKey authenticates the control API. It is not stored in the
	// database, because it is how you reach an empty one.
	OperatorKey string

	// SecretKey encrypts credentials stored through the control plane, such as
	// a hosted model's API key.
	SecretKey string
	// SecretKeyPrevious is the key SecretKey replaced, while credentials move.
	SecretKeyPrevious string

	// MetricsToken reads /metrics and nothing else, so a scraper needs no
	// operator key. Empty leaves the route to operators.
	MetricsToken string

	// ModelsFile declares the models each new organisation starts with.
	ModelsFile string

	LogLevel  string
	LogFormat string

	MaxBodyBytes          int64
	MaxResponseBytes      int64
	UpstreamHeaderTimeout time.Duration
	// UpstreamDeny is the addresses the gateway never sends a request to:
	// loopback and the cloud metadata service by default. See docs/install.md.
	UpstreamDeny []netip.Prefix
	// LimitPrivate keeps organisations from addresses inside the network,
	// except through the host names in PrivateHosts. KEERA_UPSTREAM_PRIVATE
	// sets both.
	LimitPrivate bool
	PrivateHosts []string
	CacheTTL     time.Duration
	SpendRefresh time.Duration

	// UsageRetention and AuditRetention are how long usage events and audit
	// entries are kept. Ended sandboxes and keys go with usage. Zero, the
	// default, keeps them for ever. See docs/install.md.
	UsageRetention time.Duration
	AuditRetention time.Duration

	// SessionGap is how long an agent conversation may go quiet before the
	// next request starts a new task. Changing it re-cuts past sessions too.
	// See docs/sessions.md.
	SessionGap time.Duration

	// Platform is the deployment's own provider keys, which organisations
	// use and are billed for. See docs/billing.md.
	Platform catalog.Platform
	// Payments is how organisations pay for those keys in advance. Nil
	// without a PostFinance Checkout account.
	Payments *Payments
	// ClaudeSubscriptions lets Claude Code signed in to a Claude plan through,
	// paid by that plan. Off by default. See docs/subscriptions.md.
	ClaudeSubscriptions bool

	// Redis is where replicas share their rate limits, from KEERA_REDIS_URL.
	// Nil, the default, keeps limits per process, so N replicas allow N times
	// the limit. See docs/gateway.md.
	Redis *redis.Options
	// RedisPrefix namespaces the keys, so two deployments can share one Redis.
	RedisPrefix string

	// TrustedProxies are the proxies in front of the gateway, whose
	// X-Forwarded-For says where a request came from. See docs/install.md.
	TrustedProxies []netip.Prefix

	// UI serves the control panel from the listener's root.
	UI bool
	// PublicURL is the gateway's origin as a browser sees it, for the
	// redirect after sign-out, "My clients" and the panel link in a refusal.
	// Empty uses the Host header for the first two and leaves the link out.
	PublicURL string
	// ChatURL is Keera Chat's origin. A sign-in started there may hand back
	// to its /auth/callback. Empty allows only loopback. See docs/sso.md.
	ChatURL string
	// SecureCookies marks the session cookie Secure. On by default for an
	// https public URL; off lets a plain-http demo on localhost sign in.
	SecureCookies bool

	// Sandbox is the machines this deployment lends out. Off by default.
	Sandbox SandboxConfig

	// OIDC is every identity provider, in the order the sign-in screen shows
	// them. With none and no passkeys, the operator key is the only way in.
	OIDC []authn.OIDCConfig
	// Passkeys lets administrators create accounts that sign in with a
	// passkey instead of a directory. Nil when off, the default, as such an
	// account outlives its owner leaving the directory. See docs/sso.md.
	Passkeys *authn.RelyingParty
}

// Load reads the environment.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:           env("KEERA_DATABASE_URL", ""),
		MaxDBConns:            int32(min(envInt("KEERA_MAX_DB_CONNS", 16), math.MaxInt32)),
		Addr:                  Addr(),
		OperatorKey:           env("KEERA_OPERATOR_KEY", ""),
		SecretKey:             env("KEERA_SECRET_KEY", ""),
		SecretKeyPrevious:     env("KEERA_SECRET_KEY_PREVIOUS", ""),
		MetricsToken:          env("KEERA_METRICS_TOKEN", ""),
		ModelsFile:            env("KEERA_MODELS_FILE", ""),
		LogLevel:              strings.ToLower(env("KEERA_LOG_LEVEL", "info")),
		LogFormat:             strings.ToLower(env("KEERA_LOG_FORMAT", "text")),
		MaxBodyBytes:          int64(envInt("KEERA_MAX_BODY_BYTES", gateway.DefaultMaxBodyBytes)),
		MaxResponseBytes:      int64(envInt("KEERA_MAX_RESPONSE_BYTES", gateway.DefaultMaxResponseBytes)),
		UpstreamHeaderTimeout: envDuration("KEERA_UPSTREAM_HEADER_TIMEOUT", gateway.DefaultUpstreamHeaderTimeout),
		CacheTTL:              envDuration("KEERA_CACHE_TTL", registry.DefaultTTL),
		SpendRefresh:          envDuration("KEERA_SPEND_REFRESH", registry.DefaultSpendRefresh),
		UsageRetention:        envDuration("KEERA_USAGE_RETENTION", 0),
		AuditRetention:        envDuration("KEERA_AUDIT_RETENTION", 0),
		SessionGap:            envDuration("KEERA_SESSION_GAP", store.DefaultSessionGap),
		RedisPrefix:           env("KEERA_REDIS_PREFIX", ratelimit.DefaultRedisPrefix),
		Sandbox:               sandboxConfig(),
		UI:                    envBool("KEERA_UI", true),
		ClaudeSubscriptions:   envBool("KEERA_CLAUDE_SUBSCRIPTIONS", false),
		PublicURL:             strings.TrimRight(env("KEERA_PUBLIC_URL", ""), "/"),
		ChatURL:               strings.TrimRight(env("KEERA_CHAT_URL", ""), "/"),
	}
	c.OIDC = oidcProviders(c.PublicURL)
	c.SecureCookies = envBool("KEERA_SECURE_COOKIES", c.servedOverHTTPS())
	if err := c.validate(); err != nil {
		return c, err
	}
	// A passkey is bound to the public URL's host, which must support
	// WebAuthn.
	if envBool("KEERA_PASSKEYS", false) {
		rp, err := authn.NewRelyingParty(c.PublicURL)
		if err != nil {
			return c, fmt.Errorf("KEERA_PASSKEYS: %w", err)
		}
		c.Passkeys = rp
	}
	var err error
	if c.UpstreamDeny, err = envPrefixes("KEERA_UPSTREAM_DENY", gateway.DefaultUpstreamDeny); err != nil {
		return c, err
	}
	if c.TrustedProxies, err = envPrefixes("KEERA_TRUSTED_PROXIES", DefaultTrustedProxies); err != nil {
		return c, err
	}
	if c.LimitPrivate, c.PrivateHosts, err = c.privateHosts(); err != nil {
		return c, err
	}
	if c.Platform, err = platformKeys(); err != nil {
		return c, err
	}
	if c.Payments, err = payments(c.Platform, c.PublicURL); err != nil {
		return c, err
	}
	// Parsed here, so a typo stops the start before the database is touched.
	if u := env("KEERA_REDIS_URL", ""); u != "" {
		opt, err := redis.ParseURL(u)
		if err != nil {
			return c, fmt.Errorf("KEERA_REDIS_URL: %w", err)
		}
		c.Redis = opt
	}
	return c, nil
}

// servedOverHTTPS reports whether the public URL is https.
func (c Config) servedOverHTTPS() bool {
	return strings.HasPrefix(c.PublicURL, "https://")
}

func (c Config) validate() error {
	for _, check := range []func() error{
		c.validateCredentials, c.validateLogging,
		c.validateDBConns, c.validateRetention, c.validateSessionGap, c.Sandbox.validate, c.validateOIDC,
		c.validateChatURL,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
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
	if c.SecretKey == "" {
		return errors.New("KEERA_SECRET_KEY is required; generate one with: openssl rand -hex 32")
	}
	if len(c.SecretKey) < 16 {
		return errors.New("KEERA_SECRET_KEY is too short to be a key; generate one with: openssl rand -hex 32")
	}
	if c.SecretKeyPrevious != "" && len(c.SecretKeyPrevious) < 16 {
		return errors.New("KEERA_SECRET_KEY_PREVIOUS is too short to be a key; it is the KEERA_SECRET_KEY " +
			"this one replaced")
	}
	if c.MetricsToken == "" {
		return nil
	}
	if len(c.MetricsToken) < 16 {
		return errors.New("KEERA_METRICS_TOKEN is too short to be a credential")
	}
	if c.MetricsToken == c.OperatorKey {
		return errors.New("KEERA_METRICS_TOKEN must differ from KEERA_OPERATOR_KEY; " +
			"it exists so a scrape configuration does not have to hold the operator key")
	}
	return nil
}

// validateLogging refuses an unknown log setting rather than quietly ignore it.
func (c Config) validateLogging() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("KEERA_LOG_LEVEL is %q; it is 'debug', 'info', 'warn' or 'error'", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("KEERA_LOG_FORMAT is %q; it is 'text' or 'json'", c.LogFormat)
	}
	return nil
}

// minDBConns is the smallest pool that can serve, leaving room for a
// transaction while the usage recorder and requests still get a connection.
// The two listeners use their own connections (see store.Listen).
const minDBConns = 4

func (c Config) validateDBConns() error {
	if c.MaxDBConns < minDBConns {
		return fmt.Errorf("KEERA_MAX_DB_CONNS is %d; the smallest pool that can serve "+
			"requests is %d", c.MaxDBConns, minDBConns)
	}
	return nil
}

// validateRetention sets a floor of a day, so a stray "10m" cannot delete the
// day's billing data.
func (c Config) validateRetention() error {
	for _, r := range []struct {
		name string
		d    time.Duration
	}{
		{"KEERA_USAGE_RETENTION", c.UsageRetention},
		{"KEERA_AUDIT_RETENTION", c.AuditRetention},
	} {
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

// validateSessionGap sets a floor of a minute. Below that, every request would
// be its own session.
func (c Config) validateSessionGap() error {
	if c.SessionGap < time.Minute {
		return fmt.Errorf("KEERA_SESSION_GAP is %s; below a minute every request "+
			"is its own session, which is the request log", c.SessionGap)
	}
	return nil
}

// validateChatURL accepts a bare origin: sign-ins go to one fixed path under it.
func (c Config) validateChatURL() error {
	if c.ChatURL == "" {
		return nil
	}
	u, err := url.Parse(c.ChatURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("KEERA_CHAT_URL must be the address a browser reaches Keera Chat on, "+
			"without a path, such as https://chat.example.ch, not %q", c.ChatURL)
	}
	return nil
}

func (c Config) validateOIDC() error {
	// The identity provider sends the browser back to a callback under it.
	if len(c.OIDC) > 0 && c.PublicURL == "" {
		return errors.New("set KEERA_PUBLIC_URL to the address a browser reaches the " +
			"panel on; single sign-on returns to its /control/auth/callback")
	}
	seen := map[string]bool{}
	for _, p := range c.OIDC {
		if seen[p.Name] {
			return fmt.Errorf("KEERA_OIDC_PROVIDERS names %q twice", p.Name)
		}
		seen[p.Name] = true
	}
	for _, p := range c.OIDC {
		// Otherwise a typo only shows as a missing sign-in button.
		if !p.Configured() {
			return fmt.Errorf("KEERA_OIDC_PROVIDERS names %q, but %sISSUER is not set, "+
				"or %sCLIENT_ID for an OpenID Connect provider", p.Name, OIDCEnvPrefix(p.Name),
				OIDCEnvPrefix(p.Name))
		}
		if err := requiredSettings(p); err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		// Not operator: that role comes only from KEERA_OPERATORS or an
		// operator group, so those show everyone who holds it.
		if !p.Mapping.Default.Assignable() {
			return fmt.Errorf("the default role for the %s identity provider "+
				"must be admin or member", p.Name)
		}
		if p.IsLDAP() {
			if err := ldapVouching(p); err != nil {
				return err
			}
		}
		// Otherwise, with several directories, one could give its users
		// another customer's addresses, or an operator's.
		if len(c.OIDC) > 1 && len(p.Domains) == 0 {
			return fmt.Errorf("set %sDOMAINS to the email domains the %s "+
				"identity provider may vouch for, or to %q for one that proves every "+
				"address itself, such as Google", OIDCEnvPrefix(p.Name), p.Name, authn.AnyDomain)
		}
		// Any GitHub account can sign in to an OAuth App, and would land in
		// the one organisation.
		if p.IsGitHub() && len(p.Domains) == 0 {
			return fmt.Errorf("set %sDOMAINS: any GitHub account can sign in, so name the "+
				"email domains of the people who may, or %q where people sign up",
				OIDCEnvPrefix(p.Name), authn.AnyDomain)
		}
	}
	return c.validateSignUp()
}

// requiredSettings names the settings a provider is missing. Only config knows
// their names.
func requiredSettings(p authn.OIDCConfig) error {
	prefix := OIDCEnvPrefix(p.Name)
	var missing []string
	need := func(value, setting string) {
		if value == "" {
			missing = append(missing, prefix+setting)
		}
	}
	if p.IsLDAP() {
		need(p.LDAP.BindDN, "LDAP_BIND_DN")
		need(p.LDAP.BindPassword, "LDAP_BIND_PASSWORD")
		need(p.LDAP.BaseDN, "LDAP_BASE_DN")
	} else {
		need(p.ClientSecret, "CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the %s identity provider needs %s", p.Name, strings.Join(missing, " and "))
	}
	return nil
}

// ldapVouching refuses what is unsafe for a directory: its administrator sets
// each address, so it may only vouch for its own domains, and not for sign-up.
func ldapVouching(p authn.OIDCConfig) error {
	prefix := OIDCEnvPrefix(p.Name)
	if len(p.Domains) == 0 || slices.Contains(p.Domains, authn.AnyDomain) {
		return fmt.Errorf("set %sDOMAINS to the email domains the %s directory may vouch "+
			"for; its administrator sets every address, so %q is not allowed",
			prefix, p.Name, authn.AnyDomain)
	}
	if p.SignUp {
		return fmt.Errorf("%sSIGNUP cannot be used with an LDAP directory, which does not "+
			"prove an address to someone it does not know", prefix)
	}
	return nil
}

// validateSignUp refuses sign-up where an admin group decides who is an
// administrator: whoever created an organisation would lose the role at their
// next sign-in, leaving it with no administrator.
func (c Config) validateSignUp() error {
	var signUp, group string
	for _, p := range c.OIDC {
		if p.SignUp && signUp == "" {
			signUp = p.Name
		}
		if p.Mapping.DecidesAdmin() && group == "" {
			group = p.Name
		}
	}
	if signUp != "" && group != "" {
		return fmt.Errorf("%sSIGNUP and %sADMIN_GROUPS cannot both be set: an admin group "+
			"decides the administrator role for the whole gateway, so whoever creates an "+
			"organisation would lose it at their next sign-in", OIDCEnvPrefix(signUp),
			OIDCEnvPrefix(group))
	}
	return nil
}

// privateHosts reads KEERA_UPSTREAM_PRIVATE. It must be set where strangers can
// sign up: by default an organisation reaches the whole private network.
func (c Config) privateHosts() (bool, []string, error) {
	raw := env("KEERA_UPSTREAM_PRIVATE", "")
	if raw == "" {
		for _, p := range c.OIDC {
			if p.SignUp {
				return false, nil, fmt.Errorf("%sSIGNUP lets anyone create an organisation, "+
					"so set KEERA_UPSTREAM_PRIVATE to the hosts inside this network they may "+
					"reach, such as keera-engine,keera-decider, or to none or all", OIDCEnvPrefix(p.Name))
			}
		}
		return false, nil, nil
	}
	limit, hosts, err := gateway.ParsePrivateHosts(raw)
	if err != nil {
		return false, nil, fmt.Errorf("KEERA_UPSTREAM_PRIVATE: %w", err)
	}
	return limit, hosts, nil
}

// oidcProviders reads the identity providers KEERA_OIDC_PROVIDERS names.
// Provider N reads KEERA_OIDC_<N>_*, and nothing is shared between them.
//
// One callback serves every provider: a sign-in is recognised by its state.
func oidcProviders(publicURL string) []authn.OIDCConfig {
	redirect := ""
	if publicURL != "" {
		redirect = publicURL + httpx.ControlPrefix + "/auth/callback"
	}
	// One operator list for all directories: an address is the same in each.
	operators := envList("KEERA_OPERATORS")
	var out []authn.OIDCConfig
	for _, name := range envList("KEERA_OIDC_PROVIDERS") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		out = append(out, providerFrom(OIDCEnvPrefix(name), name, redirect, operators))
	}
	return out
}

// providerFrom reads one provider's settings from prefix.
func providerFrom(prefix, name, redirect string, operators []string) authn.OIDCConfig {
	issuer := env(prefix+"ISSUER", "")
	// A directory's groups are DNs, which hold commas themselves.
	groupSep := ","
	if (authn.OIDCConfig{IssuerURL: issuer}).IsLDAP() {
		groupSep = ";"
	}
	return authn.OIDCConfig{
		Name:         name,
		DisplayName:  env(prefix+"LABEL", providerLabel(name)),
		IssuerURL:    issuer,
		ClientID:     env(prefix+"CLIENT_ID", ""),
		ClientSecret: env(prefix+"CLIENT_SECRET", ""),
		RedirectURL:  redirect,
		Scopes:       envList(prefix + "SCOPES"),
		GroupsClaim:  env(prefix+"GROUPS_CLAIM", ""),
		Mapping: authn.RoleMapping{
			OperatorGroups: envListBy(prefix+"OPERATOR_GROUPS", groupSep),
			AdminGroups:    envListBy(prefix+"ADMIN_GROUPS", groupSep),
			OperatorEmails: operators,
			Default: authn.Role(strings.ToLower(
				env(prefix+"DEFAULT_ROLE", string(authn.RoleMember)))),
		},
		Domains: envList(prefix + "DOMAINS"),
		SignUp:  envBool(prefix+"SIGNUP", false),
		LDAP: authn.LDAPConfig{
			BindDN:          env(prefix+"LDAP_BIND_DN", ""),
			BindPassword:    env(prefix+"LDAP_BIND_PASSWORD", ""),
			BaseDN:          env(prefix+"LDAP_BASE_DN", ""),
			UserFilter:      env(prefix+"LDAP_USER_FILTER", ""),
			LoginAttributes: envList(prefix + "LDAP_LOGIN_ATTRIBUTES"),
			CAFile:          env(prefix+"LDAP_CA_FILE", ""),
		},
	}
}

// OIDCEnvPrefix is how every setting of the named provider starts:
// "acme-ad" reads KEERA_OIDC_ACME_AD_*.
func OIDCEnvPrefix(name string) string { return envPrefix("OIDC", name) }

// envPrefix is how the settings of one named item start, such as
// KEERA_PROVIDER_STEPPING_STONE_.
func envPrefix(group, name string) string {
	return "KEERA_" + group + "_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
}

// providerLabel is the sign-in button's text when no label is set: a product
// name ("Microsoft", not "Entra"), or the name capitalised.
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
	case "github":
		return "GitHub"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// envBool reads a switch. An unreadable value is ignored, so a typo cannot turn
// a safety setting off.
func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// envList reads a comma-separated setting.
func envList(key string) []string { return envListBy(key, ",") }

// envPrefixes reads a list of addresses and prefixes, or "none".
func envPrefixes(key, def string) ([]netip.Prefix, error) {
	prefixes, err := gateway.ParsePrefixes(env(key, def))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return prefixes, nil
}

// envListBy reads a setting whose items are separated by sep.
func envListBy(key, sep string) []string {
	raw := env(key, "")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Addr is KEERA_ADDR alone, for `keera-gateway health`, which needs nothing
// else.
func Addr() string { return env("KEERA_ADDR", ":8080") }

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envInt reads a size or a count. Zero or less means the default: none of
// them means "unlimited".
func envInt(key string, def int) int {
	n, err := strconv.Atoi(env(key, ""))
	if err != nil || n <= 0 {
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

// SandboxConfig holds the sandbox settings, off by default. Switching them on
// is one variable; the rest defaults to what the Helm chart installs.
type SandboxConfig struct {
	// Driver is "kubernetes", "podman", or empty for no sandboxes.
	Driver string
	// File declares the sandbox classes each new organisation starts with.
	File string
	// Namespace is where the Kubernetes driver creates sandboxes. Not the
	// gateway's own: a sandbox's API key is in its pod spec, readable by anyone
	// who can read pods there.
	Namespace string
	// Runtimes maps an isolation tier to this cluster's or host's name for it.
	// A class asking for an unmapped tier is refused, not given weaker
	// isolation.
	Runtimes map[policy.Isolation]string

	StorageClass     string
	ServiceAccount   string
	ImagePullSecrets []string
	// Images are the image prefixes an organisation's administrator may give
	// a class, besides the template's images. Others need an operator, as
	// pods pull with the deployment's credentials.
	Images []string

	// PublicURL is the gateway as a sandbox sees it, usually the in-cluster
	// Service. Empty uses the deployment's PublicURL, which suits one host.
	PublicURL string
	// IdleSuspend is how long a sandbox may sit with nobody attached before it
	// is suspended, keeping the volume. Zero leaves it running until it
	// expires.
	IdleSuspend time.Duration

	// Warm keeps a few sandboxes of each class started, so asking for one is
	// instant. It needs the agent-sandbox warm-pool extension.
	Warm bool

	// PodmanBinary and PodmanNetwork configure the single-host driver.
	PodmanBinary  string
	PodmanNetwork string

	// Git is where sandboxes get their repository credentials.
	Git GitConfig
}

// GitConfig names the forge sandboxes check out from, and the deployment's
// credential there. Secrets are read from files, to keep them out of the
// environment.
type GitConfig struct {
	// Forge is "github", "gitlab", or empty for none, which allows no
	// repository in a sandbox.
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
		Images:           envList("KEERA_SANDBOX_IMAGES"),
		PublicURL:        strings.TrimRight(env("KEERA_SANDBOX_PUBLIC_URL", ""), "/"),
		IdleSuspend:      envDuration("KEERA_SANDBOX_IDLE_SUSPEND", 0),
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

// sandboxRuntimes reads one variable per tier, not a list, so order cannot map
// the wrong tier.
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
	// The rest configures a driver, so with sandboxes off a stray setting must
	// not stop the start. The classes file is read either way.
	if !s.Enabled() {
		return nil
	}
	if s.IdleSuspend < 0 {
		return errors.New("KEERA_SANDBOX_IDLE_SUSPEND cannot be negative")
	}
	// A short timer could suspend a sandbox in use, or a laptop reconnecting.
	if s.IdleSuspend > 0 && s.IdleSuspend < 5*time.Minute {
		return fmt.Errorf("KEERA_SANDBOX_IDLE_SUSPEND is %s; below five minutes it could "+
			"suspend a sandbox somebody is still connected to, because an open connection "+
			"is only recorded once a minute", s.IdleSuspend)
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
