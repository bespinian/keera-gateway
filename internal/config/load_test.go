package config

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Load catches a deployment's mistakes. These tests pin each floor and the
// inferred secure-cookie flag, whose flip to insecure nobody would notice.

// loadWith runs Load against exactly env. The caller's own KEERA_ variables
// are cleared first, so a `make dev` shell cannot change the result.
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

// valid is the smallest environment Load accepts.
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
	// The required variables alone give a working gateway, with the defaults
	// the install guide quotes.
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
		{"redis key prefix", c.RedisPrefix, "keera"},
		// Retention is off by default, so billing history is never deleted
		// unasked.
		{"usage retention", c.UsageRetention, time.Duration(0)},
		{"audit retention", c.AuditRetention, time.Duration(0)},
		{"session gap", c.SessionGap, 30 * time.Minute},
		// On unless switched off.
		{"the panel", c.UI, true},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	// No Redis is the default: one replica does not need it.
	if c.Redis != nil {
		t.Errorf("Redis = %+v, want nil: Redis is opt-in", c.Redis)
	}
}

func TestARedisURLIsParsedOnce(t *testing.T) {
	// Load is the only parse of these options.
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
	// A database, an operator key and a secret key. None has a sensible
	// default.
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
			// Caught before the database is migrated.
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
	// The scrape token keeps the operator key out of Prometheus, so they must
	// differ.
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
	// Its route is labelled by organisation, so it is a real credential.
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
	// A stray "10m", likely meant as "10mo", would delete the day's billing
	// data.
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

			// The boundary is allowed, and zero means "keep everything".
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

func TestADatabasePoolTooSmallToServeIsRefused(t *testing.T) {
	// Below four, a transaction can leave nothing for the requests around it.
	if _, err := loadWith(t, valid(map[string]string{"KEERA_MAX_DB_CONNS": "2"})); err == nil {
		t.Error("a pool of two connections was accepted")
	}
	if _, err := loadWith(t, valid(map[string]string{"KEERA_MAX_DB_CONNS": "4"})); err != nil {
		t.Errorf("a pool of four connections was refused: %v", err)
	}
}

func TestASessionGapBelowAMinuteIsTheRequestLog(t *testing.T) {
	// Under a minute, every request would be its own task.
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

// KEERA_CHAT_URL decides where a sign-in may be handed, so it must be a bare
// origin.
func TestTheChatURLIsABareOrigin(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{"KEERA_CHAT_URL": "https://chat.example.ch/"}))
	if err != nil || c.ChatURL != "https://chat.example.ch" {
		t.Fatalf("ChatURL = %q, %v; want the origin without its slash", c.ChatURL, err)
	}
	for _, bad := range []string{
		"chat.example.ch",
		"ftp://chat.example.ch",
		"https://chat.example.ch/chat",
		"https://chat.example.ch?x=1",
		"https://me@chat.example.ch",
	} {
		if _, err := loadWith(t, valid(map[string]string{"KEERA_CHAT_URL": bad})); err == nil {
			t.Errorf("KEERA_CHAT_URL=%q was accepted", bad)
		}
	}
}

func TestSandboxSettingsAreOnlyCheckedWhenSandboxesAreOn(t *testing.T) {
	// Without a driver the setting does nothing, so a leftover line must not
	// stop the start.
	c, err := loadWith(t, valid(map[string]string{"KEERA_SANDBOX_IDLE_SUSPEND": "1s"}))
	if err != nil {
		t.Fatalf("a stray sandbox setting stopped a gateway with sandboxes off: %v", err)
	}
	if c.Sandbox.Enabled() {
		t.Error("sandboxes are enabled with no driver configured")
	}

	// With a driver it is checked: a few seconds would suspend a sandbox
	// whose user is reading a diff.
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

func TestLogSettingsAreReadWhateverTheirCaseAndUnknownOnesRefused(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_LOG_LEVEL": "DEBUG", "KEERA_LOG_FORMAT": "JSON",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "debug" || c.LogFormat != "json" {
		t.Errorf("level, format = %q, %q; want debug, json", c.LogLevel, c.LogFormat)
	}
	for name, value := range map[string]string{
		"KEERA_LOG_LEVEL": "verbose", "KEERA_LOG_FORMAT": "logfmt",
	} {
		_, err := loadWith(t, valid(map[string]string{name: value}))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: error = %v, want a refusal that names it", name, value, err)
		}
	}
}

func TestAnUnknownSandboxDriverIsRefused(t *testing.T) {
	// Otherwise the gateway would advertise sandboxes it cannot create.
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
	// Inferred from the public URL. Wrong one way breaks a local http
	// deployment loudly; wrong the other sends the cookie in plaintext quietly.
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
	// Otherwise the callback would be "https://keera.example.ch//control/...",
	// which is not the URL registered with the identity provider.
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
	// The name is part of every sign-in's external id, so two providers under
	// one name would mix two directories' people.
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":              "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS":          "okta,okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
	}))
	if err == nil {
		t.Fatal("the same provider name was accepted twice")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error = %q, want it to say the name is repeated", err)
	}
}

func TestADefaultRoleThatIsNotAdminOrMemberIsRefused(t *testing.T) {
	// It decides what everyone who matches no group gets, so a typo could lock
	// everyone out or let the wrong people in. Operator is not allowed: as a
	// default it would make a whole directory operators.
	for _, role := range []string{"administrator", "operator"} {
		_, err := loadWith(t, valid(map[string]string{
			"KEERA_PUBLIC_URL":              "https://keera.example.ch",
			"KEERA_OIDC_PROVIDERS":          "okta",
			"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
			"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
			"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
			"KEERA_OIDC_OKTA_DEFAULT_ROLE":  role,
		}))
		if err == nil {
			t.Errorf("a default role of %s was accepted", role)
		} else if !strings.Contains(err.Error(), "admin or member") {
			t.Errorf("%s: error = %q, want it to name the roles that can be defaults", role, err)
		}
	}
}

