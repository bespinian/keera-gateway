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

// routerOrg resolves which organisation a router request is about. Like a
// filter, a router belongs to one organisation.
func (s *Server) routerOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) (string, bool) {
	return s.requireOrg(w, p, r.URL.Query().Get("org_id"),
		"choose an organisation first; a router belongs to one")
}

// routerAdmin is routerOrg for a request that changes a router.
func (s *Server) routerAdmin(w http.ResponseWriter, r *http.Request, p *authn.Principal) (string, bool) {
	orgID, ok := s.routerOrg(w, r, p)
	return orgID, ok && s.requireOrgAdmin(w, p, orgID)
}

// listRouters is readable by every member of the organisation. A router is an
// alias developers type into their editor, so they need to find it, and to
// see why a request went where it did.
func (s *Server) listRouters(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.routerOrg(w, r, p)
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
	orgID, ok := s.routerOrg(w, r, p)
	if !ok {
		return
	}
	from, to, ok := queryWindow(w, r.URL.Query())
	if !ok {
		return
	}
	alias := r.PathValue("alias")

	report, err := s.st.RouterReportFor(r.Context(), orgID, alias, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"report": report, "currency": s.opts.Currency}
	// A deleted router keeps its traffic, so the report is served either way
	// and "router" is left out rather than answering 404.
	rt, err := s.st.Router(r.Context(), orgID, alias)
	switch {
	case err == nil:
		out["router"] = rt
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ordersBy says, for the error messages, what orders the destinations of a
// router that reads nothing.
func ordersBy(mode policy.RouterMode) string {
	switch mode {
	case policy.RouterModeLatency:
		return "what each of them has lately taken to begin answering, fastest first"
	case policy.RouterModeLeastBusy:
		return "how many requests this gateway has in flight against each, emptiest first"
	case policy.RouterModeSize:
		return "how much text is in the request, against the ceiling set on each"
	default:
		return "the order they are written in"
	}
}

// readsNothing says, for the error messages, what a router with no deciding
// model reads. A size router does read the request's length, so it must not be
// told it "reads nothing".
func readsNothing(mode policy.RouterMode) string {
	if mode.Sizes() {
		return "it reads how much text is in the request and nothing of what the request says"
	}
	return "it reads nothing and decides nothing"
}

// routerInput is the body of PUT /v1/routers/{alias}.
type routerInput struct {
	Mode         policy.RouterMode `json:"mode"`
	Model        string            `json:"model"`
	Prompt       string            `json:"prompt"`
	Destinations []string          `json:"destinations"`
	// Ceilings is a size router's configuration: the largest request each
	// destination should get, in estimated tokens.
	Ceilings map[string]int `json:"ceilings"`
	// Fallback is a pointer so that leaving it out is not the same as
	// clearing it.
	Fallback    *string `json:"fallback"`
	Description string  `json:"description"`
}

// router turns the request into a router, trimmed and with duplicates removed.
func (in routerInput) router(orgID, alias string) policy.Router {
	rt := policy.Router{
		OrgID:       orgID,
		Alias:       alias,
		Mode:        in.Mode,
		Model:       strings.TrimSpace(in.Model),
		Prompt:      strings.TrimSpace(in.Prompt),
		Description: strings.TrimSpace(in.Description),
	}
	// A missing mode means instruction, the only mode older clients know.
	if rt.Mode == "" {
		rt.Mode = policy.RouterModeInstruction
	}
	if in.Fallback != nil {
		rt.Fallback = strings.TrimSpace(*in.Fallback)
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
		// A destination named twice is one destination.
		if d = strings.TrimSpace(d); d != "" && !slices.Contains(rt.Destinations, d) {
			rt.Destinations = append(rt.Destinations, d)
		}
	}
	return rt
}

func (s *Server) putRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.routerAdmin(w, r, p)
	if !ok {
		return
	}
	var in routerInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	rt := in.router(orgID, r.PathValue("alias"))
	if msg := routerProblem(rt); msg != "" {
		badRequest(w, msg)
		return
	}
	if !s.checkRouterAlias(w, r, rt.Alias) {
		return
	}
	if rt.Decides() && !s.checkRouterModel(w, r, rt.Model) {
		return
	}
	if rt.Sizes() && !checkCeilings(w, rt) {
		return
	}
	if !s.checkDestinations(w, r, &rt) {
		return
	}

	saved, err := s.st.UpsertRouter(r.Context(), rt)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "router.put", "router", rt.Alias, saved)
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// routerProblem says what is wrong with a router's own fields, or "" when
// nothing is. It reads nothing from the store.
func routerProblem(rt policy.Router) string {
	switch {
	case !policy.ValidRouterAlias(rt.Alias):
		return "a router's alias goes where a model's alias goes - into a developer's own client " +
			"configuration - so it is lowercase letters, digits and inner hyphens: " +
			"'auto', not '" + rt.Alias + "'"
	case !policy.ValidRouterMode(rt.Mode):
		return "'mode' is '" + string(rt.Mode) + "'; a router either reads each request with a model " +
			"and sends it where that model says ('instruction'), places it by how much " +
			"text is in it ('size'), or tries its destinations until one answers - in " +
			"the order they are written ('fallback'), fastest first by what they have " +
			"lately taken to begin answering ('latency'), or emptiest first by what the " +
			"gateway has in flight against each ('least-busy')"
	case len(rt.Ceilings) > 0 && !rt.Sizes():
		return "'ceilings' is set, and this is a " + string(rt.Mode) + " router: what orders its " +
			"destinations is " + ordersBy(rt.Mode) + ", and nothing there reads the size of " +
			"a request. Use 'mode': 'size' for a router that does"
	}
	if msg := leftoverProblem(rt); msg != "" {
		return msg
	}
	switch {
	case rt.Decides() && rt.Prompt == "":
		return "'prompt' is required; it is the instruction the deciding model is given, and a " +
			"router without one would be shown a list of models on every request with " +
			"nothing to choose between them by"
	case len(rt.Prompt) > maxRouterPromptBytes:
		return fmt.Sprintf("'prompt' is %d bytes; the limit is %d, because this text is sent to "+
			"the deciding model on every request that names this router",
			len(rt.Prompt), maxRouterPromptBytes)
	}
	return destinationCountProblem(rt)
}

// leftoverProblem refuses a model, prompt or fallback on a router that decides
// nothing. None of them would do anything, but a reader would take them as the
// reason its traffic goes where it goes.
func leftoverProblem(rt policy.Router) string {
	if rt.Decides() {
		return ""
	}
	mode := "a " + string(rt.Mode) + " router"
	switch {
	case rt.Model != "":
		return "'model' is set, and this is " + mode + ": " + readsNothing(rt.Mode) + ", so there " +
			"is no deciding model. Its destinations are tried until one of them " +
			"answers, and what orders them is " + ordersBy(rt.Mode)
	case rt.Prompt != "":
		return "'prompt' is set, and this is " + mode + ": " + readsNothing(rt.Mode) + ", so there " +
			"is nothing an instruction would be given to"
	case rt.Fallback != "":
		return "'fallback' is set, and this is " + mode + ": its whole list is the " +
			"fallback, since the next destination is what happens when one fails. " +
			"Put '" + rt.Fallback + "' in 'destinations' instead"
	}
	return ""
}

// destinationCountProblem refuses a router with too few or too many
// destinations, in the words that fit its mode.
func destinationCountProblem(rt policy.Router) string {
	n := len(rt.Destinations)
	switch {
	case n < 2 && rt.Decides():
		return "'destinations' needs at least two models; a router with one has no decision to " +
			"make and would charge a generation per request to reach a conclusion that " +
			"was already written down"
	case n < 2 && rt.Sizes():
		return "'destinations' needs at least two models; a size router with one places every " +
			"request on it whatever its size, which is that model's own alias with a " +
			"step in front of it"
	case n < 2:
		return "'destinations' needs at least two models; a " + string(rt.Mode) + " router with one " +
			"has nothing to choose between and nothing to fall back to, and is a slower " +
			"way of writing that model's own alias in the client"
	case n > policy.MaxDestinations && rt.Decides():
		return fmt.Sprintf("'destinations' names %d models; the limit is %d. The list is put to "+
			"a small model as a choice to make in one word, and a list longer than this "+
			"is one it answers by naming whichever end of it it read last",
			n, policy.MaxDestinations)
	case n > policy.MaxDestinations:
		return fmt.Sprintf("'destinations' names %d models; the limit is %d. Each one this "+
			"router tries is a wait the client is already spending, so a chain this long "+
			"is a request that times out rather than one that finds an answer",
			n, policy.MaxDestinations)
	}
	return ""
}

// checkRouterAlias refuses a router named like a catalogue model. A client
// names both in the same field and the catalogue wins, so such a router would
// never be reached.
func (s *Server) checkRouterAlias(w http.ResponseWriter, r *http.Request, alias string) bool {
	_, err := s.st.Model(r.Context(), alias)
	switch {
	case err == nil:
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "alias_in_use",
			"'"+alias+"' is already a model in this deployment's catalogue. A client names a "+
				"router in the same field it names a model, and the catalogue wins there, so "+
				"this router would never be reached - give it a different alias")
		return false
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return false
	}
	return true
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
			badRequest(w, "'ceilings' sets a ceiling on '"+alias+"', which is not one of this router's "+
				"destinations. A ceiling is the largest request a destination should be "+
				"given, so it has to name somewhere this router can send one")
			return false
		}
	}
	switch unbounded := rt.Unbounded(); len(unbounded) {
	case 1:
		return true
	case 0:
		badRequest(w, "every destination has a ceiling, so a request larger than all of them has "+
			"nowhere this router meant to send it. Leave the ceiling off the destination "+
			"that should take what the others will not - usually the largest model")
		return false
	default:
		badRequest(w, "'"+strings.Join(unbounded, "' and '")+"' both have no ceiling, and a router "+
			"needs exactly one destination that takes what the others will not. Whichever "+
			"of them comes first would answer every request that reaches it and the rest "+
			"would never be the first choice for anything")
		return false
	}
}

