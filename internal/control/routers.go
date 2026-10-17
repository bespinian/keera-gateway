package control

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// maxRouterPromptBytes bounds a router's instruction, at a quarter of a
// filter's. It is sent on every request that names the router, together with
// the destination list, and it has less to say.
const maxRouterPromptBytes = 4 << 10

// listRouters is readable by every member of the organisation. A router is an
// alias developers type into their editor, so they need to find it, and to
// see why a request went where it did.
func (s *Server) listRouters(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	routers, err := s.st.ListRouters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeHookList(w, r, routers, func(from, to time.Time) (any, error) {
		return s.st.RouterStats(r.Context(), orgID, from, to)
	})
}

// routerReport is one router's own screen: where it has been sending traffic,
// and so whether it is worth having. A router's failures are quiet, because
// every request it places still gets an answer.
func (s *Server) routerReport(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if out, _, ok := hookReport(s, w, r, p, "router", s.st.RouterReportFor, s.st.Router); ok {
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// routerInput is the body of PUT /v1/routers/{alias}.
type routerInput struct {
	Mode         policy.RouterMode `json:"mode"`
	Model        string            `json:"model"`
	Prompt       string            `json:"prompt"`
	Destinations []string          `json:"destinations"`
	// Ceilings is a size router's configuration: the largest request each
	// destination should get, in estimated tokens.
	Ceilings    map[string]int `json:"ceilings"`
	Fallback    string         `json:"fallback"`
	Description string         `json:"description"`
}

// router turns the request into a router, trimmed and with duplicates removed.
func (in routerInput) router(orgID, alias string) policy.Router {
	rt := policy.Router{
		OrgID:       orgID,
		Alias:       alias,
		Mode:        in.Mode,
		Model:       strings.TrimSpace(in.Model),
		Prompt:      strings.TrimSpace(in.Prompt),
		Fallback:    strings.TrimSpace(in.Fallback),
		Description: strings.TrimSpace(in.Description),
	}
	for dest, n := range in.Ceilings {
		// A ceiling of zero means no ceiling, so only positive ones are kept.
		// That leaves one way to say it.
		if n > 0 {
			if rt.Ceilings == nil {
				rt.Ceilings = map[string]int{}
			}
			rt.Ceilings[strings.TrimSpace(dest)] = n
		}
	}
	for _, d := range in.Destinations {
		if d = strings.TrimSpace(d); d != "" && !slices.Contains(rt.Destinations, d) {
			rt.Destinations = append(rt.Destinations, d)
		}
	}
	return rt
}

func (s *Server) putRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	var in routerInput
	if !readJSON(w, r, &in) {
		return
	}
	rt := in.router(orgID, r.PathValue("alias"))
	if msg := routerProblem(rt); msg != "" {
		badRequest(w, msg)
		return
	}
	if !s.checkRouterAlias(w, r, orgID, rt.Alias) {
		return
	}
	// Checked on a one-destination router too, which decides as soon as it
	// has a second. See policy.Router.ReadsWith.
	if rt.Mode.Decides() && !s.checkReaderModel(w, r, orgID, rt.Model, routerReader, true) {
		return
	}
	if rt.Sizes() && !checkCeilings(w, rt) {
		return
	}
	if !s.checkDestinations(w, r, &rt) {
		return
	}
	_, err := s.st.Router(r.Context(), orgID, rt.Alias)
	found, ok := s.replacing(w, err)
	if !ok {
		return
	}

	saved, err := s.st.UpsertRouter(r.Context(), rt)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "router.put", "router", rt.Alias, saved)
	out := SavedRouter{Router: saved}
	if out.Warning, err = s.allowRouter(r, p, saved, found); err != nil {
		s.fail(w, err)
		return
	}
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// SavedRouter is what a PUT answers with: the router, and what the
// organisation's allow-list now does with it, when that is not what an
// administrator would expect.
type SavedRouter struct {
	policy.Router
	Warning string `json:"warning,omitempty"`
}

// allowRouter puts a new router on its organisation's allow-list, as a new
// model is, so it can be called once it is added.
//
// Allowing a router allows every destination. So a new router stays off a
// list that leaves out one of them, and a router already on the list that now
// sends to such a model opens it. Either way it says so.
func (s *Server) allowRouter(r *http.Request, p *authn.Principal, rt policy.Router, found bool,
) (string, error) {
	lim, err := s.storedLimits(r.Context(), policy.ScopeOrg, rt.OrgID)
	if err != nil || lim.AllowedModels == nil {
		return "", err
	}
	missing := ""
	for _, d := range rt.Destinations {
		if !slices.Contains(lim.AllowedModels, d) {
			missing = d
			break
		}
	}
	// A new router can be on the list already: an administrator may name an
	// alias before anything is added under it.
	listed := slices.Contains(lim.AllowedModels, rt.Alias)
	switch {
	case listed && missing != "":
		return "the organisation's allow-list names '" + rt.Alias + "', so every key that may " +
			"call it can now reach '" + missing + "', which the list leaves out. Add '" +
			missing + "' to the list, or take '" + rt.Alias + "' off it", nil
	case found || listed:
		return "", nil
	case missing != "":
		return "the organisation's allow-list leaves out '" + missing + "', so the router " +
			"was not put on it: allowing a router allows every destination. No key can call '" +
			rt.Alias + "' until the list names it", nil
	}
	allowed, err := s.st.AllowInOrg(r.Context(), rt.OrgID, rt.Alias)
	if allowed {
		s.auditf(r, p, rt.OrgID, "guardrail.allow_model", string(policy.ScopeOrg), rt.OrgID, rt.Alias)
	}
	return "", err
}

// routerProblem says what is wrong with a router's own fields, or "" when
// nothing is. It reads nothing from the store.
func routerProblem(rt policy.Router) string {
	switch {
	case !policy.ValidAlias(rt.Alias):
		return badAlias(rt.Alias)
	case !rt.Mode.Valid():
		return "'mode' must be instruction, size, fallback, latency or least-busy, not '" +
			string(rt.Mode) + "'"
	case len(rt.Ceilings) > 0 && !rt.Sizes():
		return "'ceilings' only works on a router with 'mode': 'size'"
	}
	if msg := leftoverProblem(rt); msg != "" {
		return msg
	}
	switch {
	case rt.Mode.Decides() && rt.Prompt == "":
		return "'prompt' is required: it tells the deciding model how to choose a destination"
	case len(rt.Prompt) > maxRouterPromptBytes:
		return fmt.Sprintf("'prompt' is %d bytes; the limit is %d, since it is sent with every request",
			len(rt.Prompt), maxRouterPromptBytes)
	}
	return destinationCountProblem(rt)
}

// leftoverProblem refuses a model, prompt or fallback on a router that decides
// nothing. None of them would do anything, but a reader would take them as the
// reason its traffic goes where it goes.
func leftoverProblem(rt policy.Router) string {
	if rt.Mode.Decides() {
		return ""
	}
	mode := "a " + string(rt.Mode) + " router"
	switch {
	case rt.Model != "":
		return "'model' must be empty: " + mode + " has no deciding model"
	case rt.Prompt != "":
		return "'prompt' must be empty: " + mode + " has no deciding model to give it to"
	case rt.Fallback != "":
		return "'fallback' must be empty: " + mode + " already tries its destinations in turn, " +
			"so put '" + rt.Fallback + "' in 'destinations'"
	}
	return ""
}

// destinationCountProblem refuses a router with no destinations or too many,
// in the words that fit its mode. One is enough: it gives clients a name that
// stays the same while the model behind it changes.
func destinationCountProblem(rt policy.Router) string {
	n := len(rt.Destinations)
	switch {
	case n == 0:
		return "'destinations' needs at least one model to send requests to"
	case n > policy.MaxDestinations && rt.Decides():
		// A small model asked to pick one word from a longer list tends to
		// name whichever end of it it read last.
		return fmt.Sprintf("'destinations' names %d models; the limit is %d, "+
			"since a longer list is too much for the deciding model to choose from",
			n, policy.MaxDestinations)
	case n > policy.MaxDestinations:
		return fmt.Sprintf("'destinations' names %d models; the limit is %d, "+
			"since each one tried adds to the client's wait",
			n, policy.MaxDestinations)
	}
	return ""
}

// checkRouterAlias refuses a router named like a model the organisation can
// call. A client names both in the same field and the model wins, so such a
// router would never be reached.
func (s *Server) checkRouterAlias(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	_, err := s.st.Model(r.Context(), orgID, alias)
	return !s.aliasTaken(w, err, "'"+alias+"' is already a model this organisation can call. "+
		"A client names a router in the same field it names a model, and the model wins "+
		"there, so this router would never be reached - give it a different alias")
}

// checkCeilings refuses a size router that could not place every request.
//
// Every ceiling has to name a destination; one that does not is most likely
// a typo. And exactly one destination must have no ceiling: with none, a
// large request has nowhere to go, and with more, only the first is ever
// chosen.
func checkCeilings(w http.ResponseWriter, rt policy.Router) bool {
	for alias := range rt.Ceilings {
		if !rt.Offers(alias) {
			badRequest(w, "'ceilings' names '"+alias+"', which is not one of this router's destinations")
			return false
		}
	}
	switch unbounded := rt.Unbounded(); len(unbounded) {
	case 1:
		return true
	case 0:
		badRequest(w, "leave the ceiling off one destination, usually the largest model, "+
			"so the largest requests have somewhere to go")
		return false
	default:
		badRequest(w, "exactly one destination may have no ceiling, but '"+
			strings.Join(unbounded, "' and '")+"' have none")
		return false
	}
}

// subscriptionsOff refuses a subscription model or key while the deployment
// does not take Claude subscriptions.
const subscriptionsOff = "this deployment does not take Claude subscriptions; " +
	"its operator turns them on with KEERA_CLAUDE_SUBSCRIPTIONS"

// subscriptionReader refuses a subscription model as the model a filter or a
// router runs on. Only a caller's own sign-in reaches it, and that sign-in is
// for the caller's request alone.
func subscriptionReader(alias, what string) string {
	return "'" + alias + "' is paid by each person's own Claude subscription, so the gateway " +
		"cannot run a " + what + " on it; choose a model the organisation pays for"
}

// checkDestinations refuses a router that could not send a request everywhere
// it claims to.
//
// The request path already skips destinations it cannot reach, so this is
// not about safety. It makes the mistake visible: a router that quietly picks
// from two models instead of four still produces only successful requests.
func (s *Server) checkDestinations(w http.ResponseWriter, r *http.Request,
	rt *policy.Router,
) bool {
	for _, alias := range rt.Destinations {
		m, err := s.st.Model(r.Context(), rt.OrgID, alias)
		switch {
		case errors.Is(err, store.ErrNotFound):
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found",
				"this organisation has no model '"+alias+"'; a router's destinations are its "+
					"models")
			return false
		case err != nil:
			s.fail(w, err)
			return false
		case m.Kind != policy.KindChat:
			badRequest(w, "'"+alias+"' is a "+string(m.Kind)+" model; a router is reached on a chat "+
				"request, so everything it may choose between has to be able to answer one")
			return false
		case m.Subscription:
			badRequest(w, "'"+alias+"' is paid by each person's own Claude subscription, and only "+
				"Claude Code signed in to one reaches it; a router cannot send a request there")
			return false
		}
	}
	if rt.Fallback != "" && !rt.Offers(rt.Fallback) {
		badRequest(w, "'fallback' is '"+rt.Fallback+"', which is not one of this router's destinations. "+
			"The fallback is where a request goes when the decision could not be made, so "+
			"it has to be somewhere this router was allowed to send it in the first place")
		return false
	}
	return true
}

