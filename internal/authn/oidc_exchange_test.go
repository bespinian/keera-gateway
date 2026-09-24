package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeIDP is a minimal but real OpenID provider: discovery, a JWK set, and a
// token endpoint that mints a properly signed id_token. It exists so the whole
// sign-in exchange is exercised - including signature verification and the
// nonce check - rather than assumed to work.
type fakeIDP struct {
	*httptest.Server
	key *rsa.PrivateKey
	// claims is what the next id_token will carry, so a test can bend one field.
	claims func(m map[string]any)
	// lastForm records what the client actually posted, which is where PKCE
	// either happened or did not.
	lastForm url.Values
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{key: key}

	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                idp.URL,
			"authorization_endpoint":                idp.URL + "/authorize",
			"token_endpoint":                        idp.URL + "/token",
			"jwks_uri":                              idp.URL + "/jwks",
			"end_session_endpoint":                  idp.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.lastForm = r.PostForm
		claims := map[string]any{
			"iss":    idp.URL,
			"aud":    "keera",
			"sub":    "sub-123",
			"exp":    time.Now().Add(time.Hour).Unix(),
			"iat":    time.Now().Unix(),
			"nonce":  r.PostForm.Get("__nonce"),
			"email":  "ada@example.ch",
			"name":   "Ada Lovelace",
			"groups": []string{"engineering", "keera-admins"},
		}
		if idp.claims != nil {
			idp.claims(claims)
		}
		writeJSON(w, map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idp.sign(t, claims),
		})
	})
	return idp
}

func (idp *fakeIDP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// connect wires Keera Gateway to the fake provider.
func connect(t *testing.T, idp *fakeIDP, mapping RoleMapping) *OIDC {
	t.Helper()
	o, err := NewOIDC(context.Background(), OIDCConfig{
		Name:         "test",
		IssuerURL:    idp.URL,
		ClientID:     "keera",
		ClientSecret: "secret",
		RedirectURL:  "https://keera.example.ch/auth/callback",
		GroupsClaim:  "groups",
		Mapping:      mapping,
	}, idp.Client())
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	if o == nil {
		t.Fatal("NewOIDC returned nothing for a configured provider")
	}
	return o
}

func TestAuthCodeURLCarriesStateAndPKCE(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(o.AuthCodeURL(flow))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()

	if q.Get("state") != flow.State {
		t.Errorf("state = %q, want the flow's", q.Get("state"))
	}
	if q.Get("nonce") != flow.Nonce {
		t.Errorf("nonce = %q, want the flow's", q.Get("nonce"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	// The challenge must be the hash of the verifier, never the verifier.
	sum := sha256.Sum256([]byte(flow.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if q.Get("code_challenge") != want {
		t.Errorf("code_challenge = %q, want the S256 of the verifier", q.Get("code_challenge"))
	}
	if strings.Contains(u.String(), flow.Verifier) {
		t.Error("the PKCE verifier was sent to the browser, which defeats the point of it")
	}
	if q.Get("redirect_uri") != "https://keera.example.ch/auth/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
}

func TestExchangeReturnsAVerifiedIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{AdminGroups: []string{"keera-admins"}, Default: RoleMember})

	flow, _ := NewFlow()
	// The stand-in provider echoes back whatever nonce it is told to.
	idp.claims = func(m map[string]any) { m["nonce"] = flow.Nonce }

	id, err := o.Exchange(context.Background(), "the-code", flow)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "sub-123" || id.Email != "ada@example.ch" || id.Name != "Ada Lovelace" {
		t.Errorf("identity = %+v", id)
	}
	if !slices.Contains(id.Groups, "keera-admins") {
		t.Errorf("groups = %v, want the directory's", id.Groups)
	}
	if got := o.Mapping().RoleFor(id.Email, id.Groups); got != RoleAdmin {
		t.Errorf("role = %q, want admin from the group claim", got)
	}
	if id.Domain() != "example.ch" {
		t.Errorf("domain = %q", id.Domain())
	}

	// PKCE has to reach the token endpoint, or it protected nothing.
	if got := idp.lastForm.Get("code_verifier"); got != flow.Verifier {
		t.Errorf("code_verifier posted = %q, want the flow's", got)
	}
}

func TestExchangeRejectsAMismatchedNonce(t *testing.T) {
	// A token minted for a different login must not be accepted for this one.
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})
	idp.claims = func(m map[string]any) { m["nonce"] = "some-other-login" }

	flow, _ := NewFlow()
	if _, err := o.Exchange(context.Background(), "the-code", flow); err == nil {
		t.Fatal("a token with the wrong nonce was accepted")
	} else if !strings.Contains(err.Error(), "nonce") {
		t.Errorf("error = %v, want it to name the nonce", err)
	}
}

func TestExchangeRejectsAForeignSignature(t *testing.T) {
	// The whole point of the JWKS fetch: a token signed by anybody else fails.
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp.key = other // the JWK set still advertises the original public key

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) { m["nonce"] = flow.Nonce }
	if _, err := o.Exchange(context.Background(), "the-code", flow); err == nil {
		t.Fatal("a token signed by an unknown key was accepted")
	}
}

func TestExchangeRejectsATokenForAnotherClient(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) {
		m["nonce"] = flow.Nonce
		m["aud"] = "some-other-application"
	}
	if _, err := o.Exchange(context.Background(), "the-code", flow); err == nil {
		t.Fatal("a token issued for another client was accepted")
	}
}

