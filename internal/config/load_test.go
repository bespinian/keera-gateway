package config

import (
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Load is where a deployment's mistakes are meant to be caught, and most of
// what it does is refuse. Every floor below has a comment in config.go saying
// why it is there - retention deletes rows for good, a five-minute idle suspend
// would suspend a sandbox somebody is typing into - and a floor whose reason
// lives only in a comment is a floor that comes back as a default the next time
// this function is tidied.
//
// The other half is the settings nothing names: the secure-cookie flag is
// inferred rather than configured, so no deployment will ever notice it
// flipping to insecure.

// loadWith runs Load against exactly the environment it is given.
//
// The KEERA_ variables of whoever is running the tests are cleared first.
// Otherwise a developer with a gateway configured in their shell - which is
// every developer who has run `make dev` - would get different results from
// the same test, and the failure would look like a bug in this package.
//
// Clearing is done by setting them empty rather than by unsetting: env() trims
// and treats blank as absent, so the two are the same thing here, and t.Setenv
// puts every one of them back when the test ends.
func loadWith(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "KEERA_") {
			t.Setenv(k, "")
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

// valid is the smallest environment Load accepts, for tests that are about one
// setting and need the other required ones out of the way.
func valid(extra map[string]string) map[string]string {
	env := map[string]string{
		"KEERA_DATABASE_URL": "postgres://keera@127.0.0.1/keera",
		"KEERA_OPERATOR_KEY": "an-operator-key-long-enough",
		"KEERA_SECRET_KEY":   "a-secret-key-long-enough",
	}
	maps.Copy(env, extra)
	return env
}

func TestLoadDefaultsAreWhatTheDocumentationSays(t *testing.T) {
	// A deployment that sets only the two required variables gets a working
	// gateway. These are the numbers the install guide quotes, so they are the
	// numbers somebody plans a deployment around.
	c, err := loadWith(t, valid(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name      string
		got, want any
	}{
		{"listener", c.Addr, ":8080"},
		{"database connections", c.MaxDBConns, int32(16)},
		{"log level", c.LogLevel, "info"},
		{"log format", c.LogFormat, "text"},
		{"request body ceiling", c.MaxBodyBytes, int64(32 << 20)},
		{"response ceiling", c.MaxResponseBytes, int64(64 << 20)},
		{"upstream header timeout", c.UpstreamHeaderTimeout, 2 * time.Minute},
		{"cache lifetime", c.CacheTTL, 30 * time.Second},
		{"spend refresh", c.SpendRefresh, 10 * time.Second},
		{"currency", c.Currency, "CHF"},
		{"redis key prefix", c.RedisPrefix, "keera"},
		// Retention off by default: a gateway must not start quietly deleting
		// the billing history of a deployment that never asked it to.
		{"usage retention", c.UsageRetention, time.Duration(0)},
		{"audit retention", c.AuditRetention, time.Duration(0)},
		{"session gap", c.SessionGap, 30 * time.Minute},
		// The panel is the reason most deployments have a browser pointed at
		// this process at all, so it is on unless switched off.
		{"the panel", c.UI, true},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	// No Redis configured is the single-replica shape, and it is the default:
	// requiring one would make a coordination layer a dependency of a demo.
	if c.Redis != nil {
		t.Errorf("Redis = %+v, want nil: Redis is opt-in", c.Redis)
	}
}

func TestARedisURLIsParsedOnce(t *testing.T) {
	// The limiter takes these options as they are, so Load is the only parse.
	c, err := loadWith(t, valid(map[string]string{"KEERA_REDIS_URL": "redis://cache:6380/2"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Redis == nil || c.Redis.Addr != "cache:6380" || c.Redis.DB != 2 {
		t.Errorf("Redis = %+v, want cache:6380, database 2", c.Redis)
	}
}

func TestACountOfZeroIsTheDefault(t *testing.T) {
	// Zero would otherwise reach pgx, which reads it as "choose for me".
	c, err := loadWith(t, valid(map[string]string{"KEERA_MAX_DB_CONNS": "0"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxDBConns != 16 {
		t.Errorf("MaxDBConns = %d, want the documented 16", c.MaxDBConns)
	}
}

func TestLoadNeedsTheTwoThingsItCannotInvent(t *testing.T) {
	// A database it can reach, a credential for the control plane and a key
	// for stored credentials. None has a sensible default, and a gateway that
	// started without the first two would serve nothing and let anybody
	// administer it.
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "no database",
			env:  map[string]string{"KEERA_OPERATOR_KEY": "an-operator-key-long-enough"},
			want: "KEERA_DATABASE_URL is required",
		},
		{
			name: "no operator key",
			env:  map[string]string{"KEERA_DATABASE_URL": "postgres://keera@127.0.0.1/keera"},
			want: "KEERA_OPERATOR_KEY is required",
		},
		{
			name: "an operator key somebody could guess",
			env: valid(map[string]string{
				"KEERA_OPERATOR_KEY": "keera",
			}),
			want: "too short to be a credential",
		},
		{
			name: "no secret key",
			env: valid(map[string]string{
				"KEERA_SECRET_KEY": "",
			}),
			want: "KEERA_SECRET_KEY is required",
		},
		{
			name: "a secret key somebody could guess",
			env:  valid(map[string]string{"KEERA_SECRET_KEY": "keera"}),
			want: "KEERA_SECRET_KEY is too short",
		},
		{
			// Caught here, before the database is migrated, not when the
			// limiter is built.
			name: "a Redis URL that is not one",
			env:  valid(map[string]string{"KEERA_REDIS_URL": "localhost:6379"}),
			want: "KEERA_REDIS_URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.env)
			if err == nil {
				t.Fatal("Load accepted it; a gateway must not start like this")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTheScrapeTokenCannotBeTheOperatorKey(t *testing.T) {
	// The whole point of a separate scrape token is that a Prometheus
	// configuration does not have to hold the operator key. The same value
	// under two names would leave it holding one and looking like it did not.
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_METRICS_TOKEN": "an-operator-key-long-enough",
	}))
	if err == nil {
		t.Fatal("Load accepted a scrape token equal to the operator key")
	}
	if !strings.Contains(err.Error(), "must differ") {
		t.Errorf("error = %q, want it to say the two must differ", err)
	}
}

func TestAScrapeTokenIsHeldToTheSameLengthAsTheOperatorKey(t *testing.T) {
	// It reads a route that is labelled by organisation, so it is a credential
	// and not a formality.
	_, err := loadWith(t, valid(map[string]string{"KEERA_METRICS_TOKEN": "short"}))
	if err == nil || !strings.Contains(err.Error(), "too short to be a credential") {
		t.Fatalf("error = %v, want a refusal naming the length", err)
	}

	c, err := loadWith(t, valid(map[string]string{
		"KEERA_METRICS_TOKEN": "a-scrape-token-long-enough",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MetricsToken != "a-scrape-token-long-enough" {
		t.Errorf("MetricsToken = %q, want the token that was set", c.MetricsToken)
	}
}

func TestRetentionIsRefusedBelowADay(t *testing.T) {
	// Retention deletes rows that cannot be recovered and every plausible value
	// is measured in months, so a stray "10m" - which is a plausible typo for
	// "10mo" - would silently discard the day's billing data.
	for _, name := range []string{"KEERA_USAGE_RETENTION", "KEERA_AUDIT_RETENTION"} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadWith(t, valid(map[string]string{name: "-1h"})); err == nil {
				t.Error("a negative retention was accepted")
			}

			_, err := loadWith(t, valid(map[string]string{name: "10m"}))
			if err == nil {
				t.Fatal("ten minutes of retention was accepted")
			}
			if !strings.Contains(err.Error(), "24h") {
				t.Errorf("error = %q, want it to name the shortest window it takes", err)
			}

			// The boundary itself is allowed, and zero stays "keep everything"
			// rather than "keep nothing".
			for _, ok := range []string{"24h", "0"} {
				c, err := loadWith(t, valid(map[string]string{name: ok}))
				if err != nil {
					t.Fatalf("%s = %s: %v", name, ok, err)
				}
				want, _ := time.ParseDuration(ok)
				got := c.UsageRetention
				if name == "KEERA_AUDIT_RETENTION" {
					got = c.AuditRetention
				}
				if got != want {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
		})
	}
}

func TestASessionGapBelowAMinuteIsTheRequestLog(t *testing.T) {
	// Sessions exist to group a person's requests into tasks. A gap under a
	// minute reports every request as its own task, which is the report this
	// one was added to replace.
	_, err := loadWith(t, valid(map[string]string{"KEERA_SESSION_GAP": "30s"}))
	if err == nil {
		t.Fatal("a thirty-second session gap was accepted")
	}
	if !strings.Contains(err.Error(), "request log") {
		t.Errorf("error = %q, want it to say what such a report would be", err)
	}

	if _, err := loadWith(t, valid(map[string]string{"KEERA_SESSION_GAP": "-1s"})); err == nil {
		t.Error("a negative session gap was accepted")
	}

	// Unlike retention this deletes nothing, so the floor is all it needs.
	if _, err := loadWith(t, valid(map[string]string{"KEERA_SESSION_GAP": "1m"})); err != nil {
		t.Errorf("a one-minute gap was refused: %v", err)
	}
}

func TestSandboxSettingsAreOnlyCheckedWhenSandboxesAreOn(t *testing.T) {
	// A deployment that lends out no sandboxes has no driver, and refusing to
	// start it over a variable that does nothing would turn a leftover line in
	// an .env into an outage.
	c, err := loadWith(t, valid(map[string]string{"KEERA_SANDBOX_IDLE_SUSPEND": "1s"}))
	if err != nil {
		t.Fatalf("a stray sandbox setting stopped a gateway with sandboxes off: %v", err)
	}
	if c.Sandbox.Enabled() {
		t.Error("sandboxes are enabled with no driver configured")
	}

	// With a driver it is checked, because now it decides something. A few
	// seconds would suspend a sandbox whose user is reading a diff, which sends
	// nothing over an attached ssh session.
	_, err = loadWith(t, valid(map[string]string{
		"KEERA_SANDBOX_DRIVER":       "podman",
		"KEERA_SANDBOX_IDLE_SUSPEND": "1s",
	}))
	if err == nil {
		t.Fatal("a one-second idle suspend was accepted for a live driver")
	}
	if !strings.Contains(err.Error(), "five minutes") {
		t.Errorf("error = %q, want it to name the floor", err)
	}
}

func TestAForgeNeedsItsCredential(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"KEERA_SANDBOX_GIT_FORGE": "gitea"}, "'github', 'gitlab'"},
		{map[string]string{"KEERA_SANDBOX_GIT_FORGE": "github"}, "KEERA_SANDBOX_GIT_APP_KEY_FILE"},
		{map[string]string{"KEERA_SANDBOX_GIT_FORGE": "gitlab"}, "KEERA_SANDBOX_GIT_TOKEN_FILE"},
	} {
		tc.env["KEERA_SANDBOX_DRIVER"] = "podman"
		_, err := loadWith(t, valid(tc.env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error = %v, want it to name %s", tc.env, err, tc.want)
		}
	}
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_SANDBOX_DRIVER":           "podman",
		"KEERA_SANDBOX_GIT_FORGE":        "GitLab",
		"KEERA_SANDBOX_GIT_URL":          "https://gitlab.example.ch",
		"KEERA_SANDBOX_GIT_TOKEN_FILE":   "/run/secrets/gitlab",
		"KEERA_SANDBOX_GIT_APP_KEY_FILE": "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if g := c.Sandbox.Git; g.Forge != "gitlab" || g.TokenFile != "/run/secrets/gitlab" {
		t.Errorf("Git = %+v", g)
	}
}

func TestAnUnknownSandboxDriverIsRefused(t *testing.T) {
	// The alternative is a gateway that starts, advertises sandboxes and fails
	// every request to create one.
	_, err := loadWith(t, valid(map[string]string{"KEERA_SANDBOX_DRIVER": "docker"}))
	if err == nil {
		t.Fatal("an unknown driver was accepted")
	}
	for _, want := range []string{"kubernetes", "podman"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q as a driver that exists", err, want)
		}
	}
}

func TestCookiesAreSecureWheneverTheBrowserWillBeOnHTTPS(t *testing.T) {
	// Nothing names this setting in a normal deployment: it is inferred from
	// the address the browser actually uses, because an operator who has put
	// the panel behind TLS has already said everything they should have to.
	//
	// The inference is the security-relevant part. Getting it wrong in the safe
	// direction breaks a local http deployment loudly; getting it wrong in the
	// other direction sends a session cookie over plaintext and nothing says so.
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "nothing configured at all",
			env:  nil,
			want: false,
		},
		{
			name: "a public URL on plain http",
			env:  map[string]string{"KEERA_PUBLIC_URL": "http://127.0.0.1:8080"},
			want: false,
		},
		{
			name: "a public URL on https",
			env:  map[string]string{"KEERA_PUBLIC_URL": "https://keera.example.ch"},
			want: true,
		},
		{
			name: "an explicit setting wins over the inference",
			env: map[string]string{
				"KEERA_PUBLIC_URL":     "https://keera.example.ch",
				"KEERA_SECURE_COOKIES": "false",
			},
			want: false,
		},
		{
			// A typo is ignored like a malformed number, so it cannot turn
			// Secure off.
			name: "a value that is not a switch keeps the inference",
			env: map[string]string{
				"KEERA_PUBLIC_URL":     "https://keera.example.ch",
				"KEERA_SECURE_COOKIES": "ture",
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadWith(t, valid(tc.env))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.SecureCookies != tc.want {
				t.Errorf("SecureCookies = %v, want %v", c.SecureCookies, tc.want)
			}
		})
	}
}

func TestThePublicURLLosesItsTrailingSlash(t *testing.T) {
	// It is concatenated with paths to build the redirect a browser is sent to,
	// and "https://keera.example.ch//control/auth/callback" is not the URL that
	// was registered with the identity provider.
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL": "https://keera.example.ch/",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PublicURL != "https://keera.example.ch" {
		t.Errorf("PublicURL = %q, want it without the trailing slash", c.PublicURL)
	}
}

func TestAProviderNamedTwiceIsRefused(t *testing.T) {
	// The name is stored in the external id of every sign-in it writes, so two
	// providers under one name would file two directories' people together.
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":     "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS": "okta,okta",
	}))
	if err == nil {
		t.Fatal("the same provider name was accepted twice")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error = %q, want it to say the name is repeated", err)
	}
}

func TestADefaultRoleThatIsNotARoleIsRefused(t *testing.T) {
	// It decides what everybody who matches no group gets, so a typo here is a
	// deployment where nobody can sign in - or one where the wrong people can.
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":              "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS":          "okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
		"KEERA_OIDC_OKTA_DEFAULT_ROLE":  "administrator",
	}))
	if err == nil {
		t.Fatal("an unknown default role was accepted")
	}
	if !strings.Contains(err.Error(), "admin or member") {
		t.Errorf("error = %q, want it to name the roles that exist", err)
	}
}

