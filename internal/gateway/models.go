package gateway

import (
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// modelEntry is one model or router as /v1/models lists it.
type modelEntry struct {
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
	// Location says where prompts sent to the model go.
	Location string `json:"location,omitempty"`
	// Destinations is set only on routers, which is also how a client
	// tells a router from a model.
	Destinations []string `json:"destinations,omitempty"`
}

func modelEntryOf(m policy.Model) modelEntry {
	return modelEntry{
		ID: m.Alias, Object: "model", Created: created(m), OwnedBy: "keera",
		MaxContext: m.MaxContext, Description: m.Description, Location: m.Location,
	}
}

// routerEntryOf lists a router as a model, because a client names it in the
// same field. A router has no context window of its own, so none is given.
func routerEntryOf(rt policy.Router) modelEntry {
	return modelEntry{
		ID: rt.Alias, Object: "model", OwnedBy: "keera",
		Description: rt.Description, Destinations: rt.Destinations,
	}
}

// listModels answers /v1/models with the models and routers this key may use.
// Clients discover through it, so it must never leak one the key cannot reach.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return
	}
	out := []modelEntry{}
	for _, m := range s.src.Models(res.Key.OrgID) {
		if m.Enabled && res.MayUse(m) {
			out = append(out, modelEntryOf(m))
		}
	}
	for _, rt := range s.src.Routers(res.Key.OrgID) {
		if res.AllowsModel(rt.Alias) && !res.Key.Subscription() {
			out = append(out, routerEntryOf(rt))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	if res.AllowsModel(alias) {
		if m, found := s.src.Model(res.Key.OrgID, alias); found && m.Enabled && res.MayUse(m) {
			httpx.WriteJSON(w, http.StatusOK, modelEntryOf(m))
			return
		}
		if rt, found := s.src.Router(res.Key.OrgID, alias); found && !res.Key.Subscription() {
			httpx.WriteJSON(w, http.StatusOK, routerEntryOf(rt))
			return
		}
	}
	httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
		s.advise("the model '"+alias+"' does not exist or this key may not use it"))
}

// created is the model's release date as a Unix time, which is what OpenAI's
// "created" field holds. Zero when no date is stated.
func created(m policy.Model) int64 {
	t, err := time.Parse(time.DateOnly, m.ReleaseDate)
	if err != nil {
		return 0
	}
	return t.Unix()
}