func TestExchangeRejectsAnExpiredToken(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) {
		m["nonce"] = flow.Nonce
		m["exp"] = time.Now().Add(-time.Minute).Unix()
	}
	if _, err := o.Exchange(context.Background(), "the-code", flow); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

func TestExchangeRefusesAnIdentityWithNoEmail(t *testing.T) {
	// Without an email there is nothing to attribute an audit entry to and no
	// way to place the person in a tenant, so signing in is refused rather than
	// half-completed.
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) {
		m["nonce"] = flow.Nonce
		delete(m, "email")
	}
	_, err := o.Exchange(context.Background(), "the-code", flow)
	if err == nil {
		t.Fatal("an identity with no email was accepted")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error = %v, want it to say what is missing and how to fix it", err)
	}
}

// An email address places a first sign-in in a tenant and can name an operator
// outright, so an address the directory will not vouch for cannot be allowed to
// do either. Providers that federate guest, external or self-registered
// accounts are where an unverified one comes from.
func TestExchangeRefusesAnUnverifiedEmail(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{OperatorEmails: []string{"ada@example.ch"}, Default: RoleMember})

	for _, claim := range []any{false, "false"} {
		flow, _ := NewFlow()
		idp.claims = func(m map[string]any) {
			m["nonce"] = flow.Nonce
			m["email_verified"] = claim
		}
		_, err := o.Exchange(context.Background(), "the-code", flow)
		if err == nil {
			t.Fatalf("an identity with email_verified=%v (%T) was accepted", claim, claim)
		}
		if !strings.Contains(err.Error(), "verified") {
			t.Errorf("error = %v, want it to name the unverified address", err)
		}
	}
}

// The claim is optional, and plenty of enterprise providers issue none. A
// directory that says nothing is trusted as it always was - refusing there
// would lock out the deployments this product is sold into.
func TestExchangeAcceptsAVerifiedOrUnstatedEmail(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	for _, claim := range []any{true, "true", nil} {
		flow, _ := NewFlow()
		idp.claims = func(m map[string]any) {
			m["nonce"] = flow.Nonce
			if claim == nil {
				delete(m, "email_verified")
				return
			}
			m["email_verified"] = claim
		}
		id, err := o.Exchange(context.Background(), "the-code", flow)
		if err != nil {
			t.Fatalf("email_verified=%v (%T): %v", claim, claim, err)
		}
		if id.Email != "ada@example.ch" {
			t.Errorf("email = %q, want the claim's", id.Email)
		}
	}
}

func TestExchangeReadsAGroupsClaimUnderAnotherName(t *testing.T) {
	idp := newFakeIDP(t)
	o, err := NewOIDC(context.Background(), OIDCConfig{
		Name:      "test",
		IssuerURL: idp.URL, ClientID: "keera", ClientSecret: "s",
		RedirectURL: "https://keera.example.ch/auth/callback",
		GroupsClaim: "roles",
		Mapping:     RoleMapping{OperatorGroups: []string{"platform"}, Default: RoleMember},
	}, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) {
		m["nonce"] = flow.Nonce
		m["roles"] = []string{"platform"}
	}
	id, err := o.Exchange(context.Background(), "the-code", flow)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Mapping().RoleFor(id.Email, id.Groups); got != RoleOperator {
		t.Errorf("role = %q, want operator from the 'roles' claim", got)
	}
}