// checkRouterModel refuses a model that cannot make a decision.
func (s *Server) checkRouterModel(w http.ResponseWriter, r *http.Request, alias string) bool {
	if alias == "" {
		badRequest(w, "'model' is required; it names the model that makes the decision, which should be "+
			"a fast one served locally - its generation is added to every request that "+
			"names this router")
		return false
	}
	m, err := s.st.Model(r.Context(), alias)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found",
			"no model '"+alias+"' exists; a router decides with a model from the catalogue")
		return false
	case err != nil:
		s.fail(w, err)
		return false
	case m.Kind != policy.KindChat:
		badRequest(w, "'"+alias+"' is a "+string(m.Kind)+" model; a router reads text and answers with a "+
			"alias, which only a chat model does")
		return false
	}
	return true
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
		m, err := s.st.Model(r.Context(), alias)
		switch {
		case errors.Is(err, store.ErrNotFound):
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found",
				"no model '"+alias+"' exists; a router's destinations are models from the "+
					"catalogue")
			return false
		case err != nil:
			s.fail(w, err)
			return false
		case m.Kind != policy.KindChat:
			badRequest(w, "'"+alias+"' is a "+string(m.Kind)+" model; a router is reached on a chat "+
				"request, so everything it may choose between has to be able to answer one")
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

func (s *Server) deleteRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.routerAdmin(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")

	// Editors that name a deleted router are out of sight. What can be seen
	// are the scopes whose allow-list names it: they would be left able to
	// reach nothing, so the router is not deleted while they exist.
	users, err := s.st.RouterUsers(r.Context(), orgID, alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(users) > 0 {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "router_in_use",
			"the router '"+alias+"' is in the allow-list of "+scopeList(users)+
				". Every request naming it would be answered 'model not found', so take it "+
				"off those allow-lists first")
		return
	}

	if err := s.st.DeleteRouter(r.Context(), orgID, alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, orgID, "router", alias)
}

// checkRouter puts sample prompts through a router and shows where each went.
//
// A wrong router still answers every request, just from the wrong model. The
// likeliest mistake is an instruction that always picks the same destination,
// which costs a generation per request and routes nothing.
func (s *Server) checkRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.routerAdmin(w, r, p)
	if !ok || !s.requireGateway(w) {
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
