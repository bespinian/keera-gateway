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

// OIDCConfig describes one identity provider. A deployment may have several.
type OIDCConfig struct {
	// Name is used in configuration, the sign-in URL and stored external IDs.
	// Renaming it orphans everyone who signed in through it.
	Name string
	// DisplayName is the sign-in button's text. Empty uses Name.
	DisplayName  string
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// RedirectURL is the absolute callback URL. Providers may share one: the
	// callback tells flows apart by their state.
	RedirectURL string
	Scopes      []string
	// GroupsClaim is where the provider puts group membership. Empty is
	// "groups".
	GroupsClaim string
	Mapping     RoleMapping
	// Domains are the email domains this provider may vouch for. The domain
	// picks the organisation and can name an operator, and a directory admin
	// can give anyone any address. "*" allows any, for a provider that proves
	// the address itself, such as Google. Empty allows any too, which config
	// refuses for GitHub, for LDAP and when there are several providers.
	Domains []string
	// SignUp lets a person who matches no organisation create one, or accept
	// an invitation. Only for a provider that proves every address, since
	// anyone it vouches for can then become an administrator.
	SignUp bool
	// LDAP is set when the issuer is an ldap:// or ldaps:// URL.
	LDAP LDAPConfig
}

// AnyDomain in Domains lets a provider vouch for every address.
const AnyDomain = "*"

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

func (c OIDCConfig) vouchesForAll() bool {
	return len(c.Domains) == 0 || slices.Contains(c.Domains, AnyDomain)
}

// ReservedProvider names the gateway's own sign-ins, such as the operator
// key's. No identity provider may use it, so none can issue a matching
// subject.
const ReservedProvider = "keera"

// nameRE keeps provider names safe in a URL and a stored external ID.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Configured reports whether this provider was set up. Without one the control
// plane still works through the operator key.
func (c OIDCConfig) Configured() bool {
	return c.IssuerURL != "" && (c.ClientID != "" || c.IsLDAP())
}

// Label is the provider's name for people.
func (c OIDCConfig) Label() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// Validate checks the shape of a complete configuration. Missing settings are
// for config to report, since it knows their names.
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
	case c.IsLDAP():
		return c.validateLDAP()
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
	Provider string
	Subject  string
	Email    string
	// Name is empty when the provider sent none.
	Name   string
	Groups []string
	// RefreshToken lets the gateway ask the directory again later. Empty when
	// none was issued.
	RefreshToken string
}

// ExternalID includes the provider because a subject is only unique within its
// directory.
func (i Identity) ExternalID() string {
	return i.Provider + ":" + i.Subject
}

// Domain is the email domain, which places a first sign-in in an organisation.
func (i Identity) Domain() string {
	if at := strings.LastIndexByte(i.Email, '@'); at >= 0 {
		return strings.ToLower(i.Email[at+1:])
	}
	return ""
}

type OIDC struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	// google is set for accounts.google.com, which wants a URL parameter for
	// refresh tokens and refuses the offline_access scope.
	google bool
	// github is set for github.com, which speaks OAuth but not OpenID Connect.
	// It leaves provider and verifier nil.
	github *githubAPI
	// ldap has no redirect at all, and also leaves provider and verifier nil.
	ldap *ldapDir
}

