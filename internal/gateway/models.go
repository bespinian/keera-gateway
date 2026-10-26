package gateway

import (
	"cmp"
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// modelEntry is one model or router as /v1/models lists it. It carries the
// fields of both OpenAI's and Anthropic's model list, so an SDK of either
// reads it.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Type and CreatedAt are Anthropic's names for Object and Created.
	Type      string `json:"type"`
	CreatedAt string `json:"created_at,omitempty"`
	// MaxContext is not part of the OpenAI shape, but clients that know to
	// look for it can size a request without being told out of band.
	MaxContext int `json:"max_context_tokens,omitempty"`
	// DisplayName is the model's or router's name for people, as Anthropic's
	// API calls it. Anthropic's has one for every model, so the alias stands
	// in.
	DisplayName string `json:"display_name"`
	// Description says what the model is for, so a person choosing has
	// more to go on than the name.
	Description string `json:"description,omitempty"`
	// Location says where prompts sent to the model go.
	Location string `json:"location,omitempty"`
	// Destinations is set only on routers, which is also how a client
	// tells them from a model.
	Destinations []string `json:"destinations,omitempty"`
}

func modelEntryOf(m policy.Model) modelEntry {
	e := modelEntry{
		ID: m.Alias, Object: "model", Type: "model", Created: created(m), OwnedBy: "keera",
		MaxContext: m.MaxContext, DisplayName: cmp.Or(m.DisplayName, m.Alias),
		Description: m.Description, Location: m.Location,
	}
	if e.Created != 0 {
		e.CreatedAt = time.Unix(e.Created, 0).UTC().Format(time.RFC3339)
	}
	return e
}

// routerEntryOf lists a router as a model, because a client names it in
// the same field. Its destinations are every model it may send to. It has no
// context window of its own, so none is given.
func (s *Server) routerEntryOf(p policy.Router) modelEntry {
	return modelEntry{
		ID: p.Alias, Object: "model", Type: "model", OwnedBy: "keera",
		DisplayName: cmp.Or(p.DisplayName, p.Alias),
		Description: p.Description, Destinations: p.Reaches(),
	}
}

// listModels answers /v1/models with the models and routers this key may
// use. Clients discover through it, so it must never leak one the key cannot
// reach.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return
	}
	out := []modelEntry{}
	for _, m := range s.src.Models(res.Key.OrgID) {
		// A locked model would only refuse, so a client is not offered it.
		// Nor a decision model, which answers only routers and gates.
		if m.Enabled && !m.Locked && m.Kind != policy.KindDecision && res.MayUse(m) {
			out = append(out, modelEntryOf(m))
		}
	}
	for _, p := range s.src.Routers(res.Key.OrgID) {
		if res.MayCallRouter(p.Alias) {
			out = append(out, s.routerEntryOf(p))
		}
	}
	// The list is never paged. has_more and the two ids are Anthropic's
	// paging fields, which its SDKs read.
	list := map[string]any{
		"object": "list", "data": out, "has_more": false,
		"first_id": nil, "last_id": nil,
	}
	if len(out) > 0 {
		list["first_id"], list["last_id"] = out[0].ID, out[len(out)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, list)
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
	case !ok, !n.viaRouter() && n.model.Locked:
		httpx.WriteNotFound(w, "model_not_found", s.advise(modelNotFound(alias)))
	case n.viaRouter():
		httpx.WriteJSON(w, http.StatusOK, s.routerEntryOf(n.router))
	default:
		httpx.WriteJSON(w, http.StatusOK, modelEntryOf(n.model))
	}
}

// named is what a request's 'model' field names: a model or a router.
type named struct {
	model  policy.Model
	router policy.Router
}

func (n named) viaRouter() bool { return n.router.Alias != "" }

// callable decides what a key may name as its model: a model it may use, or
// else one of its organisation's routers. kind narrows it to one API
// surface, and empty accepts any. Routers answer only chat, and a decision
// model no client at all.
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
		ok := m.Enabled && m.Kind != policy.KindDecision && (kind == "" || m.Kind == kind) &&
			res.MayUse(m)
		return named{model: m}, ok
	}
	if kind != "" && kind != policy.KindChat {
		return named{}, false
	}
	if p, found := s.src.Router(res.Key.OrgID, alias); found && res.MayCallRouter(alias) {
		return named{router: p}, true
	}
	return named{}, false
}

// servedAt names the endpoints a model the key may use is served at, for a
// request that named it at an endpoint for another kind of model. It is ""
// for any other model, so a refusal says nothing a key could not list.
func (s *Server) servedAt(res *policy.Resolved, alias string) string {
	m, found := s.findModel(res.Key, alias)
	if !found || !m.Enabled || !res.MayUse(m) {
		return ""
	}
	switch m.Kind {
	case policy.KindChat:
		return "/v1/chat/completions, /v1/messages or /v1/responses"
	case policy.KindCompletion:
		return "/v1/completions"
	case policy.KindEmbedding:
		return "/v1/embeddings"
	}
	// No client can name a decision model.
	return ""
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
