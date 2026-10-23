package control

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// An LDAP directory signs people in with a password. There is no redirect: the
// panel shows a form and sends it here, and the gateway binds as the person.
// After that it is the same sign-in as through any directory. See docs/sso.md.

// passwordPerAccountRPM bounds the attempts on one account, from any number of
// addresses, so the gateway cannot guess a password or lock an account out
// faster than this. It is counted twice: by what was typed, which bounds the
// searches, and by the entry found, which bounds the passwords tried on it
// however its username is spelled.
const passwordPerAccountRPM = 5

// The per-account buckets share the limiter with the per-address ones, whose
// keys are a route name and an address. No route is named like these.
const (
	passwordNameBucket  = "password-name|"
	passwordEntryBucket = "password-entry|"
)

const passwordThrottled = "too many sign-in attempts for this account; wait a minute"

// errAccountThrottled stops a sign-in an account's bucket refused.
type errAccountThrottled struct{ retry time.Duration }

func (errAccountThrottled) Error() string { return passwordThrottled }

// The answer to a refused username and password. The reason goes to the log.
const passwordRefused = "that username and password were not accepted"

// startPasswordLogin is /auth/login for a password provider. It is how `keera
// login` reaches the form: the flow keeps the loopback hand-over, and the panel
// asks for the password.
func (s *Server) startPasswordLogin(w http.ResponseWriter, r *http.Request,
	provider *authn.OIDC, cli cliLogin) {
	f := store.LoginFlow{
		State: authn.NewFlow().State, Provider: provider.Name(),
		RedirectTo:  safeRedirect(r.URL.Query().Get("next")),
		CLIRedirect: cli.Redirect, CLIChallenge: cli.Challenge, CLIState: cli.State,
	}
	if err := s.st.CreateLoginFlow(r.Context(), f, time.Now().Add(authn.FlowTTL)); err != nil {
		s.fail(w, err)
		return
	}
	// After the #, so the state stays out of access logs.
	http.Redirect(w, r, "/#password-sign-in="+provider.Name()+"."+f.State+
		cli.panelHint(s.opts.ChatURL), http.StatusFound)
}

func (s *Server) passwordSignIn(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		// Flow is the sign-in /auth/login started for `keera login`. Without
		// one, this is a sign-in to the panel.
		Flow     string `json:"flow"`
		Username string `json:"username"`
		Password string `json:"password"`
		Next     string `json:"next"`
	}
	if !readSignedOutJSON(w, r, &in) {
		return
	}
	provider := s.opts.Providers.ByName(in.Provider)
	if provider == nil || !provider.Password() {
		badRequest(w, in.Provider+" is not a password sign-in on this gateway")
		return
	}
	flow := store.LoginFlow{Provider: provider.Name(), RedirectTo: safeRedirect(in.Next)}
	if in.Flow != "" {
		// Read, not taken: a mistyped password must not end a `keera login`.
		f, err := s.st.LoginFlowByState(r.Context(), in.Flow)
		if err != nil || f.Provider != provider.Name() {
			unauthorized(w, "invalid_flow", "this sign-in has expired; start again")
			return
		}
		flow = f
	}
	limit := func(key string) error {
		if now := time.Now(); !s.signIn.Allow(key, passwordPerAccountRPM, now) {
			return errAccountThrottled{s.signIn.Retry(key, passwordPerAccountRPM, now)}
		}
		return nil
	}
	// A name too long for any directory gets no bucket, which would keep it in
	// memory for minutes: Authenticate refuses it without asking the directory.
	var err error
	if name := strings.ToLower(strings.TrimSpace(in.Username)); len(name) <= authn.LDAPUsernameMax {
		err = limit(passwordNameBucket + provider.Name() + "|" + name)
	}
	var identity authn.Identity
	if err == nil {
		identity, err = provider.Authenticate(r.Context(), in.Username, in.Password,
			func(dn string) error { return limit(passwordEntryBucket + provider.Name() + "|" + dn) })
	}
	switch throttled, isThrottled := errors.AsType[errAccountThrottled](err); {
	case isThrottled:
		tooMany(w, throttled.retry, passwordThrottled)
		return
	case errors.Is(err, authn.ErrBadCredentials):
		s.log.Warn("password sign-in refused", "provider", provider.Name(), "error", err)
		unauthorized(w, "invalid_credentials", passwordRefused)
		return
	case errors.Is(err, authn.ErrDirectoryUnreachable):
		s.log.Error("password sign-in failed", "provider", provider.Name(), "error", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, "api_error", "directory_unreachable",
			"the directory did not answer; try again in a moment")
		return
	case errors.Is(err, authn.ErrUnusableEntry):
		s.log.Warn("password sign-in refused", "provider", provider.Name(), "error", err)
		forbid(w, authn.ErrUnusableEntry.Error())
		return
	case err != nil:
		// The password was right, but Keera cannot use the account.
		s.log.Warn("password sign-in refused", "provider", provider.Name(), "error", err)
		forbid(w, err.Error())
		return
	}
	if in.Flow != "" {
		// Taking it now makes the hand-over work once.
		if flow, err = s.st.TakeLoginFlow(r.Context(), in.Flow); err != nil {
			unauthorized(w, "invalid_flow", "this sign-in has expired; start again")
			return
		}
	}

	place, err := s.orgFor(r, provider, identity)
	if err != nil {
		forbid(w, err.Error())
		return
	}
	if place.signUp {
		// The configuration refuses sign-up on a directory, so this is only
		// here should that change.
		forbid(w, "there is no organisation for "+identity.Email+" yet; ask an operator")
		return
	}
	user, was, err := s.linkIdentity(r, provider, identity, place.orgID)
	if err != nil {
		if refused, ok := errors.AsType[forbidden](err); ok {
			forbid(w, refused.Error())
		} else {
			s.fail(w, err)
		}
		return
	}
	s.rememberDirectory(r, user, s.sealRefreshToken(user.ExternalID, identity.RefreshToken))
	if !s.signedIn(w, r, user, was, flow.CLI(),
		map[string]any{"via": "password", "provider": identity.Provider}) {
		return
	}

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