func TestAMalformedNumberFallsBackRatherThanFailing(t *testing.T) {
	// envInt and envDuration ignore what they cannot parse. That is a decision
	// worth pinning either way: it means a typo in a tuning variable leaves a
	// gateway running on the default rather than refusing to start, which is
	// the right trade for a number that only tunes and the wrong one for the
	// credentials and floors above, where Load refuses instead.
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_MAX_DB_CONNS": "sixteen",
		"KEERA_CACHE_TTL":    "half a minute",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxDBConns != 16 {
		t.Errorf("MaxDBConns = %d, want the default 16", c.MaxDBConns)
	}
	if c.CacheTTL != 30*time.Second {
		t.Errorf("CacheTTL = %v, want the default 30s", c.CacheTTL)
	}
}

func TestSandboxRuntimesAreReadPerIsolationTier(t *testing.T) {
	// Each tier names the runtime class that provides it, because what gives
	// you a microVM is called something different on every cluster.
	env := valid(map[string]string{"KEERA_SANDBOX_DRIVER": "kubernetes"})
	for _, tier := range policy.Isolations {
		env["KEERA_SANDBOX_RUNTIME_"+strings.ToUpper(string(tier))] = "runtime-" + string(tier)
	}
	c, err := loadWith(t, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tier := range policy.Isolations {
		if got := c.Sandbox.Runtimes[tier]; got != "runtime-"+string(tier) {
			t.Errorf("runtime for %s = %q, want %q", tier, got, "runtime-"+string(tier))
		}
	}
}

// One provider is configured the same way as several. The callback is the
// public address's, because one callback serves every provider.
func TestOneProviderIsNamedLikeSeveral(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":              "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS":          "okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.OIDC) != 1 {
		t.Fatalf("got %d providers, want 1", len(c.OIDC))
	}
	p := c.OIDC[0]
	if p.Name != "okta" {
		t.Errorf("provider name = %q, want okta", p.Name)
	}
	if p.RedirectURL != "https://keera.example.ch/control/auth/callback" {
		t.Errorf("redirect = %q, want the public URL's callback", p.RedirectURL)
	}
	if p.Mapping.Default != authn.RoleMember {
		t.Errorf("default role = %q, want member", p.Mapping.Default)
	}
}

