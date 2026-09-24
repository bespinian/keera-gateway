package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig describes one identity provider. A dedicated deployment has one;
// the hosted one has several, because its customers use different directories.
type OIDCConfig struct {
	// Name identifies this provider in configuration, in the sign-in URL and
	// in stored external IDs. Renaming it orphans everyone who signed in
	// through it.
	Name string
	// DisplayName is the sign-in button's text. Empty falls back to Name.
	DisplayName  string
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// RedirectURL is the absolute URL of the control plane's callback, since
	// the identity provider redirects the browser to it. Several providers
	// may share one: the callback tells flows apart by their state.
	RedirectURL string
	Scopes      []string
	// GroupsClaim is where the provider puts group membership, usually
	// "groups" and sometimes "roles".
	GroupsClaim string
	Mapping     RoleMapping
}

// nameRE is what a provider name may be. The name ends up in a URL and in a
// stored external ID, so it keeps to characters that are safe in both.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Configured reports whether this provider was set up at all. Without one the
// control plane still works through the operator key.
func (c OIDCConfig) Configured() bool {
	return c.IssuerURL != "" && c.ClientID != ""
}

// Label is what to call this provider in front of a person.
func (c OIDCConfig) Label() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// Validate checks a configuration that claims to be complete.
func (c OIDCConfig) Validate() error {
	switch {
	case !c.Configured():
		return nil
	case c.Name == "":
		return errors.New("an identity provider needs a name")
	case !nameRE.MatchString(c.Name):
		return fmt.Errorf("%q is not a usable provider name; use lower-case "+
			"letters, digits and hyphens", c.Name)
	case c.ClientSecret == "":
		return fmt.Errorf("the client secret is required for the %s identity provider", c.Name)
	case c.RedirectURL == "":
		return fmt.Errorf("the redirect URL is required for the %s identity provider; "+
			"it is the absolute URL of /control/auth/callback as the browser sees it", c.Name)
	case !strings.HasPrefix(c.IssuerURL, "https://") && !strings.HasPrefix(c.IssuerURL, "http://"):
		return fmt.Errorf("the issuer for the %s identity provider must be an absolute URL", c.Name)
	}
	return nil
}

// Identity is what the provider said about a person.
type Identity struct {
	// Provider is the name of the provider that vouched for this person.
	Provider string
	Subject  string
	Email    string
	Name     string
	Groups   []string
}

// ExternalID is how this identity is stored. It includes the provider because
// a subject is only unique within the directory that issued it.
func (i Identity) ExternalID() string {
	return i.Provider + ":" + i.Subject
}

// Domain is the email domain, used to place a first sign-in in the right
// tenant.
func (i Identity) Domain() string {
	if at := strings.LastIndexByte(i.Email, '@'); at >= 0 {
		return strings.ToLower(i.Email[at+1:])
	}
	return ""
}

