package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Passkeys sign in the accounts no directory vouches for. See docs/sso.md.
//
// Three rules keep them from weakening single sign-on:
//
//   - Only a passkey account has passkeys (authn.PasskeyExternalID). A
//     directory account never gets one, so leaving the directory still locks
//     a person out.
//   - A passkey account never holds the operator role, whatever its address.
//     Nobody proved the address; an administrator typed it.
//   - An administrator may only create one at their organisation's email
//     domain, when it has one.
//
// An administrator hands over a one-time set-up link. It adds the first
// passkey, a passkey on a device that has none yet, or a new one after a loss.

// errUserDisabled is a passkey sign-in by someone who was disabled.
var errUserDisabled = errors.New("the account is disabled")

// passkeyNameMax bounds what a person calls a passkey.
const passkeyNameMax = 64

// passkeyStepUp is how recent a sign-in has to be to add a passkey. A passkey
// outlives every session, so a stolen cookie must not be able to plant one.
const passkeyStepUp = 15 * time.Minute

// The messages a refused ceremony gets. The reason goes to the log only.
const (
	passkeyRefused = "that passkey was not accepted; try again, or ask an " +
		"administrator for a new set-up link"
	passkeyLinkGone = "this set-up link has expired, was already used, or was " +
		"replaced by a newer one; ask an administrator for a new one"
	passkeyStale = "this passkey request has expired; start again"
)

// passkeysOff refuses a passkey route on a gateway that has them off.
func (s *Server) passkeysOff(w http.ResponseWriter) bool {
	if s.opts.Passkeys != nil {
		return false
	}
	httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error",
		"passkeys_disabled", "passkeys are off on this gateway; set KEERA_PASSKEYS=true")
	return true
}

// readPasskeyJSON reads the body of an unauthenticated passkey route. It must
// be JSON by its content type: a cross-site form cannot send that, so another
// site cannot sign a browser in to an account of its choosing.
func readPasskeyJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "",
			"send the request as application/json")
		return false
	}
	return readJSON(w, r, v)
}

// ------------------------------------------------------------------ sign-in

// passkeyLogin reports whether a sign-in that names this provider is a
// passkey sign-in. Naming none is one when passkeys are the only way in.
func (s *Server) passkeyLogin(provider string) bool {
	if s.opts.Passkeys == nil {
		return false
	}
	return provider == authn.PasskeyProvider || (provider == "" && !s.opts.Providers.Enabled())
}

// startPasskeyLogin is /auth/login for a passkey. It is how `keera login`
// signs in with one: the flow keeps the loopback hand-over, and the panel
// asks for the passkey.
func (s *Server) startPasskeyLogin(w http.ResponseWriter, r *http.Request) {
	cli, err := cliLoginFrom(r.URL.Query())
	if err != nil {
		s.signInFailed(w, r, err)
		return
	}
	flow, err := s.newPasskeyFlow(r.Context(), cli, safeRedirect(r.URL.Query().Get("next")))
	if err != nil {
		s.fail(w, err)
		return
	}
	// After the #, so the state stays out of access logs.
	http.Redirect(w, r, "/#passkey-sign-in="+flow.State, http.StatusFound)
}

// newPasskeyFlow records a passkey sign-in in flight. It is a login flow like
// any other, so the command-line hand-over works the same; the nonce is the
// challenge.
func (s *Server) newPasskeyFlow(ctx context.Context, cli cliLogin, next string) (store.LoginFlow, error) {
	state := authn.NewPasskeyChallenge()
	challenge := authn.NewPasskeyChallenge()
	f := store.LoginFlow{
		State: state, Nonce: challenge, Provider: authn.PasskeyProvider, RedirectTo: next,
		CLIRedirect: cli.Redirect, CLIChallenge: cli.Challenge, CLIState: cli.State,
	}
	return f, s.st.CreateLoginFlow(ctx, f, time.Now().Add(authn.FlowTTL))
}

