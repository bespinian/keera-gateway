package control

import (
	"net/http"
	"sort"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The deployment as a picture: which clients call this gateway, which models
// they reach through it, and which of those models run outside the
// customer's boundary. A model in the cluster and one at a hosted provider
// are one row apart in the catalogue, but mean very different things.
//
// Administrator-only, like the request log: it shows every client and model in
// the organisation. Members see their own traffic on My access.

// mapClient is one thing that has been calling the gateway.
//
// Clients are not registered; they just arrive with a key. So the list is
// whatever called in the window, and a client that stopped calling drops off
// the map.
type mapClient struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	store.Cell
}

// mapModel is one model the gateway can forward to, with which side of the
// customer's boundary it runs on.
type mapModel struct {
	Alias   string `json:"alias"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Hosting string `json:"hosting"`
	// Endpoint is the host serving it. An external one is the provider's
	// public address and is shown to everyone; an internal one is private
	// layout and shown only to an operator, as on the Models screen.
	Endpoint string `json:"endpoint,omitempty"`
	// Retired marks traffic to an alias the catalogue no longer has. The
	// traffic was real, so it is drawn, but where it went is unknown.
	Retired bool `json:"retired,omitempty"`
	store.Cell
}

// trafficMap serves the map: the catalogue as nodes, the window as numbers on
// them, and the traffic between them as edges.
func (s *Server) trafficMap(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the deployment map")
		return
	}
	orgID, from, to, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	rep, err := s.st.Flows(ctx, orgID, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	models, err := s.st.LoadModels(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Every catalogue model is drawn, even one nobody called: a quiet hosted
	// model is still a way out of the building.
	out := make([]mapModel, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		seen[m.Alias] = true
		out = append(out, modelNode(m, rep.Models[m.Alias], p))
	}
	for alias, cell := range rep.Models {
		if seen[alias] {
			continue
		}
		out = append(out, mapModel{
			Alias: alias, Hosting: string(policy.HostedUnknown), Retired: true, Cell: cell,
		})
	}
	// Busiest first, so a crowded map shows the models that matter.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Alias < out[j].Alias
	})

	clients := make([]mapClient, 0, len(rep.Clients))
	for key, cell := range rep.Clients {
		clients = append(clients, mapClient{
			Key: key, Label: connect.ClientLabel(key), Cell: cell,
		})
	}
	sort.Slice(clients, func(i, j int) bool {
		if clients[i].Requests != clients[j].Requests {
			return clients[i].Requests > clients[j].Requests
		}
		return clients[i].Key < clients[j].Key
	})

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"from":     from,
		"to":       to,
		"currency": s.opts.Currency,
		// The address in the middle of the drawing, which every arrow passes
		// through.
		"gateway_url": s.gatewayURL(r),
		"gateway":     rep.Total,
		"clients":     clients,
		"models":      out,
		"flows":       rep.Flows,
	})
}

// modelNode is one catalogue entry as the map draws it.
func modelNode(m policy.Model, cell store.Cell, p *authn.Principal) mapModel {
	hosting := m.Hosting()
	node := mapModel{
		Alias:   m.Alias,
		Kind:    string(m.Kind),
		Enabled: m.Enabled,
		Hosting: string(hosting),
		Cell:    cell,
	}
	if hosting == policy.HostedExternal || p.CanAdminCatalogue() {
		node.Endpoint = m.Endpoint()
	}
	return node
}