func TestLogoutURLUsesTheProvidersEndSession(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	got := o.LogoutURL("https://keera.example.ch")
	if !strings.HasPrefix(got, idp.URL+"/logout") {
		t.Fatalf("LogoutURL = %q, want the provider's end-session endpoint", got)
	}
	if !strings.Contains(got, "post_logout_redirect_uri=https%3A%2F%2Fkeera.example.ch") {
		t.Errorf("LogoutURL = %q, want an escaped redirect back to us", got)
	}
	if o.LogoutURL("") == "" {
		t.Error("a provider that advertises end-session should still be used with no redirect")
	}
}

func TestNewOIDCFailsLoudlyOnABadIssuer(t *testing.T) {
	// Discovery happens at start-up precisely so a typo is a start-up failure
	// rather than a mystery at the first sign-in.
	_, err := NewOIDC(context.Background(), OIDCConfig{
		Name:      "test",
		IssuerURL: "http://127.0.0.1:1", ClientID: "keera", ClientSecret: "s",
		RedirectURL: "https://keera.example.ch/auth/callback",
	}, nil)
	if err == nil {
		t.Fatal("an unreachable issuer was accepted")
	}
	if !strings.Contains(err.Error(), "discovering") {
		t.Errorf("error = %v, want it to say what failed", err)
	}
}

// Entra stops putting groups in the token once somebody is in more of them than
// it will inline, and leaves a pointer to Microsoft Graph instead. Keera does
// not call Graph, so what arrives looks exactly like a person in no groups -
// and the people it happens to are the ones in the most groups, which is to say
// the administrators.
func TestExchangeRefusesAGroupsClaimTheProviderReplacedWithAPointer(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{AdminGroups: []string{"keera-admins"}, Default: RoleMember})

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) {
		m["nonce"] = flow.Nonce
		delete(m, "groups")
		m["_claim_names"] = map[string]any{"groups": "src1"}
		m["_claim_sources"] = map[string]any{
			"src1": map[string]any{"endpoint": "https://graph.microsoft.com/v1.0/me/getMemberObjects"},
		}
	}
	_, err := o.Exchange(context.Background(), "the-code", flow)
	if err == nil {
		t.Fatal("an overage pointer was accepted, and would have signed an administrator in as a member")
	}
	if !strings.Contains(err.Error(), "too many groups") {
		t.Errorf("error = %v, want it to name the cause", err)
	}
}

// The same token is fine where no role depends on a group. A deployment that
// grants roles by address has nothing to lose by the claim being absent, and
// refusing the sign-in would be a failure invented out of nothing.
func TestExchangeAllowsAnOverageWhenNoRoleDependsOnGroups(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{OperatorEmails: []string{"ada@example.ch"}, Default: RoleMember})

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) {
		m["nonce"] = flow.Nonce
		delete(m, "groups")
		m["_claim_names"] = map[string]any{"groups": "src1"}
		m["_claim_sources"] = map[string]any{
			"src1": map[string]any{"endpoint": "https://graph.microsoft.com/v1.0/me/getMemberObjects"},
		}
	}
	id, err := o.Exchange(context.Background(), "the-code", flow)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := o.Mapping().RoleFor(id.Email, id.Groups); got != RoleOperator {
		t.Errorf("role = %q, want operator from the address", got)
	}
}

// An identity has to carry the provider that vouched for it: a subject is only
// ever unique inside the directory that issued it, and two directories writing
// to one key is how somebody signs in as somebody else.
func TestExchangeStampsTheProviderOnTheIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	o := connect(t, idp, RoleMapping{Default: RoleMember})

	flow, _ := NewFlow()
	idp.claims = func(m map[string]any) { m["nonce"] = flow.Nonce }
	id, err := o.Exchange(context.Background(), "the-code", flow)
	if err != nil {
		t.Fatal(err)
	}
	if id.Provider != "test" {
		t.Errorf("provider = %q, want the one that issued the token", id.Provider)
	}
	if id.ExternalID() != "test:sub-123" {
		t.Errorf("ExternalID = %q, want the subject qualified by its provider", id.ExternalID())
	}
}

