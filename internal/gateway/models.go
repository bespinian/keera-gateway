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
		// A locked model would only refuse, so a client is not offered it.
		if m.Enabled && !m.Locked && res.MayUse(m) {
			out = append(out, modelEntryOf(m))
		}
	}
	for _, rt := range s.src.Routers(res.Key.OrgID) {
		if res.MayRoute(rt.Alias) {
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
	n, ok := s.callable(res, alias, "")
	switch {
	// A locked model is left out of the list, so it is not described either.
	case !ok, !n.routed && n.model.Locked:
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			s.advise(modelNotFound(alias)))
	case n.routed:
		httpx.WriteJSON(w, http.StatusOK, routerEntryOf(n.router))
	default:
		httpx.WriteJSON(w, http.StatusOK, modelEntryOf(n.model))
	}
}

// named is what a request's 'model' field names: a model, or a router.
type named struct {
	model  policy.Model
	router policy.Router
	routed bool
}

// callable decides what a key may name as its model: a model it may use, or
// else one of its organisation's routers. kind narrows it to one API surface,
// and empty accepts any. Routers answer only chat.
//
// Every route that takes a model name asks here, so they all agree. What is
// missing and what is forbidden are both "not found", so other projects'
// models cannot be discovered through 403s.
func (s *Server) callable(res *policy.Resolved, alias string, kind policy.Kind) (named, bool) {
	if alias == "" {
		return named{}, false
	}
	// A model is looked up first. The control plane refuses a router named
	// like a model, and a model named like a router, so the two rarely meet.
	if m, found := s.findModel(res.Key, alias); found {
		ok := m.Enabled && (kind == "" || m.Kind == kind) && res.MayUse(m)
		return named{model: m}, ok
	}
	if kind != "" && kind != policy.KindChat {
		return named{}, false
	}
	if rt, found := s.src.Router(res.Key.OrgID, alias); found && res.MayRoute(alias) {
		return named{router: rt, routed: true}, true
	}
	return named{}, false
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
