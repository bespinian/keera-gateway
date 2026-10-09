package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/bespinian/keera-gateway/internal/auth"
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
	// "groups" and sometimes "roles". Empty is "groups".
	GroupsClaim string
	Mapping     RoleMapping
	// Domains are the email domains this provider may vouch for. The domain
	// picks the organisation and can name an operator, and a customer's directory
	// admin can give anyone any address. "*" allows any domain, for a provider
	// that proves the address itself, such as Google. Empty allows any too;
	// the configuration refuses that when there are several providers.
	Domains []string
	// SignUp lets a person whose sign-in matches no organisation create one,
	// or accept an invitation. Only for a provider that proves every address,
	// since anyone it vouches for can then become an administrator.
	SignUp bool
}

// AnyDomain in Domains lets a provider vouch for every address.
const AnyDomain = "*"

// vouchesFor reports whether this provider may place someone with an address
// at domain.
func (c OIDCConfig) vouchesFor(domain string) bool {
	if len(c.Domains) == 0 {
		return true
	}
	for _, d := range c.Domains {
		if d == AnyDomain || strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(d), "@"), domain) {
			return true
		}
	}
	return false
}

// vouchesForAll reports whether this provider may place someone with any
// address.
func (c OIDCConfig) vouchesForAll() bool {
	return len(c.Domains) == 0 || slices.Contains(c.Domains, AnyDomain)
}

// ReservedProvider is the provider name the gateway uses for sign-ins of its
// own, such as the operator key's. No identity provider may be called it, so
// none can issue a subject that matches one of them.
const ReservedProvider = "keera"

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
	case c.Name == ReservedProvider || c.Name == PasskeyProvider:
		return fmt.Errorf("%q is reserved for the gateway's own sign-ins; give the "+
			"identity provider another name", c.Name)
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
	Groups   []string
	// RefreshToken lets the gateway ask the directory about this person again
	// later. Empty when the provider issued none.
	RefreshToken string
}

// ExternalID is how this identity is stored. It includes the provider because
// a subject is only unique within the directory that issued it.
func (i Identity) ExternalID() string {
	return i.Provider + ":" + i.Subject
}