// passkeySignInOptions starts a passkey sign-in, or continues one that
// /auth/login started for the command line.
func (s *Server) passkeySignInOptions(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	var in struct {
		Flow string `json:"flow"`
		// Next is where the panel goes afterwards, as after single sign-on.
		Next string `json:"next"`
	}
	if !readPasskeyJSON(w, r, &in) {
		return
	}
	var flow store.LoginFlow
	var err error
	if in.Flow != "" {
		flow, err = s.st.LoginFlowByState(r.Context(), in.Flow)
		if err != nil || flow.Provider != authn.PasskeyProvider {
			unauthorized(w, "invalid_flow", "this sign-in link has expired or was already used")
			return
		}
	} else if flow, err = s.newPasskeyFlow(r.Context(), cliLogin{},
		safeRedirect(in.Next)); err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"flow": flow.State, "options": s.opts.Passkeys.RequestOptions(flow.Nonce),
	})
}

// passkeySignIn completes a passkey sign-in.
func (s *Server) passkeySignIn(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	var in struct {
		Flow       string          `json:"flow"`
		Credential authn.Assertion `json:"credential"`
	}
	if !readPasskeyJSON(w, r, &in) {
		return
	}
	// Taking the flow deletes it, so an answer works once.
	flow, err := s.st.TakeLoginFlow(r.Context(), in.Flow)
	if err != nil || flow.Provider != authn.PasskeyProvider {
		unauthorized(w, "invalid_flow", "this sign-in has expired; start again")
		return
	}
	user, key, err := s.checkPasskey(r, flow.Nonce, in.Credential)
	switch {
	case errors.Is(err, authn.ErrPasskey), errors.Is(err, store.ErrNotFound):
		s.log.Warn("passkey sign-in refused", "error", err)
		unauthorized(w, "passkey_refused", passkeyRefused)
		return
	case errors.Is(err, errUserDisabled):
		forbid(w, "your access to Keera has been turned off; ask an administrator of "+
			"your organisation")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	// No directory speaks for this account, so it gets what a sign-in with no
	// groups and no operator address would: the operator role is taken away,
	// and so is admin where a directory group decides it.
	was := user.Role
	if err := s.syncRole(r, &user, authn.RoleMember); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, err)
		return
	}
	entry := map[string]any{"role": user.Role, "via": "passkey", "passkey": key.Name}
	if flow.CLI() {
		entry["client"] = "command line"
	}
	if was != user.Role {
		entry["role_was"] = was
	}
	s.auditf(r, &authn.Principal{Via: authn.MethodSession, Email: user.Email, UserID: user.ID},
		user.OrgID, "auth.sign_in", "user", user.ID, entry)

	// The panel goes where this answer says. For `keera login` that is the
	// loopback port, as after single sign-on.
	to := cmp.Or(flow.RedirectTo, "/")
	if flow.CLI() {
		if to, err = s.cliHandOver(r, flow, user); err != nil {
			s.fail(w, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"redirect": to})
}

// checkPasskey checks a sign-in's answer and records the use. It returns who
// signed in, and with which passkey.
func (s *Server) checkPasskey(r *http.Request, challenge string,
	a authn.Assertion) (store.User, store.Passkey, error) {
	credID, err := a.CredentialID()
	if err != nil {
		return store.User{}, store.Passkey{}, err
	}
	key, err := s.st.PasskeyByCredentialID(r.Context(), credID)
	if err != nil {
		return store.User{}, store.Passkey{}, err
	}
	if !a.OwnedBy(key.UserID) {
		return store.User{}, key, errors.Join(authn.ErrPasskey,
			errors.New("the user handle is not the passkey's owner"))
	}
	count, err := s.opts.Passkeys.VerifyAssertion(challenge, a, credentialOf(key))
	if err != nil {
		return store.User{}, key, err
	}
	// The account is checked before the use is recorded, so a refused sign-in
	// leaves the counter and last use alone.
	user, err := s.st.UserByID(r.Context(), key.UserID)
	if err != nil {
		return user, key, err
	}
	if !authn.IsPasskeyAccount(user.ExternalID) {
		return user, key, errors.Join(authn.ErrPasskey,
			errors.New("the account is linked to a directory"))
	}
	if user.Disabled() {
		return user, key, errUserDisabled
	}
	if err := s.st.UsePasskey(r.Context(), key.ID, count); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return user, key, errors.Join(authn.ErrPasskey,
				errors.New("another sign-in moved the counter first"))
		}
		return user, key, err
	}
	return user, key, nil
}

