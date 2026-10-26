package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/authn"
)

func TestNoProviderIsNotAnError(t *testing.T) {
	if got := oidcProviders("https://keera.example.ch"); got != nil {
		t.Errorf("oidcProviders = %+v, want none", got)
	}
}

// A provider without its secret is refused by the setting's name.
func TestAProviderWithoutItsSecretNamesTheSetting(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google")
	t.Setenv("KEERA_OIDC_GOOGLE_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_ID", "google-client")
	c := Config{PublicURL: "https://keera.example.ch", OIDC: oidcProviders("https://keera.example.ch")}
	err := c.validateOIDC()
	if err == nil || !strings.Contains(err.Error(), "KEERA_OIDC_GOOGLE_CLIENT_SECRET") {
		t.Errorf("err = %v, want it to name KEERA_OIDC_GOOGLE_CLIENT_SECRET", err)
	}
}

// Two directories, one callback, and group names per directory: a name on
// Google, an object GUID on Entra.
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

	// The callback is shared: it recognises a flow by its state.
	for _, p := range got {
		if p.RedirectURL != "https://keera.example.ch/control/auth/callback" {
			t.Errorf("%s redirect = %q, want the shared one", p.Name, p.RedirectURL)
		}
		if len(p.Mapping.OperatorEmails) != 1 {
			t.Errorf("%s operators = %v, want the deployment's one list", p.Name, p.Mapping.OperatorEmails)
		}
	}

	// The client and its secret are never shared, or people could sign in
	// against the wrong directory.
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

// Sign-up makes the creator of an organisation its administrator, which an
// admin group would take away at the next sign-in.
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

// Anyone who signs up chooses where their models are, so the operator must say
// what inside the network they may reach.
func TestSignUpNeedsPrivateHostsSet(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google")
	t.Setenv("KEERA_OIDC_GOOGLE_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_ID", "google-client")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_SECRET", "google-secret")
	t.Setenv("KEERA_OIDC_GOOGLE_SIGNUP", "true")
	c := Config{PublicURL: "https://keera.example.ch", OIDC: oidcProviders("https://keera.example.ch")}

	if _, _, err := c.privateHosts(); err == nil {
		t.Error("sign-up without KEERA_UPSTREAM_PRIVATE was accepted")
	}
	t.Setenv("KEERA_UPSTREAM_PRIVATE", "keera-engine")
	if limit, hosts, err := c.privateHosts(); err != nil || !limit || len(hosts) != 1 {
		t.Errorf("keera-engine = %v %v, %v", limit, hosts, err)
	}
}

// Any GitHub account can sign in to an OAuth App, so a GitHub provider must
// name its domains, even when it is the only one.
func TestGitHubNeedsDomains(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "github")
	t.Setenv("KEERA_OIDC_GITHUB_ISSUER", "https://github.com")
	t.Setenv("KEERA_OIDC_GITHUB_CLIENT_ID", "github-client")
	t.Setenv("KEERA_OIDC_GITHUB_CLIENT_SECRET", "github-secret")

	c := Config{PublicURL: "https://keera.example.ch", OIDC: oidcProviders("https://keera.example.ch")}
	if c.OIDC[0].DisplayName != "GitHub" {
		t.Errorf("label = %q, want GitHub", c.OIDC[0].DisplayName)
	}
	if err := c.validateOIDC(); err == nil {
		t.Error("a GitHub provider without domains was accepted")
	}

	t.Setenv("KEERA_OIDC_GITHUB_DOMAINS", "example.ch")
	c.OIDC = oidcProviders("https://keera.example.ch")
	if err := c.validateOIDC(); err != nil {
		t.Errorf("a GitHub provider with domains: %v", err)
	}
}