// Domain is the email domain, used to place a first sign-in in the right
// organisation.
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
	// google is set for accounts.google.com, which issues refresh tokens for
	// a URL parameter rather than the offline_access scope, and refuses that
	// scope.
	google bool
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
	google := isGoogle(cfg.IssuerURL)
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
		if !google {
			// For a refresh token, so the directory can be asked again.
			scopes = append(scopes, oidc.ScopeOfflineAccess)
		}
	}
	return &OIDC{
		cfg:      cfg,
		google:   google,
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

// isGoogle reports whether an issuer is Google's.
func isGoogle(issuer string) bool {
	u, err := url.Parse(issuer)
	return err == nil && u.Host == "accounts.google.com"
}

// Name is how this provider is addressed in a URL and stored in an identity.
func (o *OIDC) Name() string { return o.cfg.Name }

// Label is what to call this provider in front of a person.
func (o *OIDC) Label() string { return o.cfg.Label() }

// Issuer is the directory this provider signs people in against.
func (o *OIDC) Issuer() string { return o.cfg.IssuerURL }

// IsGoogle reports whether this provider is Google's. Google's ID tokens carry
// no groups claim at all.
func (o *OIDC) IsGoogle() bool { return isGoogle(o.cfg.IssuerURL) }

// Mapping returns how this provider's groups map onto Keera Gateway's roles.
func (o *OIDC) Mapping() RoleMapping { return o.cfg.Mapping }

// SignUp reports whether a person this provider vouches for may create an
// organisation.
func (o *OIDC) SignUp() bool { return o.cfg.SignUp }

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

// SignUp reports whether any provider lets people sign up. The deployment is
// then one where strangers create organisations, so a sign-in is never put
// in an organisation only because it is the only one.
func (p Providers) SignUp() bool {
	for _, o := range p {
		if o.cfg.SignUp {
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
func NewFlow() Flow {
	return Flow{State: auth.RandomToken(), Verifier: auth.RandomToken(), Nonce: auth.RandomToken()}
}

// AuthCodeURL is where the browser is sent to sign in.
func (o *OIDC) AuthCodeURL(f Flow) string {
	opts := []oauth2.AuthCodeOption{
		oidc.Nonce(f.Nonce),
		oauth2.SetAuthURLParam("code_challenge", s256(f.Verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	if o.google {
		opts = append(opts, oauth2.AccessTypeOffline)
	}
	return o.oauth.AuthCodeURL(f.State, opts...)
}

// Exchange turns an authorization code into a verified identity.
func (o *OIDC) Exchange(ctx context.Context, code string, f Flow) (Identity, error) {
	token, err := o.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", f.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("exchanging the authorization code: %w", err)
	}
	idToken, err := o.verifiedIDToken(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if idToken.Nonce != f.Nonce {
		// The token was minted for a different sign-in.
		return Identity{}, errors.New("the id_token nonce does not match this sign-in")
	}
	id, err := o.identity(idToken)
	id.RefreshToken = token.RefreshToken
	return id, err
}

// ErrDirectoryRefused is Recheck's answer when the directory no longer
// vouches for the person: their refresh token was revoked, or the account was
// removed or disabled.
var ErrDirectoryRefused = errors.New("the identity provider no longer accepts this sign-in")

// DirectoryCheckEvery is how often the gateway asks a person's directory
// again while they stay signed in. It bounds how long someone removed from
// the directory, or from an admin group, keeps their access.
const DirectoryCheckEvery = 15 * time.Minute

// Recheck asks the directory about someone again, with the refresh token an
// earlier sign-in returned.
//
// The identity is only filled in when the provider sent a fresh id_token,
// which most do. Without one, an answer still says the account is active, and
// fresh reports false. The returned refresh token is the one to keep: some
// providers rotate it on every use.
func (o *OIDC) Recheck(ctx context.Context, refreshToken string) (id Identity, fresh bool, err error) {
	token, err := o.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok && re.ErrorCode == "invalid_grant" {
			return Identity{}, false, ErrDirectoryRefused
		}
		// Anything else, such as the provider being down, is not an answer
		// about the person.
		return Identity{}, false, fmt.Errorf("refreshing the sign-in: %w", err)
	}
	if _, ok := token.Extra("id_token").(string); !ok {
		return Identity{RefreshToken: token.RefreshToken}, false, nil
	}
	idToken, err := o.verifiedIDToken(ctx, token)
	if err != nil {
		return Identity{}, false, err
	}
	id, err = o.identity(idToken)
	id.RefreshToken = token.RefreshToken
	if err != nil {
		// Signing in with these claims would be refused, so they end the
		// access the earlier sign-in gave.
		return id, false, fmt.Errorf("%w: %w", ErrDirectoryRefused, err)
	}
	return id, true, nil
}

// identity reads and checks the claims of a verified id_token.
func (o *OIDC) identity(idToken *oidc.IDToken) (Identity, error) {
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
		Groups:   stringsClaim(claims, groupsClaim),
	}
	if err := o.checkIdentity(id, claims, groupsClaim); err != nil {
		return id, err
	}
	return id, nil
}

// verifiedIDToken checks the id_token that came with a token response.
func (o *OIDC) verifiedIDToken(ctx context.Context, token *oauth2.Token) (*oidc.IDToken, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("the identity provider returned no id_token")
	}
	idToken, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("verifying the id_token: %w", err)
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
	// nothing to place the person in an organisation with.
	if id.Email == "" {
		return errors.New("the identity provider returned no email claim; " +
			"add the 'email' scope, or map an email claim for this client")
	}
	// A customer's directory can hand out any address, so each provider is
	// only trusted for its own domains.
	if !o.cfg.vouchesFor(id.Domain()) {
		return fmt.Errorf("%s cannot be used with %s sign-in, which is only for "+
			"addresses at %s", id.Email, o.cfg.Label(), strings.Join(o.cfg.Domains, ", "))
	}
	// The email picks the organisation and can name an operator, so it must not be
	// one the directory calls unverified. A missing claim is trusted: most
	// enterprise providers issue none.
	verified, present := boolClaim(claims, "email_verified")
	if present && !verified {
		return fmt.Errorf("the identity provider says %s is not a verified "+
			"address, and Keera places a sign-in in an organisation by its email domain; "+
			"verify it in the directory, or map a verified claim for this client", id.Email)
	}
	// Except where strangers sign up through a provider that vouches for every
	// domain: anyone it lets claim an address could take an invitation to it.
	if !present && o.cfg.SignUp && o.cfg.vouchesForAll() {
		return fmt.Errorf("%s sign-in allows sign-up for any address, so it must prove "+
			"each one, and the identity provider did not say %s is verified; send the "+
			"email_verified claim, or limit this provider to its own domains", o.cfg.Label(), id.Email)
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
