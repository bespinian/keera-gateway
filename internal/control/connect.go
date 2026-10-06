package control

import (
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/store"
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

// clientKey is one key a client called with.
type clientKey struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Requests int64     `json:"requests"`
	LastUsed time.Time `json:"last_used"`
}

// connectedClient is one client that called with the caller's keys.
type connectedClient struct {
	Key      string      `json:"key"`
	Label    string      `json:"label"`
	Requests int64       `json:"requests"`
	LastUsed time.Time   `json:"last_used"`
	Keys     []clientKey `json:"keys"`
}

// listClients is the caller's own clients: which ones called with their keys
// in the window, and with which key. Like My access, it shows only the
// caller's own, even to an administrator. The map shows everyone's.
func (s *Server) listClients(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	from, to, ok := queryWindow(w, r.URL.Query())
	if !ok {
		return
	}
	out := map[string]any{"from": from, "to": to, "data": []connectedClient{}}
	// The operator key is not a person and has no keys.
	if p.Via == authn.MethodOperatorKey || p.UserID == "" || p.OrgID == "" {
		out["anonymous"] = true
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	uses, err := s.st.ClientUses(r.Context(), p.OrgID, p.UserID, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	names, err := s.st.KeyNames(r.Context(), p.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out["data"] = groupClients(uses, names)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// groupClients folds one row per client and key into one entry per client.
// The rows come newest first, so the clients and their keys keep that order.
func groupClients(uses []store.ClientUse, keyNames map[string]string) []connectedClient {
	out := []connectedClient{}
	index := map[string]int{}
	for _, u := range uses {
		i, ok := index[u.Client]
		if !ok {
			i = len(out)
			index[u.Client] = i
			out = append(out, connectedClient{
				Key: u.Client, Label: connect.ClientLabel(u.Client), Keys: []clientKey{},
			})
		}
		c := &out[i]
		c.Requests += u.Requests
		if len(c.Keys) == 0 {
			c.LastUsed = u.LastUsed
		}
		c.Keys = append(c.Keys, clientKey{
			ID: u.KeyID, Name: keyNames[u.KeyID], Requests: u.Requests, LastUsed: u.LastUsed,
		})
	}
	return out
}
