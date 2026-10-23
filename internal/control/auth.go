package control

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

const sessionCookie = "keera_session"

// loginCookie holds the state of the sign-in this browser started, which the
// callback must match. Without it, anyone could send a victim the callback link
// of their own sign-in and silently sign the victim in to their account.
const loginCookie = "keera_login"

// operatorKeyExternalID is the stand-in user the operator key signs in as.
// Its provider is reserved, so no identity provider can issue this subject and
// it never matches a real person.
const operatorKeyExternalID = authn.ReservedProvider + ":operator-key"

// providerList names the configured identity providers for the panel.
func (s *Server) providerList() []map[string]any {
	providers := make([]map[string]any, 0, len(s.opts.Providers))
	for _, p := range s.opts.Providers {
		providers = append(providers, map[string]any{
			"name": p.Name(), "label": p.Label(),
			// Whether somebody new may create an organisation through it.
			"signup": p.SignUp(),
		})
	}
	return providers
}

// authConfig tells the panel how to offer signing in, before anybody has.
func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sso":       s.opts.Providers.Enabled(),
		"providers": s.providerList(),
		"passkeys":  s.opts.Passkeys != nil,
	})
}

// login starts the authorization code flow.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.passkeyLogin(r.URL.Query().Get("provider")) {
		s.startPasskeyLogin(w, r)
		return
	}
	if !s.opts.Providers.Enabled() {
		switchedOff(w, "sso_disabled",
			"no identity provider is configured; sign in with the operator key, or set KEERA_OIDC_PROVIDERS")
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
	cli, err := cliLoginFrom(r.URL.Query(), s.opts.ChatURL)
	if err != nil {
		s.signInFailed(w, r, err)
		return
	}
	flow := authn.NewFlow()
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
	http.SetCookie(w, s.loginStateCookie(flow.State, int(authn.FlowTTL.Seconds())))
	http.Redirect(w, r, provider.AuthCodeURL(flow), http.StatusFound)
}

// loginStateCookie sets loginCookie, or with maxAge -1 removes it. Lax is
// enough: the provider sends the browser back with a plain GET.
func (s *Server) loginStateCookie(state string, maxAge int) *http.Cookie {
	return s.cookie(loginCookie, state, httpx.ControlPrefix+"/auth", maxAge, http.SameSiteLaxMode)
}

// cookie is every cookie the control plane sets: never readable by scripts,
// and sent only over HTTPS where the deployment is served over it. maxAge -1
// removes it.
func (s *Server) cookie(name, value, path string, maxAge int, same http.SameSite) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: s.opts.SecureCookies, SameSite: same,
	}
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
		names := s.opts.Providers.Names()
		if s.opts.Passkeys != nil {
			names = append(names, authn.PasskeyProvider)
		}
		return nil, errors.New("this gateway has more than one way to sign in, " +
			"so a sign-in has to name one of: " + strings.Join(names, ", "))
	}
	if p := s.opts.Providers.ByName(name); p != nil {
		return p, nil
	}
	// The name is not repeated: anyone can write it into a link.
	return nil, errors.New("this sign-in names an identity provider this gateway does not have")
}

