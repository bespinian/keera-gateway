package control

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Signing up: a sign-in that matched no organisation, through a provider that
// allows it, ends on a screen where the person creates an organisation or
// joins the one that invited them. See docs/sso.md.

// signupCookie carries the sign-up between the callback and that screen. It
// is Strict, because only the panel's own requests may use it.
const signupCookie = "keera_signup"

// signupTTL is how long the screen waits for a choice.
const signupTTL = 30 * time.Minute

// maxOrgName bounds the name a stranger gives an organisation, which
// operators then read in every list.
const maxOrgName = 100

// startSignup keeps what the identity provider said, and sends the browser to
// the sign-up screen.
func (s *Server) startSignup(w http.ResponseWriter, r *http.Request, provider *authn.OIDC,
	identity authn.Identity, invited, redirectTo string) {
	userID := id.New("user")
	var sealed []byte
	if identity.RefreshToken != "" {
		sealed = s.opts.Secrets.Seal(refreshTokenName(userID), identity.RefreshToken)
	}
	token := auth.RandomToken()
	if err := s.st.CreateSignup(r.Context(), store.Signup{
		TokenHash:  authn.HashSession(token),
		UserID:     userID,
		Provider:   provider.Name(),
		ExternalID: identity.ExternalID(),
		Email:      identity.Email,
		Role:       string(provider.Mapping().RoleFor(identity.Email, identity.Groups)),
		// Sealed against the account it will belong to, so it never opens
		// as anybody else's.
		RefreshToken: sealed,
		InvitedOrg:   invited,
		RedirectTo:   redirectTo,
	}, time.Now().Add(signupTTL)); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, s.signupStateCookie(token, int(signupTTL.Seconds())))
	http.Redirect(w, r, "/?sign_up=1", http.StatusFound)
}

// signupStateCookie sets signupCookie, or with maxAge -1 removes it.
func (s *Server) signupStateCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: signupCookie, Value: token, Path: httpx.ControlPrefix + "/auth/signup",
		MaxAge: maxAge, HttpOnly: true, Secure: s.opts.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	}
}

// signupHash reads the sign-up's cookie, or answers that there is none.
func (s *Server) signupHash(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	c, err := r.Cookie(signupCookie)
	if err != nil || c.Value == "" {
		signupGone(w)
		return nil, false
	}
	return authn.HashSession(c.Value), true
}

func signupGone(w http.ResponseWriter) {
	unauthorized(w, "signup_expired", "this sign-up has expired or was finished in "+
		"another tab; sign in again")
}

// signupInfo is what the sign-up screen shows: who signed in, and who
// invited them.
func (s *Server) signupInfo(w http.ResponseWriter, r *http.Request) {
	hash, ok := s.signupHash(w, r)
	if !ok {
		return
	}
	su, err := s.st.SignupByToken(r.Context(), hash)
	if errors.Is(err, store.ErrNotFound) {
		signupGone(w)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"email": su.Email}
	if su.InvitedOrg != "" {
		out["invited_org"] = su.InvitedOrgName
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// completeSignup creates the organisation, or joins the one that invited the
// person, and signs them in.
func (s *Server) completeSignup(w http.ResponseWriter, r *http.Request) {
	hash, ok := s.signupHash(w, r)
	if !ok {
		return
	}
	var in struct {
		// Join accepts the invitation instead of creating an organisation.
		Join bool   `json:"join"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var (
		su      store.Signup
		user    store.User
		created *store.Org
		err     error
	)
	if in.Join {
		su, user, err = s.st.JoinInvitation(r.Context(), hash)
	} else {
		name := strings.TrimSpace(in.Name)
		if name == "" || utf8.RuneCountInString(name) > maxOrgName {
			badRequest(w, "give your organisation a name of up to 100 characters")
			return
		}
		var org store.Org
		su, org, user, err = s.createSignedUpOrg(r, hash, name)
		created = &org
	}
	switch {
	case errors.Is(err, store.ErrNotFound) && in.Join:
		// The sign-up or the invitation is gone. Either way, signing in
		// again shows what is there now.
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "invitation_gone",
			"that invitation is no longer there, or the sign-up expired; sign in again")
		return
	case errors.Is(err, store.ErrNotFound):
		signupGone(w)
		return
	case errors.Is(err, store.ErrEmailTaken):
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "already_signed_up",
			"this sign-in already has an account; sign in again to use it")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	http.SetCookie(w, s.signupStateCookie("", -1))

	if err := s.st.RecordDirectoryCheck(r.Context(), user.ID, su.RefreshToken); err != nil {
		s.log.Error("storing a refresh token", "user", user.ID, "error", err)
	}
	if in.Join {
		// The row's role stands, unless the configuration makes them an
		// operator.
		if err := s.syncRole(r, &user, authn.Role(su.Role)); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, err)
		return
	}
	actor := &authn.Principal{Via: authn.MethodSession, Email: user.Email, UserID: user.ID}
	how := "joined"
	if created != nil {
		how = "created"
		s.auditf(r, actor, created.ID, "org.create", "org", created.ID,
			map[string]any{"name": created.Name, "via": "signup", "limited": created.Limited})
		// Its models, copied from the template, are for the gateway to serve.
		s.changed(r)
	}
	s.auditf(r, actor, user.OrgID, "auth.sign_in", "user", user.ID, map[string]any{
		"role": user.Role, "via": "sso", "provider": su.Provider, "signup": how,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"redirect": cmp.Or(su.RedirectTo, "/")})
}

// createSignedUpOrg creates a limited organisation with the person as its
// administrator.
//
// A name somebody else has is not refused: saying so would tell a stranger
// which customers this deployment has. The address is added to make it
// unique, and an operator can rename it.
func (s *Server) createSignedUpOrg(r *http.Request, hash []byte, name string) (
	store.Signup, store.Org, store.User, error) {
	org := store.Org{ID: id.New("org"), Name: name, Limited: true}
	su, created, user, err := s.st.SignUp(r.Context(), hash, org, s.opts.Template)
	if errors.Is(err, store.ErrOrgNameTaken) {
		org.Name = name + " (" + su.Email + ")"
		su, created, user, err = s.st.SignUp(r.Context(), hash, org, s.opts.Template)
	}
	return su, created, user, err
}