func TestProvidersResolveByName(t *testing.T) {
	idp := newFakeIDP(t)
	base := func(name string) OIDCConfig {
		return OIDCConfig{
			Name: name, IssuerURL: idp.URL, ClientID: "keera", ClientSecret: "s",
			RedirectURL: "https://keera.example.ch/control/auth/callback",
			Mapping:     RoleMapping{Default: RoleMember},
		}
	}
	ctx := context.Background()

	ps, err := NewProviders(ctx, []OIDCConfig{base("google"), base("entra")}, idp.Client())
	if err != nil {
		t.Fatalf("NewProviders: %v", err)
	}
	if !ps.Enabled() || len(ps) != 2 {
		t.Fatalf("got %d providers, want 2", len(ps))
	}
	if ps.ByName("entra") == nil || ps.ByName("google") == nil {
		t.Error("a configured provider was not found by name")
	}
	if ps.ByName("okta") != nil {
		t.Error("an unconfigured provider was found by name")
	}
	// Two providers mean a sign-in has to say which, so there is no default.
	if ps.Only() != nil {
		t.Error("Only returned a provider where there is a choice to make")
	}

	one, err := NewProviders(ctx, []OIDCConfig{base("google")}, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	if one.Only() == nil {
		t.Error("Only returned nothing where there is nothing to choose between")
	}

	// Two providers under one name would have the second silently shadow the
	// first, and every identity the first wrote would resolve to the wrong
	// directory.
	if _, err := NewProviders(ctx, []OIDCConfig{base("google"), base("google")}, idp.Client()); err == nil {
		t.Error("two providers were accepted under one name")
	}

	// Nothing configured is not an error: the operator key is the deployment's
	// first ten minutes.
	none, err := NewProviders(ctx, []OIDCConfig{{Name: "google"}}, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	if none.Enabled() {
		t.Error("an unconfigured provider reported itself as enabled")
	}
}

// Where a group decides the administrator role, the directory owns it and the
// panel says so rather than offering a control the next sign-in would undo.
// That is one answer for the whole gateway: a hosted deployment where Entra
// decides roles and Google cannot would otherwise have to explain, per person,
// which of the two a reader is looking at.
func TestAdminFromDirectoryIsTrueIfAnyProviderMapsAdminGroups(t *testing.T) {
	idp := newFakeIDP(t)
	base := func(name string, m RoleMapping) OIDCConfig {
		return OIDCConfig{
			Name: name, IssuerURL: idp.URL, ClientID: "keera", ClientSecret: "s",
			RedirectURL: "https://keera.example.ch/control/auth/callback",
			Mapping:     m,
		}
	}
	ctx := context.Background()

	// Google issues no groups claim, so roles there are Keera's own.
	google := RoleMapping{Default: RoleMember, OperatorEmails: []string{"you@example.ch"}}
	// Operator groups alone do not make it the directory's: the operator role is
	// never assignable in the panel anyway, so nothing is being taken away.
	operatorsOnly := RoleMapping{Default: RoleMember, OperatorGroups: []string{"keera-operators"}}
	entra := RoleMapping{Default: RoleMember, AdminGroups: []string{"keera-admins"}}

	for _, tc := range []struct {
		name string
		cfgs []OIDCConfig
		want bool
	}{
		{"nothing configured", nil, false},
		{"by address only", []OIDCConfig{base("google", google)}, false},
		{"operator groups only", []OIDCConfig{base("entra", operatorsOnly)}, false},
		{"blank entries do not count",
			[]OIDCConfig{base("entra", RoleMapping{AdminGroups: []string{"", " "}})}, false},
		{"admin groups", []OIDCConfig{base("entra", entra)}, true},
		{"one of several",
			[]OIDCConfig{base("google", google), base("entra", entra)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := NewProviders(ctx, tc.cfgs, idp.Client())
			if err != nil {
				t.Fatalf("NewProviders: %v", err)
			}
			if got := ps.AdminFromDirectory(); got != tc.want {
				t.Errorf("AdminFromDirectory = %v, want %v", got, tc.want)
			}
		})
	}
}
