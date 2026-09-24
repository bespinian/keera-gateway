package control

import (
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// playgroundChat sends a message typed into the panel to the inference plane.
//
// It goes through the same data plane as an editor's request, so an answer
// here means a key will get one too. The caller has no API key, so the
// organisation's own guardrails apply: the panel is no way around a budget.
func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if s.opts.Gateway == nil {
		httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error",
			"playground_unavailable",
			"this control plane is not attached to an inference gateway")
		return
	}
	// An operator looking at every organisation has not said whose budget to
	// charge.
	orgID, ok := s.requireOrg(w, p, r.URL.Query().Get("org_id"),
		"choose an organisation first; this request is rate-limited and charged against one")
	if !ok {
		return
	}
	lim, err := s.storedLimits(r.Context(), policy.ScopeOrg, orgID)
	if err != nil {
		s.fail(w, err)
		return
	}

	// The listener's write timeout is for small JSON answers. A streamed
	// completion can take longer, so it is lifted for this response only. A
	// test recorder does not support it, which is fine.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// Name the client, so the map shows the playground rather than the
	// operator's browser. It is set, not defaulted, so the browser cannot
	// claim to be something else.
	r.Header.Set(connect.ClientHeader, "keera-playground")
	s.opts.Gateway.ServeChat(w, r, policy.ResolveOrg(orgID, p.UserID, &lim))
}