// Without a public URL there is no address to send the browser back to.
func TestSingleSignOnNeedsThePublicURL(t *testing.T) {
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_OIDC_PROVIDERS":          "okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
	}))
	if err == nil || !strings.Contains(err.Error(), "KEERA_PUBLIC_URL") {
		t.Errorf("error = %v, want it to ask for KEERA_PUBLIC_URL", err)
	}
}

// With several directories, each customer's admin can give their users any
// address - another customer's, or an operator's. So every provider has to
// say which domains it may vouch for.
func TestSeveralProvidersMustNameTheirDomains(t *testing.T) {
	env := valid(map[string]string{
		"KEERA_OIDC_PROVIDERS":            "google,entra",
		"KEERA_PUBLIC_URL":                "https://keera.example.ch",
		"KEERA_OIDC_GOOGLE_ISSUER":        "https://accounts.google.com",
		"KEERA_OIDC_GOOGLE_CLIENT_ID":     "google-client",
		"KEERA_OIDC_GOOGLE_CLIENT_SECRET": "google-secret",
		"KEERA_OIDC_ENTRA_ISSUER":         "https://login.microsoftonline.com/tenant/v2.0",
		"KEERA_OIDC_ENTRA_CLIENT_ID":      "entra-client",
		"KEERA_OIDC_ENTRA_CLIENT_SECRET":  "entra-secret",
		"KEERA_OIDC_GOOGLE_DOMAINS":       "*",
	})
	_, err := loadWith(t, env)
	if err == nil {
		t.Fatal("a provider with no domains was accepted next to another")
	}
	if !strings.Contains(err.Error(), "KEERA_OIDC_ENTRA_DOMAINS") {
		t.Errorf("error = %q, want it to name the setting to add", err)
	}

	env["KEERA_OIDC_ENTRA_DOMAINS"] = "bank-a.ch"
	c, err := loadWith(t, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.OIDC[1].Domains; len(got) != 1 || got[0] != "bank-a.ch" {
		t.Errorf("entra domains = %v, want its own", got)
	}
}

// Nothing is shared between providers. A customer's directory admin can
// create a group of any name, so a shared operator group would let every
// customer make operators.
func TestProvidersShareNothing(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google,entra")
	t.Setenv("KEERA_OIDC_GOOGLE_ADMIN_GROUPS", "")
	t.Setenv("KEERA_OIDC_GOOGLE_OPERATOR_GROUPS", "")
	t.Setenv("KEERA_OIDC_ENTRA_ADMIN_GROUPS", "keera-admins")
	t.Setenv("KEERA_OIDC_ENTRA_OPERATOR_GROUPS", "keera-ops")

	got := oidcProviders("https://keera.example.ch")
	if len(got) != 2 {
		t.Fatalf("got %d providers, want 2", len(got))
	}
	if g := got[0].Mapping; len(g.OperatorGroups) != 0 || len(g.AdminGroups) != 0 {
		t.Errorf("google groups = %v / %v, want none", g.OperatorGroups, g.AdminGroups)
	}
	if g := got[1].Mapping.OperatorGroups; len(g) != 1 || g[0] != "keera-ops" {
		t.Errorf("entra operator groups = %v, want its own", g)
	}
}

// The operator role spans organisations, so it comes only from settings that
// name who holds it. A default of operator would make a whole directory one.
func TestADefaultRoleOfOperatorIsRefused(t *testing.T) {
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":              "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS":          "okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
		"KEERA_OIDC_OKTA_DEFAULT_ROLE":  "operator",
	}))
	if err == nil {
		t.Fatal("a default role of operator was accepted")
	}
}
