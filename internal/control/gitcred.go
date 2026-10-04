package control

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
)

// gitCredential hands a sandbox a fresh repository credential, in the format
// git's credential helpers speak, so sandbox/git-credential-keera can pass it
// on as it is.
//
// The sandbox asks with its own key, which is the only credential it has. The
// key stops working when the sandbox ends or its owner is disabled, and so
// does this.
func (s *Server) gitCredential(w http.ResponseWriter, r *http.Request) {
	key, err := auth.FromHeader(r.Header.Get("Authorization"))
	if err != nil {
		unauthorized(w, "unauthenticated",
			"send this sandbox's own key, KEERA_API_KEY, as a bearer token")
		return
	}
	sb, err := store.Sandbox{}, store.ErrNotFound
	if auth.WellFormed(key) {
		sb, err = s.st.LiveSandboxByKey(r.Context(), auth.Hash(key))
	}
	if errors.Is(err, store.ErrNotFound) {
		unauthorized(w, "invalid_api_key", "this key belongs to no live sandbox")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	limits, err := s.sandboxLimits(r.Context(), sb.OrgID, sb.TeamID)
	if err != nil {
		s.fail(w, err)
		return
	}
	cred, err := s.opts.Sandboxes.RefreshGit(r.Context(), sb, limits)
	if err != nil {
		s.failSandbox(w, err)
		return
	}
	if cred.Token == "" {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "sandbox_state",
			"the forge gave no token for this repository, so there is nothing to refresh; "+
				"an operator can check the forge settings (KEERA_SANDBOX_GIT_*)")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, "username=%s\npassword=%s\n", cred.Username, cred.Token)
	if !cred.Expires.IsZero() {
		_, _ = fmt.Fprintf(w, "password_expiry_utc=%d\n", cred.Expires.Unix())
	}
}
