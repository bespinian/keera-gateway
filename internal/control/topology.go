package control

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/connect"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The deployment as a picture: which clients call this gateway, which models
// they reach, and which of those run outside the customer's boundary. A model
// in the cluster and one at a hosted provider are one row apart in the
// catalogue, but mean very different things. A member sees only the traffic of
// their own keys, like the request log.

// mapClient is one thing that has been calling the gateway. Clients are not
// registered, so the list is whatever called in the window.
type mapClient struct {
	Client string `json:"client"`
	Label  string `json:"label"`
	store.Cell
}

// mapModel is one model the gateway can forward to, with which side of the
// customer's boundary it runs on.
type mapModel struct {
	Alias   string `json:"alias"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Hosting string `json:"hosting"`
	// Endpoint is the host serving it. Only administrators get it, as on the
	// Models screen.
	Endpoint string `json:"endpoint,omitempty"`
	store.Cell
}

// trafficMap serves the map: the catalogue as nodes, the window as numbers on
// them, and the traffic between them as edges.
func (s *Server) trafficMap(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	holder, ok := s.trafficHolder(w, p, "")
	if !ok {
		return
	}
	orgID, from, to, ok := s.reportOrg(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	rep, err := s.st.Flows(ctx, orgID, holder, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	models, err := s.st.ListModels(ctx, orgID)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Every one of the organisation's models is drawn, even one nobody called:
	// a quiet hosted model is still a way out of the building. An alias it has
	// no model for, a typo or a deleted model, is not: there is nowhere to draw
	// it. Its requests still count on the gateway and the client.
	out := make([]mapModel, 0, len(models))
	drawn := make(map[string]bool, len(models))
	for _, m := range models {
		drawn[m.Alias] = true
		n := modelNode(m, rep.Models[m.Alias])
		if holder != "" {
			n.Endpoint = ""
		}
		out = append(out, n)
	}
	// Busiest first, so a crowded map shows the models that matter.
	slices.SortFunc(out, func(a, b mapModel) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Alias, b.Alias))
	})

	clients := make([]mapClient, 0, len(rep.Clients))
	for key, cell := range rep.Clients {
		clients = append(clients, mapClient{
			Client: key, Label: connect.ClientLabel(key), Cell: cell,
		})
	}
	slices.SortFunc(clients, func(a, b mapClient) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Client, b.Client))
	})

	flows := slices.DeleteFunc(rep.Flows, func(f store.Flow) bool { return !drawn[f.Alias] })

	recent, err := s.st.LastMinute(ctx, orgID, holder, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"from": from,
		"to":   to,
		// The address in the middle of the drawing.
		"gateway_url": s.gatewayURL(r),
		"gateway":     rep.Total,
		"clients":     clients,
		"models":      out,
		"flows":       flows,
		// Requests per second over the last minute, newest first. Seconds
		// ago rather than times, so a browser clock that is off does not
		// matter.
		"last_minute": recent,
	})
}

func modelNode(m policy.Model, cell store.Cell) mapModel {
	return mapModel{
		Alias:    m.Alias,
		Kind:     string(m.Kind),
		Enabled:  m.Enabled,
		Hosting:  string(m.Hosting()),
		Endpoint: m.Endpoint(),
		Cell:     cell,
	}
}