// callback completes the flow and signs the person in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Providers.Enabled() {
		s.signInFailed(w, r, errors.New("no identity provider is configured"))
		return
	}
	q := r.URL.Query()
	// The provider's own error is only shown once the state matches: before
	// that, the text could come from anyone's link.
	refused := q.Get("error")
	if refused != "" {
		s.log.Warn("the identity provider refused a sign-in",
			"error", refused, "description", q.Get("error_description"))
	}

	state := q.Get("state")
	if c, err := r.Cookie(loginCookie); err != nil || state == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		if refused != "" {
			s.signInFailed(w, r, errors.New("the identity provider refused the sign-in"))
			return
		}
		s.signInFailed(w, r, errors.New("this sign-in was started in another browser or tab; "+
			"start it again here"))
		return
	}
	http.SetCookie(w, s.loginStateCookie("", -1))
	// Taking the flow deletes it, so a replayed callback finds nothing.
	flow, err := s.st.TakeLoginFlow(r.Context(), state)
	if err != nil {
		s.signInFailed(w, r, errors.New("this sign-in link has expired or was already used"))
		return
	}
	if refused != "" {
		if d := q.Get("error_description"); d != "" {
			refused = d
		}
		s.signInFailed(w, r, errors.New(refused))
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
	place, err := s.orgFor(r, provider, identity)
	if err != nil {
		s.signInFailed(w, r, err)
		return
	}
	if place.signUp {
		if flow.CLI() {
			// The choice is made on a screen of the panel, which this
			// browser is not going to show.
			s.signInFailed(w, r, errors.New("there is no organisation for "+identity.Email+
				" yet; open "+s.publicOrigin(r)+" in a browser to create or join one, "+
				"then run keera login again"))
			return
		}
		s.startSignup(w, r, provider, identity, place.invited, flow.RedirectTo)
		return
	}
	user, was, ok := s.linkIdentity(w, r, provider, identity, place.orgID)
	if !ok {
		return
	}
	s.rememberDirectory(r, user, s.sealRefreshToken(user.ExternalID, identity.RefreshToken))
	if !s.signedIn(w, r, user, was, flow.CLI(),
		map[string]any{"via": "sso", "provider": identity.Provider}) {
		return
	}

	if flow.CLI() {
		// The browser goes on to the port `keera login` waits on. The session
		// above is created either way, so the browser is signed in to the
		// panel too.
		to, err := s.cliHandOver(r, flow, user)
		if err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	http.Redirect(w, r, cmp.Or(flow.RedirectTo, "/"), http.StatusFound)
}

// linkIdentity finds or creates the user for a signed-in identity and applies
// the role the directory gives them. It also returns the role they had
// before.
func (s *Server) linkIdentity(w http.ResponseWriter, r *http.Request, provider *authn.OIDC,
	identity authn.Identity, orgID string,
) (user store.User, was string, ok bool) {
	role := provider.Mapping().RoleFor(identity.Email, identity.Groups)

	user, err := s.st.LinkUser(r.Context(), id.New("user"), store.Link{
		OrgID:      orgID,
		Email:      identity.Email,
		Name:       identity.Name,
		ExternalID: identity.ExternalID(),
		Role:       string(role),
	})
	if errors.Is(err, store.ErrEmailTaken) {
		s.log.Warn("sign-in refused: the address belongs to a different identity",
			"email", identity.Email, "provider", identity.Provider)
		s.signInFailed(w, r, errors.New(identity.Email+" already belongs to an account "+
			"linked to a different identity; sign in the way that account was created, "+
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
	if err := s.syncOtherRoles(r, user, role); err != nil {
		s.fail(w, err)
		return user, "", false
	}
	return user, was, true
}

// placement is where a sign-in lands: in an organisation, or on the sign-up
// screen.
type placement struct {
	orgID  string
	signUp bool
	// invited is the organisation that added the person's address, which the
	// sign-up screen offers to join.
	invited string
}

// orgFor decides which tenant a sign-in belongs to: where the person already
// is, and of several the one they used last, else the organisation of their
// email domain. After that, a provider that allows sign-up sends the person to
// choose; otherwise they land in the only organisation there is. Anything else
// is refused, because the wrong tenant is worse than none.
//
// An invitation is never joined without asking: anyone who administers an
// organisation without a domain can add any address to it.
func (s *Server) orgFor(r *http.Request, provider *authn.OIDC, identity authn.Identity) (placement, error) {
	ms, err := s.st.Memberships(r.Context(), identity.ExternalID(), "", false)
	if err != nil {
		return placement{}, err
	}
	if len(ms) > 0 {
		return placement{orgID: lastOrg(r, ms)}, nil
	}
	if err := s.sameIdentity(r.Context(), identity); err != nil {
		return placement{}, err
	}
	if org, err := s.st.OrgByEmailDomain(r.Context(), identity.Domain()); err == nil {
		return placement{orgID: org.ID}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return placement{}, err
	}
	if provider.SignUp() {
		org, err := s.st.InvitationFor(r.Context(), identity.Email)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return placement{}, err
		}
		return placement{signUp: true, invited: org.ID}, nil
	}
	// Where strangers create organisations, the only one is somebody else's.
	if !s.opts.Providers.SignUp() {
		if org, err := s.st.OnlyOrg(r.Context()); err == nil {
			return placement{orgID: org.ID}, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return placement{}, err
		}
	}
	return placement{}, errors.New("no organisation matches " + identity.Domain() +
		"; an operator has to create one and set its email domain")
}

// localLogin signs an operator in with the operator key, the way in before an
// identity provider is set up. It grants only what the key grants, behind a
// session, so the panel can be used.
func (s *Server) localLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.matchesOperatorKey(strings.TrimSpace(in.Key)) {
		// The same message whatever was wrong with it.
		unauthorized(w, "invalid_api_key",
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
	org, err := s.st.CreateOrg(ctx, store.Org{ID: id.New("org"), Name: "Keera"}, s.opts.Template)
	if err != nil {
		return "", err
	}
	s.announce(ctx, registry.OrgChange(org.ID))
	return org.ID, nil
}

// syncRole updates a stored role to what this sign-in says, for the part the
// identity provider decides. See roleAtSignIn.
//
// The operator role is the person's wherever they are. The admin role is the
// directory's only in the organisation its addresses land in: in any other,
// the directory of one tenant would decide who runs another. There, the role
// the organisation gave stands, and a former operator becomes a member.
func (s *Server) syncRole(r *http.Request, user *store.User, role authn.Role) error {
	want, change := roleAtSignIn(user.Role, role, s.opts.Providers.AdminFromDirectory())
	if !change {
		return nil
	}
	if want != authn.RoleOperator {
		home, err := s.directoryOrg(r.Context(), user.Email)
		if err != nil {
			return err
		}
		if home != user.OrgID {
			if user.Role != string(authn.RoleOperator) {
				return nil
			}
			want = authn.RoleMember
		}
	}
	if err := s.st.SetUserRole(r.Context(), user.ID, string(want)); err != nil {
		return err
	}
	user.Role = string(want)
	return nil
}

// syncOtherRoles applies role to the person's rows in their other
// organisations. The directory is asked once per person, not per row, so
// without this a row it skipped would keep an operator role they have lost.
func (s *Server) syncOtherRoles(r *http.Request, user store.User, role authn.Role) error {
	if user.ExternalID == "" {
		return nil
	}
	ms, err := s.st.Memberships(r.Context(), user.ExternalID, "", false)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.UserID == user.ID {
			continue
		}
		other := store.User{ID: m.UserID, OrgID: m.OrgID, Email: user.Email,
			ExternalID: user.ExternalID, Role: m.Role}
		if err := s.syncRole(r, &other, role); err != nil {
			return err
		}
		if other.Role != m.Role {
			s.auditf(r, actorOf(other), other.OrgID, "auth.directory_check", "user", other.ID,
				map[string]any{"role": other.Role, "role_was": m.Role})
		}
	}
	return nil
}

// roleAtSignIn is the rule syncRole applies. stored is the role in the
// database, fromIDP what this sign-in's claims map to, and adminFromDirectory
// whether a group decides the admin role on this deployment.
//
// The operator role always follows configuration, so someone removed from it
// is demoted at their next sign-in. The admin role follows the directory only
// where a group decides it; elsewhere the stored role stands.
func roleAtSignIn(stored string, fromIDP authn.Role, adminFromDirectory bool) (authn.Role, bool) {
	if !adminFromDirectory &&
		fromIDP != authn.RoleOperator && stored != string(authn.RoleOperator) {
		return authn.Role(stored), false
	}
	return fromIDP, stored != string(fromIDP)
}

// signedIn starts a session for someone who just signed in, and logs it with
// entry. was is their role before, so the log records a change; cli marks a
// sign-in for the command line.
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request, user store.User, was string,
	cli bool, entry map[string]any) bool {
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, err)
		return false
	}
	entry["role"] = user.Role
	if cli {
		// Recorded apart, because "signed in at a terminal" is what an
		// administrator reading the log looks for.
		entry["client"] = "command line"
	}
	if was != "" && was != user.Role {
		// This entry is the only record of the change.
		entry["role_was"] = was
	}
	s.auditf(r, actorOf(user), user.OrgID, "auth.sign_in", "user", user.ID, entry)
	return true
}

