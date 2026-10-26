package control

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// listRouters is readable by every member: developers type a router's alias
// into their editor, so they need to find it.
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

// routerReport is one router's own screen: how much went through it, which
// models answered, and how its route steps decided. Route step failures are
// quiet, because every request still gets an answer.
func (s *Server) routerReport(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	out, _, ok := hookReport(s, w, r, p, "router", s.st.RouterReportFor, s.st.Router)
	if !ok {
		return
	}
	if rtr, ok := out["router"].(policy.Router); ok {
		out["limits"] = s.limitUse(rtr, time.Now())
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// RouterLimit is one limit step of a router, and how much of it is used in
// its open window, whatever window the report covers.
type RouterLimit struct {
	Step   string        `json:"step"`
	Budget *AccessBudget `json:"budget,omitempty"`
	Rate   *AccessRate   `json:"rate,omitempty"`
}

// limitUse reads the gateway's own counts, which the limits are checked
// against. A rate limit per minute is left out: it refills as it goes, so
// what it used says little.
func (s *Server) limitUse(rtr policy.Router, now time.Time) []RouterLimit {
	out := []RouterLimit{}
	for step := range rtr.Steps() {
		switch {
		case step.Kind == policy.StepBudget:
			sc := rtr.Scopes(step)[0]
			out = append(out, RouterLimit{Step: step.ID, Budget: &AccessBudget{
				BudgetMicros: sc.BudgetMicros, Period: sc.Period,
				SpendMicros: s.reg.Budgets().Spent(sc.Type, sc.ID, sc.Period, now),
				ResetsAt:    sc.Period.Next(now),
			}})
		case step.Kind == policy.StepRateLimit && !step.RateLimit().PerMinute():
			rate := &AccessRate{Period: step.RateLimit().Per()}
			for _, sc := range rtr.Scopes(step) {
				used := s.reg.Budgets().Spent(sc.Type, sc.ID, sc.Period, now)
				if sc.Meter == policy.MeterRequests {
					rate.Requests, rate.RequestsUsed = sc.Quota, used
				} else {
					rate.Tokens, rate.TokensUsed = sc.Quota, used
				}
			}
			out = append(out, RouterLimit{Step: step.ID, Rate: rate})
		}
	}
	return out
}

type routerInput struct {
	DisplayName string       `json:"display_name"`
	Description string       `json:"description"`
	Start       *policy.Step `json:"start"`
}

// SavedRouter is what a PUT answers with: the router, what the
// organisation's allow-list now does with it, and likely mistakes. See
// policy.Router.Warnings.
type SavedRouter struct {
	policy.Router
	// Warning is about the allow-list, Warnings about the router itself.
	Warning  string   `json:"warning,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
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
	rtr := policy.Router{
		OrgID:       orgID,
		Alias:       r.PathValue("alias"),
		DisplayName: strings.TrimSpace(in.DisplayName),
		Description: strings.TrimSpace(in.Description),
		Start:       in.Start,
	}
	tidySteps(rtr)

	// Held until the router is saved, so nothing it names is deleted in
	// between.
	unlock, err := s.st.LockReferences(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer unlock()
	refs, err := s.routerRefs(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if msg := rtr.Problem(refs); msg != "" {
		badRequest(w, msg)
		return
	}
	// Checked on a step with one destination too, which decides as soon as
	// it has a second. See policy.Route.ReadsWith.
	for _, rt := range rtr.Routes() {
		if m := rt.ReadsWith(); m != "" && !s.checkReaderModel(w, r, orgID, m, routeReader, true) {
			return
		}
	}
	if !s.checkRouterAlias(w, r, orgID, rtr.Alias) {
		return
	}
	_, err = s.st.Router(r.Context(), orgID, rtr.Alias)
	found, ok := s.replacing(w, err)
	if !ok {
		return
	}

	saved, err := s.st.UpsertRouter(r.Context(), rtr)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "router.put", "router", rtr.Alias, saved)
	out := SavedRouter{Router: saved, Warnings: saved.Warnings(refs)}
	out.Warning, err = s.allowReaching(r, p, orgID, saved.Alias, saved.Reaches(), found)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.changed(r, orgID)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// allowReaching puts a new router on its organisation's allow-list, as a new
// model is, so it can be called once added. reaches is every model it may
// send to.
//
// Allowing a router allows every model it reaches. So a new router stays off
// a list that leaves out one of them, and a listed router that now sends to
// such a model opens it. Either way it says so.
func (s *Server) allowReaching(r *http.Request, p *authn.Principal, orgID, alias string,
	reaches []string, found bool,
) (string, error) {
	lim, err := s.storedLimits(r.Context(), policy.ScopeOrg, orgID)
	if err != nil || lim.AllowedModels == nil {
		return "", err
	}
	missing := ""
	for _, d := range reaches {
		if !slices.Contains(lim.AllowedModels, d) {
			missing = d
			break
		}
	}
	// A new one can be listed already: an administrator may name an alias
	// before it exists.
	listed := slices.Contains(lim.AllowedModels, alias)
	switch {
	case listed && missing != "":
		return "the organisation's allow-list names '" + alias + "', so every key that may " +
			"call it can now reach '" + missing + "', which the list leaves out. Add '" +
			missing + "' to the list, or take '" + alias + "' off it", nil
	case found || listed:
		return "", nil
	case missing != "":
		return "the organisation's allow-list leaves out '" + missing + "', so the router " +
			"was not put on it: allowing a router allows every model it sends to. No " +
			"key can call '" + alias + "' until the list names it", nil
	}
	return "", s.allowInOrg(r, p, orgID, alias)
}

// tidySteps trims what an administrator typed, and fills in default periods
// and modes, so the stored router says what it does.
func tidySteps(rtr policy.Router) {
	for s := range rtr.Steps() {
		s.ID = strings.TrimSpace(s.ID)
		s.Ref = strings.TrimSpace(s.Ref)
		s.Prompt = strings.TrimSpace(s.Prompt)
		s.Model = strings.TrimSpace(s.Model)
		s.Fallback = strings.TrimSpace(s.Fallback)
		// Header names are not case-sensitive, so the stored one is written
		// the way the gateway looks it up.
		if h := strings.TrimSpace(s.Header); h != "" {
			s.Header = http.CanonicalHeaderKey(h)
		}
		if s.Values != nil {
			values := make(map[string]string, len(s.Values))
			for value, dest := range s.Values {
				values[strings.TrimSpace(value)] = strings.TrimSpace(dest)
			}
			s.Values = values
		}
		switch {
		case s.Kind == policy.StepBudget:
			s.Period = s.Budget().Per()
		case s.Kind == policy.StepRateLimit:
			s.Period = s.RateLimit().Per()
		case s.Kind == policy.StepRoute && s.Mode == "":
			s.Mode = policy.RouteModeInstruction
		}
		for i, d := range s.Destinations {
			s.Destinations[i] = strings.TrimSpace(d)
		}
		// A ceiling of zero means no ceiling, so only positive ones are kept,
		// leaving one way to say it.
		maps.DeleteFunc(s.Ceilings, func(_ string, n int) bool { return n <= 0 })
		if len(s.Ceilings) == 0 {
			s.Ceilings = nil
		}
	}
}

func (s *Server) routerRefs(ctx context.Context, orgID string) (policy.RouterRefs, error) {
	refs := policy.RouterRefs{
		Models:  map[string]policy.Model{},
		Filters: map[string]policy.Filter{},
	}
	models, err := s.st.ListModels(ctx, orgID)
	if err != nil {
		return refs, err
	}
	for _, m := range models {
		refs.Models[m.Alias] = m
	}
	filters, err := s.st.ListFilters(ctx, orgID)
	if err != nil {
		return refs, err
	}
	for _, f := range filters {
		refs.Filters[f.Alias] = f
	}
	return refs, nil
}

// checkRouterAlias refuses a router named like a model. A client names both
// in the same field, and the model would win.
func (s *Server) checkRouterAlias(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	_, err := s.st.Model(r.Context(), orgID, alias)
	return !s.aliasTaken(w, err, "'"+alias+"' is already a model this organisation can call, "+
		"which a client names in the same field - give this router a different alias")
}

// checkFilterRouters refuses to delete a filter a router still uses, since
// every request through that step would be refused. The caller holds
// LockReferences.
func (s *Server) checkFilterRouters(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	routers, err := s.routersUsing(r.Context(), orgID, policy.StepFilter, alias)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if len(routers) == 0 {
		return true
	}
	users := make([]string, len(routers))
	for i, rtr := range routers {
		users[i] = "router '" + rtr + "'"
	}
	conflict(w, "filter_in_use", "the filter '"+alias+"' is used by "+strings.Join(users, ", ")+
		". Every request through them would be refused, so take it out of them first")
	return false
}

// routersUsing names, in order, the organisation's routers with a step that
// uses alias as kind. See policy.Router.Uses.
func (s *Server) routersUsing(ctx context.Context, orgID string, kind policy.StepKind,
	alias string,
) ([]string, error) {
	routers, err := s.st.ListRouters(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, rtr := range routers {
		if rtr.Uses(kind, alias) {
			users = append(users, rtr.Alias)
		}
	}
	slices.Sort(users)
	return users, nil
}

// deleteRouter keeps a router that a project's or key's allow-list still
// names: those scopes could reach nothing. The organisation's own list names
// every router allowed when it was added, so there the router is taken off
// with it, unless it is all the list names.
func (s *Server) deleteRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	s.deleteUnused(w, r, p, "router", s.st.RouterUsers, s.st.DeleteRouter,
		func(alias, users string) string {
			return "the router '" + alias + "' is in the allow-list of " + users +
				". Every request naming it would be answered 'model not found', so take it " +
				"off those allow-lists first"
		})
}

// RouterCheck is one probe per route step of a router.
type RouterCheck struct {
	Router string               `json:"router"`
	Routes []gateway.RouteProbe `json:"routes"`
}

// checkRouter puts sample prompts through a router's route steps and shows
// where each went, or asks a trying step's destinations whether they are up.
// ?step= checks one step only.
//
// A wrong route step still answers every request, just from the wrong model.
// The likeliest mistake is an instruction that always picks the same
// destination, which costs a generation per request and routes nothing.
func (s *Server) checkRouter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	rtr, err := s.st.Router(r.Context(), orgID, r.PathValue("alias"))
	if err != nil {
		s.fail(w, err)
		return
	}
	step := r.URL.Query().Get("step")
	out := RouterCheck{Router: rtr.Alias, Routes: []gateway.RouteProbe{}}
	for _, rt := range rtr.Routes() {
		if step == "" || rt.Step == step {
			out.Routes = append(out.Routes, s.opts.Gateway.CheckRoute(r.Context(), rt))
		}
	}
	if len(out.Routes) == 0 {
		msg := "the router '" + rtr.Alias + "' has no route step to check"
		if step != "" {
			msg = "the router '" + rtr.Alias + "' has no route step '" + step + "'"
		}
		badRequest(w, msg)
		return
	}
	errs := map[string]string{}
	for _, probe := range out.Routes {
		if probe.Error != "" {
			errs[probe.Step] = probe.Error
		}
	}
	s.auditf(r, p, orgID, "router.check", "router", rtr.Alias, map[string]any{
		"step": step, "errors": errs,
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}
