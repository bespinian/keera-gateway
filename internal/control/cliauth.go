package control

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Signing in from the command line is the browser sign-in with one extra hop.
//
// `keera login` listens on a loopback port, opens /control/auth/login with
// that port attached, and waits. After the usual sign-in, the callback
// redirects to the port with a one-time code, which the command line redeems
// for a token.
//
// Two rules from RFC 8252 keep the extra hop safe:
//
//   - The redirect must be a loopback address, so a link cannot send the code
//     to another host.
//   - The code is worthless without the PKCE verifier the command line kept.
//     Any local process can bind a loopback port, but only the one that
//     started the sign-in has the verifier.

// cliLogin is the loopback handshake a sign-in was started with. It is empty
// for an ordinary sign-in that ends in the panel.
type cliLogin struct {
	Redirect  string
	Challenge string
	State     string
}

// cliLoginFrom reads the loopback handshake from a sign-in URL. A handshake
// with parts missing is refused rather than completed without them.
func cliLoginFrom(q url.Values) (cliLogin, error) {
	c := cliLogin{
		Redirect:  strings.TrimSpace(q.Get("cli_redirect")),
		Challenge: strings.TrimSpace(q.Get("cli_challenge")),
		State:     strings.TrimSpace(q.Get("cli_state")),
	}
	if c.Redirect == "" && c.Challenge == "" && c.State == "" {
		return cliLogin{}, nil
	}
	redirect, err := loopbackRedirect(c.Redirect)
	if err != nil {
		return cliLogin{}, err
	}
	c.Redirect = redirect
	if !validChallenge(c.Challenge) {
		return cliLogin{}, errors.New("this sign-in was started without a usable proof key; " +
			"update the keera command line")
	}
	if c.State == "" || len(c.State) > 128 {
		return cliLogin{}, errors.New("this sign-in was started without a usable state; " +
			"update the keera command line")
	}
	return c, nil
}

// loopbackRedirect accepts the address of a listener on the browser's own
// machine, and nothing else.
//
// Only literal loopback addresses count, not "localhost": that name can be
// pointed elsewhere, which would make this an open redirect carrying a
// credential.
//
// The query is dropped, because the gateway adds the code and state itself.
func loopbackRedirect(raw string) (string, error) {
	refuse := errors.New("a command-line sign-in has to come back to a loopback " +
		"address such as http://127.0.0.1:1234/callback")
	if raw == "" {
		return "", refuse
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return "", refuse
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || port == "" {
		return "", refuse
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil || !ip.IsLoopback() {
		return "", refuse
	}
	if u.User != nil || !strings.HasPrefix(u.Path, "/") {
		return "", refuse
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(), nil
}

// validChallenge checks the shape of an S256 challenge: 32 bytes, base64url,
// unpadded. It is not a security check, since the verifier is checked later
// anyway. It just fails early, before the person goes to the browser.
func validChallenge(s string) bool {
	if len(s) != 43 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// handOverToCLI ends a command-line sign-in: it creates the one-time code and
// sends the browser to the port the command line is waiting on.
func (s *Server) handOverToCLI(w http.ResponseWriter, r *http.Request,
	flow store.LoginFlow, user store.User) {
	code, hash, err := authn.NewCLICode()
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.CreateCLICode(r.Context(), hash, user.ID, flow.CLIChallenge,
		time.Now().Add(authn.CLICodeTTL)); err != nil {
		s.fail(w, err)
		return
	}
	// The redirect was rebuilt without a query at the start, so appending one
	// is safe.
	q := url.Values{"code": {code}, "state": {flow.CLIState}}
	http.Redirect(w, r, flow.CLIRedirect+"?"+q.Encode(), http.StatusFound)
}

// cliToken redeems a one-time code for a token the command line keeps.
//
// Nobody is signed in to reach it: the code is the credential, and the
// verifier proves the caller started the sign-in. The code is deleted as it
// is read, so it works only once.
func (s *Server) cliToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
		// Label is how the machine describes itself, so a person can tell
		// their sign-ins apart. It decides nothing.
		Label string `json:"label"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil || in.Code == "" || in.Verifier == "" {
		badRequest(w, "a 'code' and the 'verifier' that redeems it are required")
		return
	}

	code, err := s.st.TakeCLICode(r.Context(), authn.HashCLICode(in.Code))
	if err != nil {
		// One message for expired, used and unknown codes, so a guesser
		// cannot tell which codes were real.
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_code",
			"this sign-in has expired or was already completed; run 'keera login' again")
		return
	}
	if subtle.ConstantTimeCompare([]byte(authn.CLIChallenge(in.Verifier)),
		[]byte(code.Challenge)) != 1 {
		s.log.Warn("a command-line sign-in was redeemed with the wrong proof key",
			"user", code.UserID)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_verifier",
			"this sign-in was completed by a different process than the one that started it")
		return
	}
	user, err := s.st.UserByID(r.Context(), code.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}

	token, hash, err := authn.NewCLIToken()
	if err != nil {
		s.fail(w, err)
		return
	}
	expires := time.Now().Add(authn.CLITokenTTL)
	if err := s.st.CreateCLIToken(r.Context(), hash, user.ID, in.Label, expires); err != nil {
		s.fail(w, err)
		return
	}
	// A credential now exists on someone's machine, which is its own event:
	// the one to look for when asking where an account is signed in.
	s.auditf(r, &authn.Principal{Via: authn.MethodCLI, Email: user.Email, UserID: user.ID},
		user.OrgID, "auth.cli_token", "user", user.ID,
		map[string]any{"label": in.Label, "expires_at": expires})

	// Only the token and its expiry. Who this is comes from /v1/me, so there
	// is one answer to "who am I".
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires_at": expires,
	})
}

// cliPrincipal resolves a command-line token sent as a bearer credential.
func (s *Server) cliPrincipal(r *http.Request, token string) (*authn.Principal, error) {
	hash := authn.HashCLIToken(token)
	tu, err := s.st.LookupCLIToken(r.Context(), hash)
	if err != nil {
		return nil, authn.ErrUnauthenticated
	}
	if err := s.st.TouchCLIToken(r.Context(), hash); err != nil {
		s.log.Warn("recording command-line activity failed", "error", err)
	}
	return &authn.Principal{
		Via:    authn.MethodCLI,
		UserID: tu.User.ID,
		Email:  tu.User.Email,
		Role:   authn.Role(tu.User.Role),
		OrgID:  tu.User.OrgID,
		// No CSRF token: a browser never sends this credential.
		CredentialHash: hash,
	}, nil
}
