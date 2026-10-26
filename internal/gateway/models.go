package gateway

import (
	"cmp"
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// modelEntry is one model or router as /v1/models lists it, with the fields of
// both OpenAI's and Anthropic's model list.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Type and CreatedAt are Anthropic's names for Object and Created.
	Type      string `json:"type"`
	CreatedAt string `json:"created_at,omitempty"`
	// MaxContext is not an OpenAI field, but lets clients size a request.
	MaxContext int `json:"max_context_tokens,omitempty"`
	// DisplayName falls back to the alias, since Anthropic always sends one.
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	// Location says where prompts sent to the model go.
	Location string `json:"location,omitempty"`
	// Destinations is set only on routers, which tells them from models.
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

// routerEntryOf lists a router as a model, since a client names it in the same
// field. It has no context window of its own.
func (s *Server) routerEntryOf(p policy.Router) modelEntry {
	return modelEntry{
		ID: p.Alias, Object: "model", Type: "model", OwnedBy: "keera",
		DisplayName: cmp.Or(p.DisplayName, p.Alias),
		Description: p.Description, Destinations: p.Reaches(),
	}
}

// listModels answers /v1/models with the models and routers this key may use,
// and never one it cannot reach.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	res, ok := s.authenticate(w, r, openAIShape{})
	if !ok {
		return
	}
	out := []modelEntry{}
	for _, m := range s.src.Models(res.Key.OrgID) {
		// Locked models would only refuse, and decision models serve only
		// routers and gates.
		if m.Enabled && !m.Locked && m.Kind != policy.KindDecision && res.MayUse(m) {
			out = append(out, modelEntryOf(m))
		}
	}
	for _, p := range s.src.Routers(res.Key.OrgID) {
		if res.MayCallRouter(p.Alias) {
			out = append(out, s.routerEntryOf(p))
		}
	}
	// The list is never paged, but Anthropic's SDKs read its paging fields.
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
	// A locked model is not listed, so it is not described either.
	case !ok, !n.viaRouter() && n.model.Locked:
		httpx.WriteNotFound(w, "model_not_found", s.advise(modelNotFound(alias)))
	case n.viaRouter():
		httpx.WriteJSON(w, http.StatusOK, s.routerEntryOf(n.router))
	default:
		httpx.WriteJSON(w, http.StatusOK, modelEntryOf(n.model))
	}
}

// named is a model or router named by a request's 'model' field.
type named struct {
	model  policy.Model
	router policy.Router
}

func (n named) viaRouter() bool { return n.router.Alias != "" }

// callable decides what a key may name as its model: a model it may use, or a
// router. kind narrows it to one API surface; empty accepts any. Routers
// answer only chat, and decision models no client.
//
// Every route that takes a model name asks here. Missing and forbidden are
// both "not found", so other projects' models cannot be found through 403s.
func (s *Server) callable(res *policy.Resolved, alias string, kind policy.Kind) (named, bool) {
	if alias == "" {
		return named{}, false
	}
	// Models first. The control plane keeps router and model aliases apart.
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

// servedAt names the endpoints that serve a model, for a request that used
// the wrong endpoint. It is "" for a model the key may not use.
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
	return ""
}

// created is the model's release date as Unix time, or 0.
func created(m policy.Model) int64 {
	t, err := time.Parse(time.DateOnly, m.ReleaseDate)
	if err != nil {
		return 0
	}
	return t.Unix()
}
