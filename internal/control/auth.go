package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

const sessionCookie = "keera_session"

// operatorKeyExternalID is the stand-in user the operator key signs in as. No
// identity provider can issue this subject, so it never matches a real person.
const operatorKeyExternalID = "keera:operator-key"

// authConfig tells the panel how to offer signing in, before anybody has.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	providers := make([]map[string]string, 0, len(s.opts.Providers))
	for _, p := range s.opts.Providers {
		providers = append(providers, map[string]string{
			"name": p.Name(), "label": p.Label(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sso": s.opts.Providers.Enabled(),
		// One button per provider.
		"providers": providers,
		// Without an operator key, the panel should not offer a field for it.
		"operator_key": s.hasOperatorKey,
	})
}

// login starts the authorization code flow.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Providers.Enabled() {
		httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error", "sso_not_configured",
			"no identity provider is configured; sign in with the operator key, or set KEERA_OIDC_ISSUER")
		return
	}
	provider, err := s.providerFor(r.URL.Query().Get("provider"))
	if err != nil {
		// Back to the sign-in screen, not a dead end, for a stale link.
		s.signInFailed(w, r, err)
		return
	}
	// Where this sign-in ends: usually the panel, or for `keera login` a
	// loopback port. See cliauth.go.
	cli, err := cliLoginFrom(r.URL.Query())
	if err != nil {
		s.signInFailed(w, r, err)
		return
	}
	flow, err := authn.NewFlow()
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.CreateLoginFlow(r.Context(), store.LoginFlow{
		State:    flow.State,
		Verifier: flow.Verifier,
		Nonce:    flow.Nonce,
		Provider: provider.Name(),
		// Only a path is kept, never a full URL, so ?next= is not an open
		// redirect.
		RedirectTo:   safeRedirect(r.URL.Query().Get("next")),
		CLIRedirect:  cli.Redirect,
		CLIChallenge: cli.Challenge,
		CLIState:     cli.State,
	}, time.Now().Add(authn.FlowTTL)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, provider.AuthCodeURL(flow), http.StatusFound)
}

// providerFor resolves which identity provider a sign-in names.
//
// Naming none is fine when there is only one. With several, it is refused
// rather than guessed: the wrong directory could put an account in the wrong
// tenant.
func (s *Server) providerFor(name string) (*authn.OIDC, error) {
	if name == "" {
		if only := s.opts.Providers.Only(); only != nil {
			return only, nil
		}
		return nil, errors.New("this gateway has more than one identity provider, " +
			"so a sign-in has to name one of: " + strings.Join(s.opts.Providers.Names(), ", "))
	}
	if p := s.opts.Providers.ByName(name); p != nil {
		return p, nil
	}
	return nil, errors.New(name + " is not an identity provider on this gateway")
}

// callback completes the flow and signs the person in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Providers.Enabled() {
		s.signInFailed(w, r, errors.New("no identity provider is configured"))
		return
	}
	q := r.URL.Query()
	if desc := q.Get("error"); desc != "" {
		if d := q.Get("error_description"); d != "" {
			desc = d
		}
		s.signInFailed(w, r, errors.New(desc))
		return
	}

	// Taking the flow deletes it, so a replayed callback finds nothing.
	flow, err := s.st.TakeLoginFlow(r.Context(), q.Get("state"))
	if err != nil {
		s.signInFailed(w, r, errors.New("this sign-in link has expired or was already used"))
		return
	}
	// The provider comes from the flow, not the URL, so a callback cannot be
	// sent to a different client than the one that issued the code.
	provider := s.opts.Providers.ByName(flow.Provider)
	if provider == nil {
		s.signInFailed(w, r, errors.New("the identity provider this sign-in started "+
			"against is no longer configured"))
		return
	}
	identity, err := provider.Exchange(r.Context(), q.Get("code"), authn.Flow{
		State: flow.State, Verifier: flow.Verifier, Nonce: flow.Nonce,
	})
	if err != nil {
		s.log.Warn("sign-in failed", "error", err)
		s.signInFailed(w, r, err)
		return
	}
	user, was, ok := s.linkIdentity(w, r, provider, identity)
	if !ok {
		return
	}
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, err)
		return
	}
	entry := map[string]any{"role": user.Role, "via": "sso", "provider": identity.Provider}
	if flow.CLI() {
		// Recorded apart, because "signed in at a terminal" is what an
		// administrator reading the log looks for.
		entry["client"] = "command line"
	}
	if was != "" && was != user.Role {
		// The directory changed the role. This entry is the only record of it.
		entry["role_was"] = was
	}
	s.auditf(r, &authn.Principal{Via: authn.MethodSession, Email: user.Email, UserID: user.ID},
		user.OrgID, "auth.sign_in", "user", user.ID, entry)

	if flow.CLI() {
		// The browser goes on to the port `keera login` waits on. The session
		// above is created either way, so the browser is signed in to the
		// panel too.
		s.handOverToCLI(w, r, flow, user)
		return
	}
	http.Redirect(w, r, orRoot(flow.RedirectTo), http.StatusFound)
}

