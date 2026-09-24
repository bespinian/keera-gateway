package gateway

import (
	"net/http"

	"github.com/bespinian/keera-gateway/internal/httpx"
)

// listModels answers /v1/models with the models this key may use. Clients
// discover through it, so it must never leak a model the key cannot reach.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		// MaxContext is not part of the OpenAI shape, but clients that know to
		// look for it can size a request without being told out of band.
		MaxContext int `json:"max_context,omitempty"`
		// Description says what the model is for, so a person choosing has
		// more to go on than the name.
		Description string `json:"description,omitempty"`
		// Destinations is set only on routers, which is also how a client
		// tells a router from a model.
		Destinations []string `json:"destinations,omitempty"`
	}
	out := []model{}
	for _, m := range s.src.Models() {
		if !m.Enabled || !res.AllowsModel(m.Alias) {
			continue
		}
		out = append(out, model{
			ID: m.Alias, Object: "model", Created: 0, OwnedBy: "keera", MaxContext: m.MaxContext,
			Description: m.Description,
		})
	}
	// The organisation's routers are listed as models, because a client names
	// them in the same field. Only the ones this key may use are listed. A
	// router has no context window of its own, so none is given.
	for _, rt := range s.src.Routers(res.Key.OrgID) {
		if !res.AllowsModel(rt.Alias) {
			continue
		}
		out = append(out, model{
			ID: rt.Alias, Object: "model", Created: 0, OwnedBy: "keera",
			Description: rt.Description, Destinations: rt.Destinations,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	m, found := s.src.Model(alias)
	if found && m.Enabled && res.AllowsModel(alias) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"id": m.Alias, "object": "model", "created": 0, "owned_by": "keera",
			"max_context": m.MaxContext, "description": m.Description,
		})
		return
	}
	// A router found in the list can be looked up here too. It reports its
	// destinations instead of a context window.
	if rt, ok := s.src.Router(res.Key.OrgID, alias); ok && res.AllowsModel(alias) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"id": rt.Alias, "object": "model", "created": 0, "owned_by": "keera",
			"description": rt.Description, "destinations": rt.Destinations,
		})
		return
	}
	httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
		"the model '"+alias+"' does not exist or you do not have access to it")
}