// An LDAP directory needs no OAuth client, and its administrator sets every
// address, so it must name its domains and cannot let strangers sign up.
func TestLDAPProvider(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "corp")
	t.Setenv("KEERA_OIDC_CORP_ISSUER", "ldaps://dc1.corp.example.ch")
	t.Setenv("KEERA_OIDC_CORP_LDAP_BIND_DN", "cn=keera-svc,dc=corp,dc=example,dc=ch")
	t.Setenv("KEERA_OIDC_CORP_LDAP_BIND_PASSWORD", "secret")
	t.Setenv("KEERA_OIDC_CORP_LDAP_BASE_DN", "dc=corp,dc=example,dc=ch")
	t.Setenv("KEERA_OIDC_CORP_LDAP_LOGIN_ATTRIBUTES", "uid, mail")

	load := func() Config {
		return Config{PublicURL: "https://keera.example.ch", OIDC: oidcProviders("https://keera.example.ch")}
	}
	c := load()
	if got := c.OIDC[0].LDAP.LoginAttributes; len(got) != 2 || got[1] != "mail" {
		t.Errorf("login attributes = %q", got)
	}
	if err := c.validateOIDC(); err == nil {
		t.Error("an LDAP provider without domains was accepted")
	}

	t.Setenv("KEERA_OIDC_CORP_DOMAINS", "*")
	if err := load().validateOIDC(); err == nil {
		t.Errorf("an LDAP provider that vouches for every domain was accepted")
	}

	t.Setenv("KEERA_OIDC_CORP_DOMAINS", "corp.example.ch")
	if err := load().validateOIDC(); err != nil {
		t.Errorf("a complete LDAP provider: %v", err)
	}

	t.Setenv("KEERA_OIDC_CORP_SIGNUP", "true")
	if err := load().validateOIDC(); err == nil {
		t.Error("sign-up through an LDAP directory was accepted")
	}
	t.Setenv("KEERA_OIDC_CORP_SIGNUP", "false")

	t.Setenv("KEERA_OIDC_CORP_LDAP_BASE_DN", "")
	if err := load().validateOIDC(); err == nil {
		t.Error("an LDAP provider without a base DN was accepted")
	} else if !strings.Contains(err.Error(), "KEERA_OIDC_CORP_LDAP_BASE_DN") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// A group DN holds commas, so LDAP group lists are split on semicolons. OpenID
// Connect ones stay comma-separated.
func TestLDAPGroupsAreWholeDNs(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "corp,okta")
	t.Setenv("KEERA_OIDC_CORP_ISSUER", "ldaps://dc1.corp.example.ch")
	t.Setenv("KEERA_OIDC_CORP_ADMIN_GROUPS",
		"CN=keera-admins,OU=Groups,DC=corp,DC=example,DC=ch; CN=Domain Admins,CN=Users,DC=corp,DC=example,DC=ch")
	t.Setenv("KEERA_OIDC_CORP_OPERATOR_GROUPS", "CN=keera-ops,OU=Groups,DC=corp,DC=example,DC=ch")
	t.Setenv("KEERA_OIDC_OKTA_ISSUER", "https://acme.okta.com")
	t.Setenv("KEERA_OIDC_OKTA_ADMIN_GROUPS", "keera-admins, platform")

	c := oidcProviders("https://keera.example.ch")
	corp, okta := c[0].Mapping, c[1].Mapping
	want := []string{
		"CN=keera-admins,OU=Groups,DC=corp,DC=example,DC=ch",
		"CN=Domain Admins,CN=Users,DC=corp,DC=example,DC=ch",
	}
	if !slices.Equal(corp.AdminGroups, want) {
		t.Errorf("LDAP admin groups = %q, want %q", corp.AdminGroups, want)
	}
	if len(corp.OperatorGroups) != 1 {
		t.Errorf("LDAP operator groups = %q, want one DN", corp.OperatorGroups)
	}
	if !slices.Equal(okta.AdminGroups, []string{"keera-admins", "platform"}) {
		t.Errorf("OpenID Connect admin groups = %q", okta.AdminGroups)
	}
}

func TestAnLDAPProviderAmongOthersIsNotToldItMayVouchForAnyDomain(t *testing.T) {
	t.Setenv("KEERA_OIDC_PROVIDERS", "google,corp")
	t.Setenv("KEERA_OIDC_GOOGLE_ISSUER", "https://accounts.google.com")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_ID", "google-client")
	t.Setenv("KEERA_OIDC_GOOGLE_CLIENT_SECRET", "google-secret")
	t.Setenv("KEERA_OIDC_GOOGLE_DOMAINS", "*")
	t.Setenv("KEERA_OIDC_CORP_ISSUER", "ldaps://dc1.corp.example.ch")
	t.Setenv("KEERA_OIDC_CORP_LDAP_BIND_DN", "cn=keera-svc,dc=corp,dc=example,dc=ch")
	t.Setenv("KEERA_OIDC_CORP_LDAP_BIND_PASSWORD", "secret")
	t.Setenv("KEERA_OIDC_CORP_LDAP_BASE_DN", "dc=corp,dc=example,dc=ch")

	c := Config{PublicURL: "https://keera.example.ch", OIDC: oidcProviders("https://keera.example.ch")}
	err := c.validateOIDC()
	if err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Errorf("err = %v, want the directory's own message, which refuses %q", err, "*")
	}
}