// linkIdentity finds or creates the user for a signed-in identity and applies
// the role the directory gives them. It also returns the role they had
// before.
func (s *Server) linkIdentity(w http.ResponseWriter, r *http.Request, provider *authn.OIDC,
	identity authn.Identity,
) (user store.User, was string, ok bool) {
	orgID, err := s.orgFor(r, identity)
	if err != nil {
		s.signInFailed(w, r, err)
		return user, "", false
	}
	role := provider.Mapping().RoleFor(identity.Email, identity.Groups)

	user, err = s.st.LinkUser(r.Context(), id.New("user"), store.Link{
		OrgID:        orgID,
		Email:        identity.Email,
		ExternalID:   identity.ExternalID(),
		Role:         string(role),
		AdoptByEmail: s.opts.OIDCAdoptByEmail,
	})
	if errors.Is(err, store.ErrEmailTaken) {
		s.log.Warn("sign-in refused: the address belongs to another provider's identity",
			"email", identity.Email, "provider", identity.Provider)
		s.signInFailed(w, r, errors.New(identity.Email+" already belongs to an account "+
			"from a different identity provider; sign in the way that account was created, "+
			"or ask an operator to move it"))
		return user, "", false
	}
	if err != nil {
		s.fail(w, err)
		return user, "", false
	}
	if user.Disabled() {
		s.log.Warn("sign-in refused: the person is disabled",
			"user", user.ID, "provider", identity.Provider)
		s.signInFailed(w, r, errors.New("your access to Keera has been turned off; "+
			"ask an administrator of your organisation"))
		return user, "", false
	}
	was = user.Role
	if err := s.syncRole(r, &user, role); err != nil {
		s.fail(w, err)
		return user, "", false
	}
	return user, was, true
}