func TestAMalformedNumberFallsBackRatherThanFailing(t *testing.T) {
	// A typo in a tuning number keeps the default rather than stop the start.
	// The credentials and floors above are refused instead.
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
	// Each tier names its runtime class, as the name differs per cluster.
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

// One provider is configured the same way as several, with the shared callback.
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

// A provider named in the list but not set up stops the start.
func TestAProviderListedButNotSetUpIsRefused(t *testing.T) {
	_, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":              "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS":          "okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
	}))
	if err == nil || !strings.Contains(err.Error(), "KEERA_OIDC_OKTA_CLIENT_ID") {
		t.Errorf("error = %v, want it to name the missing setting", err)
	}
}

// A role is read in any case.
func TestTheDefaultRoleIsReadInAnyCase(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":              "https://keera.example.ch",
		"KEERA_OIDC_PROVIDERS":          "okta",
		"KEERA_OIDC_OKTA_ISSUER":        "https://accounts.example.ch",
		"KEERA_OIDC_OKTA_CLIENT_ID":     "keera",
		"KEERA_OIDC_OKTA_CLIENT_SECRET": "a-secret",
		"KEERA_OIDC_OKTA_DEFAULT_ROLE":  "Admin",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OIDC[0].Mapping.Default != authn.RoleAdmin {
		t.Errorf("default role = %q, want admin", c.OIDC[0].Mapping.Default)
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
// address, so every provider must name the domains it vouches for.
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

// Nothing is shared between providers: a shared operator group would let any
// customer's directory admin make operators.
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

// Browsers run WebAuthn only on https or on localhost.
func TestPasskeysNeedAPublicURLABrowserAccepts(t *testing.T) {
	for url, ok := range map[string]bool{
		"":                         false,
		"http://keera.example.ch":  false,
		"http://127.0.0.1:8080":    false,
		"https://keera.example.ch": true,
		"http://localhost:8080":    true,
	} {
		c, err := loadWith(t, valid(map[string]string{
			"KEERA_PASSKEYS": "true", "KEERA_PUBLIC_URL": url,
		}))
		if ok && (err != nil || c.Passkeys == nil) {
			t.Errorf("%q: %v, passkeys %v; want them on", url, err, c.Passkeys)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "KEERA_PASSKEYS")) {
			t.Errorf("%q: error = %v, want one naming KEERA_PASSKEYS", url, err)
		}
	}
	if c, err := loadWith(t, valid(nil)); err != nil || c.Passkeys != nil {
		t.Errorf("passkeys = %v, %v; want them off by default", c.Passkeys, err)
	}
}

func TestTheGatewayKeepsAwayFromItsHostByDefault(t *testing.T) {
	// Loopback and the cloud metadata service are the gateway's host, not the
	// administrator's.
	c, err := loadWith(t, valid(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var denied []string
	for _, p := range c.UpstreamDeny {
		denied = append(denied, p.String())
	}
	for _, want := range []string{"127.0.0.0/8", "::1/128", "169.254.0.0/16"} {
		if !slices.Contains(denied, want) {
			t.Errorf("deny list %v does not have %s", denied, want)
		}
	}

	if c, err := loadWith(t, valid(map[string]string{"KEERA_UPSTREAM_DENY": "none"})); err != nil ||
		c.UpstreamDeny != nil {
		t.Errorf("none = %v, %v; want nothing denied", c.UpstreamDeny, err)
	}
	// A typo must not quietly open the host up.
	if _, err := loadWith(t, valid(map[string]string{"KEERA_UPSTREAM_DENY": "127.0.0.1/33"})); err == nil {
		t.Error("an unreadable deny list was accepted")
	}
}

func TestTrustedProxiesDefaultToThePrivateRanges(t *testing.T) {
	c, err := loadWith(t, valid(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TrustedProxies) != 6 || c.TrustedProxies[2].String() != "10.0.0.0/8" {
		t.Errorf("trusted proxies = %v, want loopback and the private ranges", c.TrustedProxies)
	}
	if c, err := loadWith(t, valid(map[string]string{"KEERA_TRUSTED_PROXIES": "none"})); err != nil ||
		c.TrustedProxies != nil {
		t.Errorf("none = %v, %v; want no proxy trusted", c.TrustedProxies, err)
	}
	if _, err := loadWith(t, valid(map[string]string{"KEERA_TRUSTED_PROXIES": "10.0.0.0/33"})); err == nil {
		t.Error("an unreadable proxy list was accepted")
	}
}

func TestPrivateHostsLimitWhatOrganisationsReach(t *testing.T) {
	c, err := loadWith(t, valid(nil))
	if err != nil || c.LimitPrivate {
		t.Errorf("default = %v, %v; want the private network open", c.LimitPrivate, err)
	}
	c, err = loadWith(t, valid(map[string]string{"KEERA_UPSTREAM_PRIVATE": "keera-engine"}))
	if err != nil || !c.LimitPrivate || !slices.Equal(c.PrivateHosts, []string{"keera-engine"}) {
		t.Errorf("keera-engine = %v %v, %v", c.LimitPrivate, c.PrivateHosts, err)
	}
	if _, err := loadWith(t, valid(map[string]string{"KEERA_UPSTREAM_PRIVATE": "keera-engine:8000"})); err == nil {
		t.Error("a host with a port was accepted")
	}
}
