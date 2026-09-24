package authn

import (
	"strings"
	"testing"
)

func TestOIDCConfigValidate(t *testing.T) {
	full := OIDCConfig{
		Name:      "entra",
		IssuerURL: "https://login.example.ch", ClientID: "keera",
		ClientSecret: "s", RedirectURL: "https://keera.example.ch/auth/callback",
	}
	if err := full.Validate(); err != nil {
		t.Errorf("a complete configuration was rejected: %v", err)
	}

	// Nothing configured is not an error: the operator key is the deployment's
	// first ten minutes.
	if err := (OIDCConfig{}).Validate(); err != nil {
		t.Errorf("an empty configuration should be allowed: %v", err)
	}
	if (OIDCConfig{}).Configured() {
		t.Error("an empty configuration must not report itself as configured")
	}

	tests := []struct {
		name string
		mut  func(*OIDCConfig)
		want string
	}{
		{"no secret", func(c *OIDCConfig) { c.ClientSecret = "" }, "client secret"},
		{"no redirect", func(c *OIDCConfig) { c.RedirectURL = "" }, "redirect URL"},
		{"a relative issuer", func(c *OIDCConfig) { c.IssuerURL = "login.example.ch" }, "absolute"},
		{"no name", func(c *OIDCConfig) { c.Name = "" }, "name"},
		// The name ends up in a URL and in every external ID this provider
		// writes, so it is kept to what is unambiguous in both.
		{"a name with a space", func(c *OIDCConfig) { c.Name = "entra id" }, "provider name"},
		{"a name in capitals", func(c *OIDCConfig) { c.Name = "Entra" }, "provider name"},
		{"a name with a colon", func(c *OIDCConfig) { c.Name = "en:tra" }, "provider name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := full
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("an incomplete configuration was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNewFlowIsUniqueInEveryPart(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		f, err := NewFlow()
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range []string{f.State, f.Verifier, f.Nonce} {
			if len(part) < 40 {
				t.Fatalf("%q is too short to be unguessable", part)
			}
			if seen[part] {
				t.Fatalf("%q was generated twice", part)
			}
			seen[part] = true
		}
		if f.State == f.Verifier || f.State == f.Nonce || f.Verifier == f.Nonce {
			t.Fatal("the parts of one flow must differ from each other")
		}
	}
}

func TestIdentityDomain(t *testing.T) {
	tests := []struct{ email, want string }{
		{"ada@Example.CH", "example.ch"},
		{"ada@sub.example.ch", "sub.example.ch"},
		{"weird@name@example.ch", "example.ch"},
		{"nodomain", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := (Identity{Email: tc.email}).Domain(); got != tc.want {
			t.Errorf("Domain(%q) = %q, want %q", tc.email, got, tc.want)
		}
	}
}

func TestStringsClaimAcceptsWhatProvidersActuallySend(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{"a list, as most send", map[string]any{"groups": []any{"a", "b"}}, []string{"a", "b"}},
		{"a single string", map[string]any{"groups": "a"}, []string{"a"}},
		{"space separated", map[string]any{"groups": "a b"}, []string{"a", "b"}},
		{"missing", map[string]any{}, nil},
		{"a number, which is nobody's group", map[string]any{"groups": 3}, nil},
		{"a list with junk in it", map[string]any{"groups": []any{"a", 2}}, []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stringsClaim(tc.claims, "groups")
			if len(got) != len(tc.want) {
				t.Fatalf("stringsClaim = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("stringsClaim = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