// actorOf is the principal a person acts as through their session.
func actorOf(user store.User) *authn.Principal {
	return &authn.Principal{Via: authn.MethodSession, Email: user.Email, UserID: user.ID}
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user store.User) error {
	token, hash, csrf := authn.NewSession()
	expires := time.Now().Add(authn.SessionTTL)
	if err := s.st.CreateSession(r.Context(), hash, user.ID, csrf, expires); err != nil {
		return err
	}
	http.SetCookie(w, s.cookie(sessionCookie, token, "/", int(authn.SessionTTL.Seconds()),
		http.SameSiteLaxMode))
	http.SetCookie(w, s.rememberOrg(user.OrgID))
	return nil
}

func (s *Server) sessionPrincipal(r *http.Request) (*authn.Principal, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, authn.ErrUnauthenticated
	}
	hash := auth.Hash(c.Value)
	su, err := s.st.LookupSession(r.Context(), hash)
	if err != nil {
		return nil, authn.ErrUnauthenticated
	}
	user, ok := s.checkDirectory(r, su.User, su.Directory)
	if !ok {
		return nil, authn.ErrUnauthenticated
	}
	p := principalOf(authn.MethodSession, user, hash)
	p.CSRF = su.Session.CSRF
	p.SignedInAt = su.Session.CreatedAt
	return p, nil
}

