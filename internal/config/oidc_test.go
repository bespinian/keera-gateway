package config

import (
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
)

func TestNoProviderIsNotAnError(t *testing.T) {
	if got := oidcProviders("https://keera.example.ch"); got != nil {
		t.Errorf("oidcProviders = %+v, want none", got)
	}
}

// The hosted deployment: two directories, one callback, and group names that
// are per-directory because the same role is a name on Google and an object
// GUID on Entra.
func TestEachProviderReadsItsOwnSettings(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google,entra")
	t.Setenv("KEERA_OPERATORS", "ada@example.ch")

	t.Setenv("KEERA_OIDC_GOOGLE_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_ID", "google-client")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_SECRET", "google-secret")

	t.Setenv("KEERA_OIDC_ENTRA_ISSUER", "https://login.microsoftonline.com/tenant/v2.0")
	t.Setenv("KEERA_OIDC_ENTRA_CLIENT_ID", "entra-client")
	t.Setenv("KEERA_OIDC_ENTRA_CLIENT_SECRET", "entra-secret")
	t.Setenv("KEERA_OIDC_ENTRA_ADMIN_GROUPS", "11111111-2222-3333-4444-555555555555")
	t.Setenv("KEERA_OIDC_ENTRA_GROUPS_CLAIM", "roles")
	t.Setenv("KEERA_OIDC_ENTRA_DEFAULT_ROLE", "admin")

	got := oidcProviders("https://keera.example.ch")
	if len(got) != 2 {
		t.Fatalf("got %d providers, want 2", len(got))
	}
	google, entra := got[0], got[1]

	// The callback is shared. It can be, because the callback recognises a
	// flow by its state rather than by the address it came back to.
	for _, p := range got {
		if p.RedirectURL != "https://keera.example.ch/control/auth/callback" {
			t.Errorf("%s redirect = %q, want the shared one", p.Name, p.RedirectURL)
		}
		if len(p.Mapping.OperatorEmails) != 1 {
			t.Errorf("%s operators = %v, want the deployment's one list", p.Name, p.Mapping.OperatorEmails)
		}
	}

	// The client and its secret are never shared: two providers on one client
	// would sign people in against the wrong directory rather than fail.
	if google.ClientID == entra.ClientID || google.ClientSecret == entra.ClientSecret {
		t.Error("two providers ended up on one client")
	}
	if google.IssuerURL != "https://accounts.google.com" {
		t.Errorf("google issuer = %q", google.IssuerURL)
	}

	// Unset is empty, which authn reads as "groups".
	if entra.GroupsClaim != "roles" || google.GroupsClaim != "" {
		t.Errorf("groups claims = %q and %q, want the override and unset",
			entra.GroupsClaim, google.GroupsClaim)
	}
	if entra.Mapping.Default != authn.RoleAdmin || google.Mapping.Default != authn.RoleMember {
		t.Errorf("default roles = %q and %q, want Entra's own and the default",
			entra.Mapping.Default, google.Mapping.Default)
	}
	if len(entra.Mapping.AdminGroups) != 1 || len(google.Mapping.AdminGroups) != 0 {
		t.Errorf("admin groups = %v and %v, want Entra's GUID and nothing on Google",
			entra.Mapping.AdminGroups, google.Mapping.AdminGroups)
	}

	// The button says the vendor's name, not the operator's name for it.
	if google.Label() != "Google" || entra.Label() != "Microsoft" {
		t.Errorf("labels = %q and %q, want Google and Microsoft", google.Label(), entra.Label())
	}
}

func TestProviderLabelCanBeSet(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "acme-ad")
	t.Setenv("KEERA_OIDC_ACME_AD_ISSUER", "https://login.acme.example")
	t.Setenv("KEERA_OIDC_ACME_AD_CLIENT_ID", "client")
	t.Setenv("KEERA_OIDC_ACME_AD_LABEL", "your Acme account")

	got := oidcProviders("https://keera.example.ch")
	if len(got) != 1 {
		t.Fatalf("got %d providers, want 1", len(got))
	}
	// A hyphen in the name is an underscore in the variable it reads.
	if got[0].ClientID != "client" {
		t.Errorf("client = %q, want the one under the underscored name", got[0].ClientID)
	}
	if got[0].Label() != "your Acme account" {
		t.Errorf("label = %q, want the one that was set", got[0].Label())
	}
}

// Sign-up makes whoever creates an organisation its administrator. An admin
// group decides that role for the whole gateway, so the two together would
// take it away again at the next sign-in.
func TestSignUpIsRefusedAlongsideAnAdminGroup(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google,entra")
	t.Setenv("KEERA_OIDC_GOOGLE_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_ID", "google-client")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_SECRET", "google-secret")
	t.Setenv("KEERA_OIDC_GOOGLE_DOMAINS", "*")
	t.Setenv("KEERA_OIDC_GOOGLE_SIGNUP", "true")
	t.Setenv("KEERA_OIDC_ENTRA_ISSUER", "https://login.microsoftonline.com/tenant/v2.0")
	t.Setenv("KEERA_OIDC_ENTRA_CLIENT_ID", "entra-client")
	t.Setenv("KEERA_OIDC_ENTRA_CLIENT_SECRET", "entra-secret")
	t.Setenv("KEERA_OIDC_ENTRA_DOMAINS", "example.ch")

	c := Config{PublicURL: "https://keera.example.ch", OIDC: oidcProviders("https://keera.example.ch")}
	if !c.OIDC[0].SignUp || c.OIDC[1].SignUp {
		t.Fatalf("sign-up = %v and %v, want Google's own setting only",
			c.OIDC[0].SignUp, c.OIDC[1].SignUp)
	}
	if err := c.validateOIDC(); err != nil {
		t.Fatalf("sign-up without admin groups: %v", err)
	}

	t.Setenv("KEERA_OIDC_ENTRA_ADMIN_GROUPS", "keera-admins")
	c.OIDC = oidcProviders("https://keera.example.ch")
	if err := c.validateOIDC(); err == nil {
		t.Error("sign-up alongside an admin group was accepted")
	}
}

// Anyone who signs up runs an organisation, and an organisation chooses where
// its models are, so the operator has to say what inside the network it may
// reach.
func TestSignUpNeedsPrivateHostsSet(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google")
	t.Setenv("KEERA_OIDC_GOOGLE_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_ID", "google-client")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_SECRET", "google-secret")
	t.Setenv("KEERA_OIDC_GOOGLE_SIGNUP", "true")
	c := Config{OIDC: oidcProviders("https://keera.example.ch")}

	if _, _, err := c.privateHosts(); err == nil {
		t.Error("sign-up without KEERA_UPSTREAM_PRIVATE was accepted")
	}
	t.Setenv("KEERA_UPSTREAM_PRIVATE", "keera-engine")
	if limit, hosts, err := c.privateHosts(); err != nil || !limit || len(hosts) != 1 {
		t.Errorf("keera-engine = %v %v, %v", limit, hosts, err)
	}
}