// NewOIDC discovers the provider's metadata at start-up, so a typo in the
// issuer URL stops the gateway rather than the first sign-in. cfg has passed
// Validate.
func NewOIDC(ctx context.Context, cfg OIDCConfig, client *http.Client) (*OIDC, error) {
	if !cfg.Configured() {
		return nil, nil
	}
	if cfg.IsLDAP() {
		return newLDAP(cfg)
	}
	if cfg.IsGitHub() {
		return newGitHub(cfg, client, GitHubIssuer, "https://api.github.com"), nil
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
			// For a refresh token.
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

func isGoogle(issuer string) bool {
	u, err := url.Parse(issuer)
	return err == nil && u.Host == "accounts.google.com"
}

func (o *OIDC) Name() string { return o.cfg.Name }

func (o *OIDC) Label() string { return o.cfg.Label() }

func (o *OIDC) Issuer() string { return o.cfg.IssuerURL }

// IsGoogle reports whether this provider is Google's, whose ID tokens carry no
// groups claim.
func (o *OIDC) IsGoogle() bool { return o.google }

func (o *OIDC) Mapping() RoleMapping { return o.cfg.Mapping }

func (o *OIDC) SignUp() bool { return o.cfg.SignUp }

// Providers are the identity providers a deployment offers, in sign-in screen
// order.
type Providers []*OIDC

// NewProviders discovers every provider. One unreachable provider fails the
// start.
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

func (p Providers) Enabled() bool { return len(p) > 0 }

func (p Providers) ByName(name string) *OIDC {
	for _, o := range p {
		if o.cfg.Name == name {
			return o
		}
	}
	return nil
}

// Only returns the provider when there is exactly one, and nil otherwise.
func (p Providers) Only() *OIDC {
	if len(p) == 1 {
		return p[0]
	}
	return nil
}

// AdminFromDirectory reports whether the directory, not Keera, decides the
// administrator role. It is one answer for the whole gateway.
func (p Providers) AdminFromDirectory() bool {
	for _, o := range p {
		if o.cfg.Mapping.DecidesAdmin() {
			return true
		}
	}
	return false
}

// SignUp reports whether any provider lets people sign up. Then a sign-in is
// never put in an organisation only because it is the only one.
func (p Providers) SignUp() bool {
	for _, o := range p {
		if o.cfg.SignUp {
			return true
		}
	}
	return false
}

func (p Providers) Names() []string {
	out := make([]string, 0, len(p))
	for _, o := range p {
		out = append(out, o.cfg.Name)
	}
	return out
}

type Flow struct {
	State    string
	Verifier string
	Nonce    string
}

func NewFlow() Flow {
	return Flow{State: auth.RandomToken(), Verifier: auth.RandomToken(), Nonce: auth.RandomToken()}
}

func (o *OIDC) AuthCodeURL(f Flow) string {
	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", s256(f.Verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	// GitHub issues no id_token to carry a nonce back.
	if o.github == nil {
		opts = append(opts, oidc.Nonce(f.Nonce))
	}
	if o.google {
		opts = append(opts, oauth2.AccessTypeOffline)
	}
	return o.oauth.AuthCodeURL(f.State, opts...)
}

func (o *OIDC) Exchange(ctx context.Context, code string, f Flow) (Identity, error) {
	if o.ldap != nil {
		return Identity{}, errors.New("a password sign-in has no authorization code")
	}
	if o.github != nil {
		return o.exchangeGitHub(ctx, code, f)
	}
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
		return Identity{}, errors.New("the id_token nonce does not match this sign-in")
	}
	id, err := o.identity(idToken)
	id.RefreshToken = token.RefreshToken
	return id, err
}

// ErrDirectoryRefused means the directory no longer vouches for the person.
var ErrDirectoryRefused = errors.New("the identity provider no longer accepts this sign-in")

// DirectoryCheckEvery bounds how long someone removed from the directory, or
// from an admin group, keeps their access.
const DirectoryCheckEvery = 15 * time.Minute

// Recheck asks the directory about someone again with their refresh token.
// The identity is only filled in when a fresh id_token came; without one,
// success still means the account is active. Keep the returned refresh token:
// some providers rotate it.
func (o *OIDC) Recheck(ctx context.Context, refreshToken string) (id Identity, fresh bool, err error) {
	if o.ldap != nil {
		return o.recheckLDAP(ctx, refreshToken)
	}
	if o.github != nil {
		return o.recheckGitHub(ctx, refreshToken)
	}
	token, err := o.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok && re.ErrorCode == "invalid_grant" {
			return Identity{}, false, ErrDirectoryRefused
		}
		// A provider being down is not an answer about the person.
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
		// A sign-in with these claims would be refused, so access ends.
		return id, false, fmt.Errorf("%w: %w", ErrDirectoryRefused, err)
	}
	return id, true, nil
}

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
		Name:     displayName(claims),
		Groups:   stringsClaim(claims, groupsClaim),
	}
	return id, o.checkIdentity(id, claims, groupsClaim)
}

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
	// person is in too many groups. Keera does not call Graph, so refuse where
	// groups decide a role, rather than give the default role.
	if len(id.Groups) == 0 && o.cfg.Mapping.UsesGroups() && hasClaimOverage(claims, groupsClaim) {
		return fmt.Errorf("the identity provider replaced the %q claim with a "+
			"pointer to its directory API because this account is in too many groups, "+
			"so Keera cannot see the groups that decide this person's role; map the "+
			"roles Keera needs onto app roles and set the groups claim to %q",
			groupsClaim, "roles")
	}
	// The email attributes audit entries and places the person.
	if id.Email == "" {
		return errors.New("the identity provider returned no email claim; " +
			"add the 'email' scope, or map an email claim for this client")
	}
	// A directory can hand out any address, so trust only its own domains.
	if !o.cfg.vouchesFor(id.Domain()) {
		return fmt.Errorf("%s cannot be used with %s sign-in, which is only for "+
			"addresses at %s", id.Email, o.cfg.Label(), strings.Join(o.cfg.Domains, ", "))
	}
	// The email picks the organisation and can name an operator, so it must
	// not be unverified. A missing claim is trusted: most enterprise
	// providers send none.
	verified, present := emailVerified(claims)
	if present && !verified {
		return fmt.Errorf("the identity provider says %s is not a verified "+
			"address, and Keera places a sign-in in an organisation by its email domain; "+
			"verify it in the directory, or map a verified claim for this client", id.Email)
	}
	// Except with sign-up on a provider for every domain: anyone it lets claim
	// an address could take an invitation to it.
	if !present && o.cfg.SignUp && o.cfg.vouchesForAll() {
		return fmt.Errorf("%s sign-in allows sign-up for any address, so it must prove "+
			"each one, and the identity provider did not say %s is verified; send the "+
			"email_verified or xms_edov claim, or limit this provider to its own domains", o.cfg.Label(), id.Email)
	}
	return nil
}