func credentialOf(p store.Passkey) authn.Credential {
	return authn.Credential{
		ID: p.CredentialID, PublicKey: p.PublicKey, Algorithm: p.Algorithm, SignCount: p.SignCount,
	}
}

// ------------------------------------------------------------- registration

// passkeySetupOptions starts adding a passkey from a set-up link. Nobody is
// signed in: the link is the credential.
func (s *Server) passkeySetupOptions(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !readPasskeyJSON(w, r, &in) {
		return
	}
	user, err := s.st.PasskeyLinkUser(r.Context(), authn.HashPasskeyLink(in.Token))
	if err != nil || !authn.IsPasskeyAccount(user.ExternalID) {
		unauthorized(w, "invalid_link", passkeyLinkGone)
		return
	}
	s.registrationOptions(w, r, user)
}

// registrationOptions starts a registration for user.
func (s *Server) registrationOptions(w http.ResponseWriter, r *http.Request, user store.User) {
	existing, err := s.st.ListPasskeys(r.Context(), user.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	exclude := make([][]byte, 0, len(existing))
	for _, k := range existing {
		exclude = append(exclude, k.CredentialID)
	}
	challenge := authn.NewPasskeyChallenge()
	challengeID := id.New("challenge")
	if err := s.st.CreatePasskeyChallenge(r.Context(), challengeID, user.ID, challenge,
		time.Now().Add(authn.PasskeyChallengeTTL)); err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"challenge_id": challengeID,
		"email":        user.Email,
		"options":      s.opts.Passkeys.CreationOptions(challenge, user.ID, user.Email, exclude),
	})
}

// registration is what both ways of adding a passkey send back.
type registration struct {
	ChallengeID string             `json:"challenge_id"`
	Name        string             `json:"name"`
	Credential  authn.Registration `json:"credential"`
}

// passkeySetup completes a set-up link: it stores the passkey, uses up the
// link and signs the person in.
func (s *Server) passkeySetup(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	var in struct {
		registration
		Token string `json:"token"`
	}
	if !readPasskeyJSON(w, r, &in) {
		return
	}
	userID, cred, ok := s.verifyRegistration(w, r, in.registration)
	if !ok {
		return
	}
	if _, err := s.st.PasskeyByCredentialID(r.Context(), cred.ID); err == nil {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "passkey_exists",
			"that passkey is already registered")
		return
	}
	// The link is used up only now, so a cancelled prompt does not cost it.
	if linkUser, err := s.st.TakePasskeyLink(r.Context(),
		authn.HashPasskeyLink(in.Token)); err != nil || linkUser != userID {
		unauthorized(w, "invalid_link", passkeyLinkGone)
		return
	}
	user, err := s.st.UserByID(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if user.Disabled() || !authn.IsPasskeyAccount(user.ExternalID) {
		unauthorized(w, "invalid_link", passkeyLinkGone)
		return
	}
	p := &authn.Principal{Via: authn.MethodSession, Email: user.Email, UserID: user.ID}
	if _, ok := s.storePasskey(w, r, p, user, cred, in.Name, "set-up link"); !ok {
		return
	}
	if err := s.syncRole(r, &user, authn.RoleMember); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.startSession(w, r, user); err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"redirect": "/"})
}

// verifyRegistration uses up the challenge and checks the browser's answer.
// It returns whose registration it was.
func (s *Server) verifyRegistration(w http.ResponseWriter, r *http.Request,
	in registration) (string, authn.Credential, bool) {
	userID, challenge, err := s.st.TakePasskeyChallenge(r.Context(), in.ChallengeID)
	if err != nil {
		badRequest(w, passkeyStale)
		return "", authn.Credential{}, false
	}
	cred, err := s.opts.Passkeys.VerifyRegistration(challenge, in.Credential)
	if err != nil {
		s.log.Warn("passkey registration refused", "error", err, "user", userID)
		badRequest(w, "the browser's answer was not accepted; try again, in an "+
			"up-to-date browser")
		return "", authn.Credential{}, false
	}
	return userID, cred, true
}

