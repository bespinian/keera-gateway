package control

import (
	"errors"
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// playgroundChat sends a message typed into the panel to the inference plane,
// with one of the caller's own keys, named by ?key_id. It goes through the same
// data plane as an editor's request, so it meets that key's guardrails, is
// charged to its budgets and shows in its usage.
func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	keyID := r.URL.Query().Get("key_id")
	if keyID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "key_required",
			"choose one of your API keys; the playground sends with it")
		return
	}
	// The operator key is not a person, so no key is linked to it.
	if p.UserID == "" {
		forbid(w, "the operator key has no API keys of its own; sign in to the panel to use the playground")
		return
	}
	res, err := s.st.LookupKeyByID(r.Context(), keyID)
	switch {
	// Somebody else's key looks the same as no key, so ids cannot be probed.
	case errors.Is(err, policy.ErrUnknownKey),
		err == nil && (res.Key.UserID != p.UserID || !p.CanReadOrg(res.Key.OrgID)):
		s.fail(w, store.ErrNotFound)
		return
	case errors.Is(err, policy.ErrKeyRevoked):
		conflict(w, "key_revoked", "this API key has been revoked; choose another one")
		return
	case errors.Is(err, policy.ErrKeyExpired):
		conflict(w, "key_expired", "this API key has expired; choose another one")
		return
	case errors.Is(err, policy.ErrKeyPending):
		conflict(w, "key_pending", "this API key is not activated yet; activate it first")
		return
	case err != nil:
		s.fail(w, err)
		return
	case res.Key.Subscription():
		forbid(w, "this is a subscription key, which only Claude Code signed in to a Claude "+
			"plan can use; choose another one")
		return
	}

	// The listener's write timeout is for small JSON answers, and a streamed
	// completion can take longer. A test recorder cannot lift it, which is fine.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	r.Header.Set(connect.ClientHeader, playgroundClient(p, r.Header.Get(connect.ClientHeader)))
	s.opts.Gateway.ServeChat(w, r, res)
}

// playgroundClient names the client of a playground request, so the map shows
// the playground rather than the operator's browser. It is set, not
// defaulted, so a browser cannot claim to be something else.
//
// Keera Chat sends through this route too. It signs in like the command
// line, with a token no browser holds, and may say it is Keera Chat.
func playgroundClient(p *authn.Principal, stated string) string {
	if p.Via == authn.MethodCLI && stated == "keera-chat" {
		return "keera-chat"
	}
	return "keera-playground"
}