// deleteRouter keeps a router that is still in use. Editors that name it are
// out of sight. What can be seen are the scopes whose allow-list names it:
// they would be left able to reach nothing. The organisation's own list names
// every router it allowed when it was added, so there the router is taken off
// with it, unless it is all the list names.
func (s *Server) deleteRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	s.deleteUnused(w, r, p, "router", s.st.RouterUsers, s.st.DeleteRouter, func(alias, users string) string {
		return "the router '" + alias + "' is in the allow-list of " + users +
			". Every request naming it would be answered 'model not found', so take it " +
			"off those allow-lists first"
	})
}

// checkRouter puts sample prompts through a router and shows where each went.
//
// A wrong router still answers every request, just from the wrong model. The
// likeliest mistake is an instruction that always picks the same destination,
// which costs a generation per request and routes nothing.
func (s *Server) checkRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	rt, err := s.st.Router(r.Context(), orgID, r.PathValue("alias"))
	if err != nil {
		s.fail(w, err)
		return
	}
	probe := s.opts.Gateway.CheckRouter(r.Context(), rt)
	s.auditf(r, p, orgID, "router.check", "router", rt.Alias, map[string]any{
		"ok": probe.OK, "error": probe.Error,
	})
	httpx.WriteJSON(w, http.StatusOK, probe)
}