// LogoutURL returns the provider's end-session endpoint, if any, so signing
// out of Keera also signs out of the provider.
func (o *OIDC) LogoutURL(redirectTo string) string {
	if o.provider == nil {
		return ""
	}
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

// displayName falls back to the given and family names.
func displayName(claims map[string]any) string {
	if name := strings.TrimSpace(stringClaim(claims, "name")); name != "" {
		return name
	}
	return strings.TrimSpace(strings.TrimSpace(stringClaim(claims, "given_name")) + " " +
		strings.TrimSpace(stringClaim(claims, "family_name")))
}

func stringClaim(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}

// hasClaimOverage detects the "aggregated claim" pointer Entra sends for group
// overage.
func hasClaimOverage(claims map[string]any, key string) bool {
	names, ok := claims["_claim_names"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = names[key]
	return ok
}

// emailVerified also reads Entra's xms_edov, since Entra sends no
// email_verified.
func emailVerified(claims map[string]any) (value, present bool) {
	if v, ok := boolClaim(claims, "email_verified"); ok {
		return v, true
	}
	return boolClaim(claims, "xms_edov")
}

// boolClaim accepts a boolean or a string. It reports presence, since absent
// and false mean different things.
func boolClaim(claims map[string]any, key string) (value, present bool) {
	switch v := claims[key].(type) {
	case bool:
		return v, true
	case string:
		v = strings.TrimSpace(v)
		return strings.EqualFold(v, "true") || v == "1", true
	case float64:
		return v == 1, true
	default:
		return false, false
	}
}

// stringsClaim accepts a list, a string, or a space-separated string.
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
	case string:
		return strings.Fields(v)
	default:
		return nil
	}
}

// SignInTTL is how long a browser session or a `keera login` token lasts: a
// month, since signing in again costs a browser window. The directory is still
// asked again meanwhile.
const SignInTTL = 30 * 24 * time.Hour

// ShortSignInTTL is how long a sign-in lasts when the directory cannot be
// asked again, so a removed person loses access within a working day.
const ShortSignInTTL = 12 * time.Hour

// FlowTTL bounds how long a login may sit half-finished.
const FlowTTL = 15 * time.Minute
