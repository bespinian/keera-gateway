package control

import (
	"net/http"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
)

// listConnect serves the client catalogue and the gateway address to put in
// it. The panel and `keera connect` both read it, so everyone gets the same
// configuration.
//
// The templates hold no secrets. It needs a sign-in only because of the
// gateway address, which a deployment may not have published.
func (s *Server) listConnect(w http.ResponseWriter, r *http.Request, _ *authn.Principal) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data":        connect.Clients(),
		"gateway_url": s.gatewayURL(r),
	})
}