// orgFor decides which tenant a sign-in belongs to: where the person already
// is, else the organisation of their email domain, else the only one there
// is. Anything else is refused, because the wrong tenant is worse than none.
func (s *Server) orgFor(r *http.Request, identity authn.Identity) (string, error) {
	if u, err := s.st.UserByExternalID(r.Context(), identity.ExternalID()); err == nil {
		return u.OrgID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if org, err := s.st.OrgByEmailDomain(r.Context(), identity.Domain()); err == nil {
		return org.ID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if org, err := s.st.OnlyOrg(r.Context()); err == nil {
		return org.ID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return "", errors.New("no organisation matches " + identity.Domain() +
		"; an operator has to create one and set its email domain")
}

// localLogin signs an operator in with the operator key.
//
// It is the way in before an identity provider is set up. It grants only what
// the key already grants, but behind a session, so the panel can be used.
func (s *Server) localLogin(w http.ResponseWriter, r *http.Request) {
	// With no key configured, comparing would accept sha256(""), which anyone
	// can send.
	if !s.hasOperatorKey {
		httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error",
			"operator_key_not_configured",
			"no operator key is configured; sign in through the identity provider, "+
				"or set KEERA_OPERATOR_KEY")
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, "a 'key' is required")
		return
	}
	if !s.matchesOperatorKey(strings.TrimSpace(in.Key)) {
		// The same message whatever was wrong with it.
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
			"that operator key is not valid")
		return
	}

	// The operator key has no identity, so the session belongs to a stand-in
	// user whose audit entries say so.
	orgID, err := s.operatorKeyOrg(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	user, err := s.st.LinkUser(r.Context(), id.New("user"), store.Link{
		OrgID: orgID, Email: "operator@localhost",
		ExternalID: operatorKeyExternalID, Role: string(authn.RoleOperator),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.syncRole(r, &user, authn.RoleOperator); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, &authn.Principal{Via: authn.MethodOperatorKey}, user.OrgID,
		"auth.sign_in", "user", user.ID, map[string]any{"via": "operator key"})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// operatorKeyOrg is the organisation of the operator key's stand-in: where it
// already is, else the first organisation, else a new one.
//
// It does not ask for the only organisation: once a second exists, that
// would create a fresh "Keera" on every sign-in.
func (s *Server) operatorKeyOrg(ctx context.Context) (string, error) {
	if u, err := s.st.UserByExternalID(ctx, operatorKeyExternalID); err == nil {
		return u.OrgID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if org, err := s.st.FirstOrg(ctx); err == nil {
		return org.ID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	org, err := s.st.CreateOrg(ctx, id.New("org"), "Keera")
	if err != nil {
		return "", err
	}
	return org.ID, nil
}

// syncRole updates a stored role to what this sign-in says, for the part the
// identity provider decides. See roleAtSignIn.
func (s *Server) syncRole(r *http.Request, user *store.User, role authn.Role) error {
	want, change := roleAtSignIn(user.Role, role, s.opts.Providers.AdminFromDirectory())
	if !change {
		return nil
	}
	if err := s.st.SetUserRole(r.Context(), user.ID, string(want)); err != nil {
		return err
	}
	user.Role = string(want)
	return nil
}

// roleAtSignIn is the rule syncRole applies. stored is the role in the
// database, fromIDP what this sign-in's claims map to, and adminFromDirectory
// whether a group decides the admin role on this deployment.
//
// The operator role always follows the configuration, so someone removed from
// it is demoted at their next sign-in. The admin role follows the directory
// only where a group decides it. Elsewhere it is set in Keera, and the stored
// role stands.
func roleAtSignIn(stored string, fromIDP authn.Role, adminFromDirectory bool) (authn.Role, bool) {
	if !adminFromDirectory &&
		fromIDP != authn.RoleOperator && stored != string(authn.RoleOperator) {
		return authn.Role(stored), false
	}
	return fromIDP, stored != string(fromIDP)
}

// startSession creates a session and sets its cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user store.User) error {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(token))

	var csrfRaw [24]byte
	if _, err := rand.Read(csrfRaw[:]); err != nil {
		return err
	}
	csrf := base64.RawURLEncoding.EncodeToString(csrfRaw[:])

	expires := time.Now().Add(authn.SessionTTL)
	if err := s.st.CreateSession(r.Context(), sum[:], user.ID, csrf, expires,
		r.Header.Get("User-Agent"), clientIP(r)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.opts.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// sessionPrincipal resolves the session cookie.
func (s *Server) sessionPrincipal(r *http.Request) (*authn.Principal, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, authn.ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(c.Value))
	su, err := s.st.LookupSession(r.Context(), sum[:])
	if err != nil {
		return nil, authn.ErrUnauthenticated
	}
	if err := s.st.TouchSession(r.Context(), sum[:]); err != nil {
		s.log.Warn("recording session activity failed", "error", err)
	}
	return &authn.Principal{
		Via:            authn.MethodSession,
		UserID:         su.User.ID,
		Email:          su.User.Email,
		Role:           authn.Role(su.User.Role),
		OrgID:          su.User.OrgID,
		CSRF:           su.Session.CSRF,
		CredentialHash: sum[:],
	}, nil
}

// logout ends the session, and returns the identity provider's own sign-out
// URL when it has one.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	// ?all ends every session and CLI token of this person, including this
	// one: the answer to a lost laptop.
	switch {
	case httpx.Flag(r.URL.Query(), "all") && p.UserID != "":
		if err := s.st.DeleteUserSessions(r.Context(), p.UserID); err != nil {
			s.fail(w, err)
			return
		}
	case len(p.CredentialHash) > 0 && p.Via == authn.MethodCLI:
		if err := s.st.DeleteCLIToken(r.Context(), p.CredentialHash); err != nil {
			s.fail(w, err)
			return
		}
	case len(p.CredentialHash) > 0:
		if err := s.st.DeleteSession(r.Context(), p.CredentialHash); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.opts.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	var providerLogout string
	if p := s.providerOf(r, p.UserID); p != nil {
		providerLogout = p.LogoutURL(s.opts.PublicURL)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"provider_logout_url": providerLogout})
}

// providerOf is the identity provider a person signed in through, read from
// their external ID. It is nil for the operator key's stand-in.
func (s *Server) providerOf(r *http.Request, userID string) *authn.OIDC {
	if userID == "" || !s.opts.Providers.Enabled() {
		return nil
	}
	u, err := s.st.UserByID(r.Context(), userID)
	if err != nil {
		return nil
	}
	name, _, ok := strings.Cut(u.ExternalID, ":")
	if !ok {
		return nil
	}
	return s.opts.Providers.ByName(name)
}

// me is what the panel loads first: who you are and what you may do.
func (s *Server) me(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	out := map[string]any{
		"via":                p.Via,
		"user_id":            p.UserID,
		"email":              p.Email,
		"role":               p.Role,
		"org_id":             p.OrgID,
		"unrestricted":       p.Unrestricted(),
		"can_edit_catalogue": p.CanAdminCatalogue(),
		// Whether a hosted model's credential can be typed into the panel, or
		// has to come from an environment variable.
		"can_store_credentials": p.CanAdminCatalogue() && s.opts.Secrets.Enabled(),
		"can_admin_org":         p.CanAdminOrg(p.OrgID),
		// Whether to offer this person a key of their own. The operator key is
		// nobody, so it gets no button.
		"can_issue_own_key": p.UserID != "" && p.CanIssueKeyFor(p.OrgID, p.UserID),
		// Separate from the flag above because another screen reads it.
		"can_revoke_own_key": p.UserID != "" && p.CanRevokeKeyFor(p.OrgID, p.UserID),
		"currency":           s.opts.Currency,
		"csrf":               p.CSRF,
		"sso":                s.opts.Providers.Enabled(),
		// True when a directory group decides roles. The panel then shows
		// roles instead of offering to change them.
		"roles_from_directory": s.opts.Providers.AdminFromDirectory(),
		// Where a developer's editor sends its requests.
		"gateway_url": s.gatewayURL(r),
		// Whether this deployment lends out sandboxes. Without a driver the
		// panel does not show them at all.
		"sandboxes": s.opts.Sandboxes != nil,
	}
	if p.OrgID != "" {
		if name, found := s.orgName(r.Context(), p.OrgID); found {
			out["org_name"] = name
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// orgName looks up an organisation's name. A failed read counts as not found:
// the name is only for display.
func (s *Server) orgName(ctx context.Context, orgID string) (string, bool) {
	orgs, err := s.st.ListOrgs(ctx)
	if err != nil {
		return "", false
	}
	for _, o := range orgs {
		if o.ID == orgID {
			return o.Name, true
		}
	}
	return "", false
}

// gatewayURL is the inference plane as a developer's client reaches it: the
// panel's own origin plus httpx.InferencePrefix. It is the one place that
// address is built.
func (s *Server) gatewayURL(r *http.Request) string {
	return s.publicOrigin(r) + httpx.InferencePrefix
}

// publicOrigin is the scheme and host a browser reaches this panel on.
// PublicURL wins over the Host header, because behind a proxy it is the name
// a browser is known to use.
func (s *Server) publicOrigin(r *http.Request) string {
	if s.opts.PublicURL != "" {
		if u, err := url.Parse(s.opts.PublicURL); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// signInFailed sends the browser back to the panel with a readable error,
// rather than leaving it on a blank callback URL.
func (s *Server) signInFailed(w http.ResponseWriter, r *http.Request, err error) {
	http.Redirect(w, r, "/?sign_in_error="+url.QueryEscape(err.Error()), http.StatusFound)
}

// safeRedirect keeps only a same-site path, so ?next= cannot send someone to
// another host right after they sign in.
//
// It parses rather than checking prefixes because browsers read "/\evil.com"
// as "//evil.com". Parsing rejects that, protocol-relative URLs and absolute
// ones alike.
func safeRedirect(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.Contains(next, `\`) {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return ""
	}
	return next
}

func orRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// clientIP is the address a request came from. It trusts the first entry of
// X-Forwarded-For, which is right behind an ingress that sets it and can be
// forged otherwise, so it is only shown to people and used as a throttle key.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i > 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
