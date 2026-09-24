package config

import (
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
)

// A deployment with one directory names nothing: the unprefixed settings are
// the whole configuration, which is the dedicated and on-premises shape.
func TestOneProviderNeedsNoList(t *testing.T) {
	t.Setenv("KEERA_OIDC_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_CLIENT_ID", "client")
	t.Setenv("KEERA_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("KEERA_OIDC_REDIRECT_URL", "https://keera.example.ch/control/auth/callback")
	t.Setenv("KEERA_OPERATORS", "ada@example.ch")

	got := oidcProviders()
	if len(got) != 1 {
		t.Fatalf("got %d providers, want 1", len(got))
	}
	p := got[0]
	if p.Name != singleProviderName {
		t.Errorf("name = %q, want %q", p.Name, singleProviderName)
	}
	if p.IssuerURL != "https://accounts.google.com" || p.ClientSecret != "secret" {
		t.Errorf("provider = %+v, want the unprefixed settings", p)
	}
	if p.Mapping.Default != authn.RoleMember {
		t.Errorf("default role = %q, want member", p.Mapping.Default)
	}
	if len(p.Mapping.OperatorEmails) != 1 {
		t.Errorf("operator emails = %v, want the one address", p.Mapping.OperatorEmails)
	}
}

func TestNoProviderIsNotAnError(t *testing.T) {
	if got := oidcProviders(); got != nil {
		t.Errorf("oidcProviders = %+v, want none", got)
	}
}

// The hosted deployment: two directories, one callback, and group names that
// are per-directory because the same role is a name on Google and an object
// GUID on Entra.
func TestProvidersInheritWhatIsWorthSharing(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google,entra")
	t.Setenv("KEERA_OIDC_REDIRECT_URL", "https://keera.example.ch/control/auth/callback")
	t.Setenv("KEERA_OIDC_DEFAULT_ROLE", "member")
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

	got := oidcProviders()
	if len(got) != 2 {
		t.Fatalf("got %d providers, want 2", len(got))
	}
	google, entra := got[0], got[1]

	// The callback is shared. It can be, because the callback recognises a
	// flow by its state rather than by the address it came back to - and a
	// deployment that had to register a second one would be registering a
	// second thing that can go wrong.
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

	// Per-provider settings override the shared ones.
	if entra.GroupsClaim != "roles" || google.GroupsClaim != "groups" {
		t.Errorf("groups claims = %q and %q, want the override and the default",
			entra.GroupsClaim, google.GroupsClaim)
	}
	if entra.Mapping.Default != authn.RoleAdmin || google.Mapping.Default != authn.RoleMember {
		t.Errorf("default roles = %q and %q, want the override and the shared one",
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

	got := oidcProviders()
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
