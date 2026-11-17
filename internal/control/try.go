package control

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
)

// A try puts an administrator's own prompts through a filter or route step and
// shows everything its model was sent and answered. See gateway/try.go.

const (
	// maxTryPrompts keeps one try to a short wait: each prompt is a
	// generation, two when the model shows no reasoning and is asked why.
	maxTryPrompts = 10
	// maxTryPromptBytes leaves room for a pasted file or stack trace, and for
	// all ten prompts within the body limit.
	maxTryPromptBytes = 64 << 10
)

// TryRequest is the prompts to try, each sent as a request of its own.
type TryRequest struct {
	Prompts []string `json:"prompts"`
}

// RouterTry is one try per route step that reads requests.
type RouterTry struct {
	Router string             `json:"router"`
	Routes []gateway.RouteTry `json:"routes"`
}

// readTry reads and bounds a try's prompts.
func readTry(w http.ResponseWriter, r *http.Request) (TryRequest, bool) {
	var req TryRequest
	if !readJSON(w, r, &req) {
		return req, false
	}
	switch {
	case len(req.Prompts) == 0:
		badRequest(w, "give at least one prompt to try")
		return req, false
	case len(req.Prompts) > maxTryPrompts:
		badRequest(w, fmt.Sprintf("give at most %d prompts at a time", maxTryPrompts))
		return req, false
	}
	for i, prompt := range req.Prompts {
		switch {
		case strings.TrimSpace(prompt) == "":
			badRequest(w, fmt.Sprintf("prompt %d is empty", i+1))
			return req, false
		case len(prompt) > maxTryPromptBytes:
			badRequest(w, fmt.Sprintf("prompt %d is longer than %d KiB", i+1, maxTryPromptBytes>>10))
			return req, false
		}
	}
	return req, true
}

// tryFilter sends prompts through one filter and shows what its model was
// sent, what it answered, and why. The audit entry counts the prompts but
// keeps none of them, like every filter run.
func (s *Server) tryFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	f, err := s.st.Filter(r.Context(), orgID, r.PathValue("alias"))
	if err != nil {
		s.fail(w, err)
		return
	}
	req, ok := readTry(w, r)
	if !ok {
		return
	}
	res := s.opts.Gateway.TryFilter(r.Context(), f, req.Prompts)
	s.auditf(r, p, orgID, "filter.try", "filter", f.Alias, map[string]any{
		"prompts": len(req.Prompts), "error": res.Error,
	})
	httpx.WriteJSON(w, http.StatusOK, res)
}

// tryRouter sends prompts through a router's instruction steps that have more
// than one destination, or the one named by ?step=. The others read no
// prompt, so there is nothing to try.
func (s *Server) tryRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	rtr, err := s.st.Router(r.Context(), orgID, r.PathValue("alias"))
	if err != nil {
		s.fail(w, err)
		return
	}
	req, ok := readTry(w, r)
	if !ok {
		return
	}
	step := r.URL.Query().Get("step")
	out := RouterTry{Router: rtr.Alias, Routes: []gateway.RouteTry{}}
	for _, rt := range rtr.Routes() {
		if rt.Step == step || step == "" && rt.Decides() {
			out.Routes = append(out.Routes, s.opts.Gateway.TryRoute(r.Context(), rt, req.Prompts))
		}
	}
	if len(out.Routes) == 0 {
		msg := "the router '" + rtr.Alias + "' has no route step that reads requests to " +
			"choose between models, so there is nothing to try; its check shows where it " +
			"sends them"
		if step != "" {
			msg = "the router '" + rtr.Alias + "' has no route step '" + step + "'"
		}
		badRequest(w, msg)
		return
	}
	s.auditf(r, p, orgID, "router.try", "router", rtr.Alias, map[string]any{
		"step": step, "prompts": len(req.Prompts),
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}