// OIDC is a configured identity provider.
type OIDC struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// NewOIDC discovers the provider's metadata. It does so once at start-up, so a
// typo in the issuer URL stops the gateway rather than the first sign-in.
func NewOIDC(ctx context.Context, cfg OIDCConfig, client *http.Client) (*OIDC, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, nil
	}
	if client != nil {
		ctx = oidc.ClientContext(ctx, client)
	}
	provider, err := oidc.NewProvider(ctx, strings.TrimRight(cfg.IssuerURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("discovering the identity provider at %s: %w", cfg.IssuerURL, err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &OIDC{
		cfg:      cfg,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
	}, nil
}

// Name is how this provider is addressed in a URL and stored in an identity.
func (o *OIDC) Name() string { return o.cfg.Name }

// Label is what to call this provider in front of a person.
func (o *OIDC) Label() string { return o.cfg.Label() }

// Issuer is the directory this provider signs people in against.
func (o *OIDC) Issuer() string { return o.cfg.IssuerURL }

// Mapping returns how this provider's groups map onto Keera Gateway's roles.
func (o *OIDC) Mapping() RoleMapping { return o.cfg.Mapping }

// Providers are the identity providers a deployment offers, in the order the
// sign-in screen should show them.
type Providers []*OIDC

// NewProviders discovers every configured provider. One unreachable provider
// fails the start, rather than leaving its users unable to sign in with no
// explanation.
func NewProviders(ctx context.Context, cfgs []OIDCConfig, client *http.Client) (Providers, error) {
	var out Providers
	seen := map[string]bool{}
	for _, cfg := range cfgs {
		if !cfg.Configured() {
			continue
		}
		if seen[cfg.Name] {
			return nil, fmt.Errorf("two identity providers are both named %q", cfg.Name)
		}
		seen[cfg.Name] = true
		p, err := NewOIDC(ctx, cfg, client)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Enabled reports whether single sign-on is available at all.
func (p Providers) Enabled() bool { return len(p) > 0 }

// ByName finds one provider, or nil.
func (p Providers) ByName(name string) *OIDC {
	for _, o := range p {
		if o.cfg.Name == name {
			return o
		}
	}
	return nil
}

// Only returns the provider when there is exactly one, and nil otherwise. It
// lets a sign-in link that names no provider work when there is no choice.
func (p Providers) Only() *OIDC {
	if len(p) == 1 {
		return p[0]
	}
	return nil
}

// AdminFromDirectory reports whether the directory, not Keera, decides the
// administrator role on this deployment. It is one answer for the whole
// gateway, so the panel never has to explain per person where a role came
// from.
func (p Providers) AdminFromDirectory() bool {
	for _, o := range p {
		if o.cfg.Mapping.DecidesAdmin() {
			return true
		}
	}
	return false
}

// Names lists the configured providers, for an error that has to say what the
// alternatives were.
func (p Providers) Names() []string {
	out := make([]string, 0, len(p))
	for _, o := range p {
		out = append(out, o.cfg.Name)
	}
	return out
}

// Flow is one login in flight.
type Flow struct {
	State    string
	Verifier string
	Nonce    string
}

// NewFlow mints the state, the PKCE verifier and the nonce for a login.
func NewFlow() (Flow, error) {
	var parts [3]string
	for i := range parts {
		s, err := randomToken()
		if err != nil {
			return Flow{}, err
		}
		parts[i] = s
	}
	return Flow{State: parts[0], Verifier: parts[1], Nonce: parts[2]}, nil
}

// AuthCodeURL is where the browser is sent to sign in.
func (o *OIDC) AuthCodeURL(f Flow) string {
	return o.oauth.AuthCodeURL(f.State,
		oidc.Nonce(f.Nonce),
		oauth2.SetAuthURLParam("code_challenge", s256(f.Verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// Exchange turns an authorization code into a verified identity.
func (o *OIDC) Exchange(ctx context.Context, code string, f Flow) (Identity, error) {
	idToken, err := o.verifiedIDToken(ctx, code, f)
	if err != nil {
		return Identity{}, err
	}
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("reading the id_token claims: %w", err)
	}
	groupsClaim := o.cfg.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	id := Identity{
		Provider: o.cfg.Name,
		Subject:  idToken.Subject,
		Email:    strings.ToLower(strings.TrimSpace(stringClaim(claims, "email"))),
		Name:     stringClaim(claims, "name"),
		Groups:   stringsClaim(claims, groupsClaim),
	}
	if err := o.checkIdentity(id, claims, groupsClaim); err != nil {
		return id, err
	}
	if id.Name == "" {
		id.Name = id.Email
	}
	return id, nil
}

// verifiedIDToken redeems the code and checks the id_token it returns.
func (o *OIDC) verifiedIDToken(ctx context.Context, code string, f Flow) (*oidc.IDToken, error) {
	token, err := o.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", f.Verifier))
	if err != nil {
		return nil, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("the identity provider returned no id_token")
	}
	idToken, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("verifying the id_token: %w", err)
	}
	if idToken.Nonce != f.Nonce {
		// The token was minted for a different login.
		return nil, errors.New("the id_token nonce does not match this login")
	}
	return idToken, nil
}

// checkIdentity refuses an identity Keera cannot safely place or give a role.
func (o *OIDC) checkIdentity(id Identity, claims map[string]any, groupsClaim string) error {
	// Entra replaces the groups claim with a pointer to Microsoft Graph when a
	// person is in too many groups. Keera does not call Graph, so that person
	// would silently get the default role. Refuse, but only where groups decide
	// a role.
	if len(id.Groups) == 0 && o.cfg.Mapping.UsesGroups() && hasClaimOverage(claims, groupsClaim) {
		return fmt.Errorf("the identity provider replaced the %q claim with a "+
			"pointer to its directory API because this account is in too many groups, "+
			"so Keera cannot see the groups that decide this person's role; grant the "+
			"role by address, or map the roles Keera needs onto app roles and set the "+
			"groups claim to %q", groupsClaim, "roles")
	}
	// Without an email there is nothing to attribute an audit entry to, and
	// nothing to place the person in a tenant with.
	if id.Email == "" {
		return errors.New("the identity provider returned no email claim; " +
			"add the 'email' scope, or map an email claim for this client")
	}
	// The email picks the tenant and can name an operator, so it must not be
	// one the directory calls unverified. A missing claim is trusted: most
	// enterprise providers issue none.
	if verified, present := boolClaim(claims, "email_verified"); present && !verified {
		return fmt.Errorf("the identity provider says %s is not a verified "+
			"address, and Keera places a sign-in in a tenant by its email domain; "+
			"verify it in the directory, or map a verified claim for this client", id.Email)
	}
	return nil
}

// LogoutURL returns the provider's end-session endpoint, if it advertises one,
// so that signing out of Keera Gateway also signs out of the identity provider.
func (o *OIDC) LogoutURL(redirectTo string) string {
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := o.provider.Claims(&meta); err != nil || meta.EndSession == "" {
		return ""
	}
	if redirectTo == "" {
		return meta.EndSession
	}
	sep := "?"
	if strings.Contains(meta.EndSession, "?") {
		sep = "&"
	}
	return meta.EndSession + sep + "post_logout_redirect_uri=" + url.QueryEscape(redirectTo)
}

func stringClaim(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}

// hasClaimOverage reports whether the provider left a pointer where a claim
// should be: the OpenID Connect "aggregated claim" shape Entra uses for group
// overage.
func hasClaimOverage(claims map[string]any, key string) bool {
	names, ok := claims["_claim_names"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = names[key]
	return ok
}

// boolClaim reads a claim that is a boolean in the specification and a string
// in some providers. It also reports whether the claim was present, because
// absent (not tracked) and false (failed) mean different things.
func boolClaim(claims map[string]any, key string) (value, present bool) {
	switch v := claims[key].(type) {
	case bool:
		return v, true
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true"), true
	default:
		return false, false
	}
}

// stringsClaim reads a claim that providers send as a list, a single string or
// a space-separated string.
func stringsClaim(claims map[string]any, key string) []string {
	switch v := claims[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		return strings.Fields(v)
	default:
		return nil
	}
}

// SessionTTL is how long a browser stays signed in.
const SessionTTL = 12 * time.Hour

// FlowTTL bounds how long a login may sit half-finished.
const FlowTTL = 15 * time.Minute