// storePasskey saves a verified passkey and audits it.
func (s *Server) storePasskey(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	user store.User, cred authn.Credential, name, via string) (store.Passkey, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > passkeyNameMax {
		name = string([]rune(name)[:passkeyNameMax])
	}
	key, err := s.st.AddPasskey(r.Context(), store.Passkey{
		ID: id.New("passkey"), UserID: user.ID, Name: name,
		CredentialID: cred.ID, PublicKey: cred.PublicKey, Algorithm: cred.Algorithm,
		SignCount: cred.SignCount,
	})
	if errors.Is(err, store.ErrPasskeyTaken) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "passkey_exists",
			"that passkey is already registered")
		return key, false
	}
	if err != nil {
		s.fail(w, err)
		return key, false
	}
	s.auditf(r, p, user.OrgID, "passkey.create", "user", user.ID, map[string]any{
		"passkey": key.ID, "name": key.Name, "via": via,
	})
	return key, true
}

// --------------------------------------------------------- your own passkeys

// ownPasskeyAccount reads the caller's account, if it is a passkey account.
func (s *Server) ownPasskeyAccount(w http.ResponseWriter, r *http.Request,
	p *authn.Principal) (store.User, bool) {
	if p.UserID == "" {
		forbid(w, "the operator key is nobody, so it has no passkeys")
		return store.User{}, false
	}
	// Only from a browser that signed in moments ago: not with a
	// command-line token, and not with a session that may have been taken.
	if p.Via != authn.MethodSession {
		forbid(w, "add a passkey in the panel, not from the command line")
		return store.User{}, false
	}
	if time.Since(p.SignedInAt) > passkeyStepUp {
		httpx.WriteError(w, http.StatusForbidden, "invalid_request_error", "sign_in_again",
			fmt.Sprintf("sign out and in again to add a passkey; it needs a sign-in "+
				"from the last %d minutes", int(passkeyStepUp.Minutes())))
		return store.User{}, false
	}
	user, err := s.st.UserByID(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, err)
		return user, false
	}
	if !authn.IsPasskeyAccount(user.ExternalID) {
		forbid(w, "you sign in through your identity provider, so your account has no passkeys")
		return user, false
	}
	return user, true
}

// ownPasskeyOptions starts adding a passkey to the caller's own account, for
// a second device.
func (s *Server) ownPasskeyOptions(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.passkeysOff(w) {
		return
	}
	if user, ok := s.ownPasskeyAccount(w, r, p); ok {
		s.registrationOptions(w, r, user)
	}
}

// addOwnPasskey completes it.
func (s *Server) addOwnPasskey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.passkeysOff(w) {
		return
	}
	var in registration
	if !readJSON(w, r, &in) {
		return
	}
	user, ok := s.ownPasskeyAccount(w, r, p)
	if !ok {
		return
	}
	userID, cred, ok := s.verifyRegistration(w, r, in)
	if !ok {
		return
	}
	if userID != user.ID {
		badRequest(w, passkeyStale)
		return
	}
	if key, ok := s.storePasskey(w, r, p, user, cred, in.Name, "signed in"); ok {
		httpx.WriteJSON(w, http.StatusCreated, key)
	}
}