// principalOf is the caller a signed-in person is to the routes. hash is the
// credential they sent, hashed, so signing out can end it.
func principalOf(via authn.Method, user store.User, hash []byte) *authn.Principal {
	return &authn.Principal{
		Via:            via,
		UserID:         user.ID,
		Email:          user.Email,
		Name:           user.Name,
		Role:           authn.Role(user.Role),
		OrgID:          user.OrgID,
		CredentialHash: hash,
	}
}

// logout ends the session, and returns the identity provider's own sign-out
// URL when it has one.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	// ?all ends every session and CLI token of this person, in every
	// organisation and including this one: the answer to a lost laptop.
	switch {
	case httpx.Flag(r.URL.Query(), "all") && p.UserID != "":
		if err := s.st.DeletePersonSessions(r.Context(), p.UserID); err != nil {
			s.fail(w, err)
			return
		}
		// Audited, unlike signing out of one browser.
		s.auditf(r, p, p.OrgID, "auth.sign_out_everywhere", "user", p.UserID, nil)
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
	http.SetCookie(w, s.cookie(sessionCookie, "", "/", -1, http.SameSiteLaxMode))
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
	return s.directoryOf(u)
}

// me is what the panel loads first: who you are and what you may do.
func (s *Server) me(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	out := map[string]any{
		"via":           p.Via,
		"user_id":       p.UserID,
		"email":         p.Email,
		"name":          p.Name,
		"role":          p.Role,
		"org_id":        p.OrgID,
		"unrestricted":  p.Unrestricted(),
		"can_admin_org": p.CanAdminOrg(p.OrgID),
		"csrf":          p.CSRF,
		"sso":           s.opts.Providers.Enabled(),
		// So the Users screen can name the provider each person signs in with.
		"providers": s.providerList(),
		// True when a directory group decides roles. The panel then shows
		// roles instead of offering to change them.
		"roles_from_directory": s.opts.Providers.AdminFromDirectory(),
		// Where a developer's editor sends its requests.
		"gateway_url": s.gatewayURL(r),
		// Where Keera Chat runs, so the panel can link to it. Empty without one.
		"chat_url": s.opts.ChatURL,
		// Whether this deployment lends out sandboxes. Without a driver the
		// panel does not show them at all.
		"sandboxes": s.opts.Sandboxes != nil,
		// Whether this deployment holds a provider key of its own, and so
		// bills organisations. Without one the panel hides Billing.
		"billing": len(s.opts.Platform) > 0,
		// Whether organisations pay for those keys in advance, by card.
		"payments": s.opts.Payments != nil,
		// Whether Claude Code may come through on a Claude plan. Without it
		// the panel offers no subscription models or keys.
		"claude_subscriptions": s.opts.ClaudeSubscriptions,
		// Whether administrators can create passkey accounts, and whether
		// this is one, which has passkeys to manage.
		"passkeys":        s.opts.Passkeys != nil,
		"passkey_account": s.isPasskeyAccount(r, p.UserID),
	}
	// The organisations this person can switch to. A failed read leaves them
	// out: the panel then shows only the current one.
	if orgs, err := s.orgsOf(r.Context(), p); err == nil && len(orgs) > 1 {
		out["orgs"] = orgs
	}
	if p.OrgID != "" {
		// A failed read leaves both out: they are only for display.
		if org, err := s.st.OrgByID(r.Context(), p.OrgID); err == nil {
			out["org_name"] = org.Name
			// The panel says on every screen what a limited organisation
			// cannot use yet.
			out["org_limited"] = org.Limited
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// isPasskeyAccount reports whether a person signs in with passkeys. A failed
// read counts as no: it only decides what the panel offers.
func (s *Server) isPasskeyAccount(r *http.Request, userID string) bool {
	if s.opts.Passkeys == nil || userID == "" {
		return false
	}
	u, err := s.st.UserByID(r.Context(), userID)
	return err == nil && authn.IsPasskeyAccount(u.ExternalID)
}

// orgName looks up an organisation's name, or "" when it cannot be read: the
// name is only for display.
func (s *Server) orgName(ctx context.Context, orgID string) string {
	name, _ := s.st.ScopeName(ctx, policy.ScopeOrg, orgID)
	return name
}

// gatewayURL is the inference plane as a developer's client reaches it: the
// panel's own origin plus httpx.InferencePrefix. It is the one place that
// address is built.
func (s *Server) gatewayURL(r *http.Request) string {
	return s.publicOrigin(r) + httpx.InferencePrefix
}

// publicOrigin is the address a browser reaches this panel on. PublicURL
// wins over the Host header, because behind a proxy it is the name a browser
// is known to use. Its path is kept: a gateway behind a proxy at
// https://host/keera is reached there, not at the host's root.
func (s *Server) publicOrigin(r *http.Request) string {
	if s.opts.PublicURL != "" {
		if u, err := url.Parse(s.opts.PublicURL); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
		}
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// signInErrorCookie carries why a sign-in failed to the panel. It is a cookie
// and not part of the address, because anyone can write an address into a
// link, and the panel shows this text on the sign-in screen.
const signInErrorCookie = "keera_sign_in_error"

// signInFailed sends the browser back to the panel with a readable error,
// rather than leaving it on a blank callback URL.
func (s *Server) signInFailed(w http.ResponseWriter, r *http.Request, err error) {
	// Unlike every other cookie here, the panel's script reads it.
	http.SetCookie(w, &http.Cookie{
		Name: signInErrorCookie, Value: url.PathEscape(err.Error()), Path: "/", MaxAge: 60,
		Secure: s.opts.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/?sign_in_error=1", http.StatusFound)
}

// safeRedirect keeps only a same-site path, so ?next= cannot send someone to
// another host right after they sign in. Backslashes are refused because
// browsers read "/\evil.com" as "//evil.com"; parsing refuses
// protocol-relative and absolute URLs.
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