// listPasskeys lists one person's passkeys: the caller's own, or with
// ?user_id someone an administrator looks after.
func (s *Server) listPasskeys(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.passkeysOff(w) {
		return
	}
	userID := cmp.Or(r.URL.Query().Get("user_id"), p.UserID)
	if userID == "" {
		badRequest(w, "pass user_id: the operator key has no passkeys of its own")
		return
	}
	if userID != p.UserID {
		target, err := s.st.UserByID(r.Context(), userID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !s.requireOwnerAdmin(w, p, target.OrgID) {
			return
		}
	}
	keys, err := s.st.ListPasskeys(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": keys})
}

// deletePasskey removes a passkey. A person may remove their own, but not the
// last one, which would lock them out. An administrator may remove anyone's
// in their organisation; that also signs the person out, since whoever held
// the passkey may be signed in with it.
func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.passkeysOff(w) {
		return
	}
	key, err := s.st.PasskeyByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	owner, err := s.st.UserByID(r.Context(), key.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	own := key.UserID == p.UserID
	if !own && !s.requireOwnerAdmin(w, p, owner.OrgID) {
		return
	}
	err = s.st.DeletePasskey(r.Context(), key.ID, own)
	if errors.Is(err, store.ErrLastPasskey) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "last_passkey",
			"this is your only passkey; add another one first, or ask an "+
				"administrator to remove it")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !own {
		if err := s.st.DeleteUserSessions(r.Context(), owner.ID); err != nil {
			s.log.Warn("signing out a user after removing a passkey failed", "error", err,
				"user", owner.ID)
		}
	}
	s.auditf(r, p, owner.OrgID, "passkey.delete", "user", owner.ID, map[string]any{
		"passkey": key.ID, "name": key.Name, "email": owner.Email,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": key.ID, "deleted": true})
}

// ----------------------------------------------------------- set-up links

// passkeyLinkOut is a set-up link as the caller gets it. The token is in it,
// so it is shown once and never stored.
type passkeyLinkOut struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// issuePasskeyLinkRoute hands out a set-up link for a person. It also turns a
// person who has not signed in yet into a passkey account.
func (s *Server) issuePasskeyLinkRoute(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.passkeysOff(w) {
		return
	}
	target, err := s.st.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.requireOwnerAdmin(w, p, target.OrgID) {
		return
	}
	if target.Role == string(authn.RoleOperator) {
		forbid(w, target.Email+" is an operator, which comes from the configuration; "+
			"a passkey account cannot hold that role")
		return
	}
	if target.Disabled() {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "user_disabled",
			target.Email+" is disabled; enable them first")
		return
	}
	if !s.mayVouchFor(w, r, p, target.OrgID, target.Email) {
		return
	}
	err = s.st.MakePasskeyAccount(r.Context(), target.ID, authn.PasskeyExternalID(target.ID))
	if errors.Is(err, store.ErrDirectoryAccount) {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "directory_account",
			target.Email+" signs in through an identity provider, and a directory "+
				"account does not get passkeys: leaving the directory has to lock them out")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	link, err := s.issuePasskeyLink(r, p, target)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, link)
}

// issuePasskeyLink mints a set-up link, replacing any earlier one.
func (s *Server) issuePasskeyLink(r *http.Request, p *authn.Principal,
	user store.User) (passkeyLinkOut, error) {
	token, hash := authn.NewPasskeyLink()
	expires := time.Now().Add(authn.PasskeyLinkTTL)
	if err := s.st.CreatePasskeyLink(r.Context(), hash, user.ID, expires); err != nil {
		return passkeyLinkOut{}, err
	}
	s.auditf(r, p, user.OrgID, "user.passkey_link", "user", user.ID, map[string]any{
		"email": user.Email, "expires_at": expires,
	})
	// After the #, so the token stays out of access logs and Referer headers.
	return passkeyLinkOut{
		URL:       s.publicOrigin(r) + "/#passkey-setup=" + token,
		ExpiresAt: expires,
	}, nil
}

// mayVouchFor checks that the caller may create a passkey account for this
// address. The address is only as good as whoever typed it, so an
// administrator may only use their organisation's domain. An operator may use
// any.
func (s *Server) mayVouchFor(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	orgID, email string) bool {
	if p.Unrestricted() {
		return true
	}
	org, err := s.st.OrgByID(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if org.EmailDomain == "" {
		return true
	}
	domain := (authn.Identity{Email: email}).Domain()
	if !strings.EqualFold(domain, strings.TrimPrefix(org.EmailDomain, "@")) {
		forbid(w, email+" is not at "+org.EmailDomain+", this organisation's domain; "+
			"only an operator can create a passkey account at another domain")
		return false
	}
	return true
}
