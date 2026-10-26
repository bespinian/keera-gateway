package control

import (
	"cmp"
	"context"
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

// maxFilterPromptBytes bounds a filter's instruction. Like a system prompt it
// goes with every request it covers, and the filter's model then rewrites the
// whole conversation.
const maxFilterPromptBytes = 16 << 10

// listFilters is readable by every member, instruction and all: it is text
// added to their own requests, not a credential.
func (s *Server) listFilters(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	filters, err := s.st.ListFilters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	aliases := make([]string, len(filters))
	for i, f := range filters {
		aliases[i] = f.Alias
	}
	s.writeHookList(w, r, filters, func(from, to time.Time) (any, error) {
		return s.st.FilterStats(r.Context(), orgID, aliases, from, to)
	})
}

// filterReport is one filter's own screen: how often it fires, what it does,
// its latency and cost, and which projects it refuses.
//
// Every member may read it: members already see these counts on the Dashboard.
func (s *Server) filterReport(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	out, orgID, ok := hookReport(s, w, r, p, "filter", s.st.FilterReportFor, s.st.Filter)
	if !ok {
		return
	}
	names, err := s.st.ProjectNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out["project_names"] = names
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) putFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}

	var in struct {
		Model       string              `json:"model"`
		Mode        policy.FilterMode   `json:"mode"`
		Shadow      bool                `json:"shadow"`
		Prompt      string              `json:"prompt"`
		Rules       []policy.FilterRule `json:"rules"`
		Description string              `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}

	f := policy.Filter{
		OrgID: orgID,
		Alias: r.PathValue("alias"),
		Model: strings.TrimSpace(in.Model),
		Mode:  policy.FilterMode(strings.TrimSpace(string(in.Mode))),
		// A missing shadow field means enforce: a filter that silently stopped
		// enforcing is a mistake nothing else would report.
		Shadow:      in.Shadow,
		Prompt:      strings.TrimSpace(in.Prompt),
		Rules:       in.Rules,
		Description: strings.TrimSpace(in.Description),
	}
	if !policy.ValidAlias(f.Alias) {
		badRequest(w, badAlias(f.Alias))
		return
	}
	if !f.Mode.Valid() {
		badRequest(w, "'mode' must be rewrite, gate or pattern, not '"+string(f.Mode)+"'")
		return
	}
	// Held until the filter is saved, so its model is not deleted in between.
	unlock, err := s.st.LockReferences(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer unlock()
	if !s.checkFilterFields(w, r, orgID, f) {
		return
	}

	saved, err := s.st.UpsertFilter(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "filter.put", "filter", f.Alias, saved)
	s.changed(r, orgID)
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// checkFilterFields refuses a filter whose fields do not fit its mode. A model
// left over from a mode change would mislead whoever reads the filter later.
func (s *Server) checkFilterFields(w http.ResponseWriter, r *http.Request,
	orgID string, f policy.Filter,
) bool {
	if !f.UsesModel() {
		return checkFilterRules(w, f)
	}
	switch {
	case len(f.Rules) > 0:
		// The store fills in a missing mode later, so it is named here.
		mode := cmp.Or(f.Mode, policy.FilterModeRewrite)
		badRequest(w, "'rules' only works with 'mode': 'pattern'; a "+string(mode)+
			" filter reads the request with a model and a prompt instead")
		return false
	case f.Model == "":
		badRequest(w, "'model' is required; it names the model the filter runs on")
		return false
	case f.Prompt == "":
		badRequest(w, "'prompt' is required: it tells the filter's model what to look for")
		return false
	case len(f.Prompt) > maxFilterPromptBytes:
		badRequest(w, fmt.Sprintf("'prompt' is %d bytes; the limit is %d, since it is sent with every request",
			len(f.Prompt), maxFilterPromptBytes))
		return false
	}
	return s.checkReaderModel(w, r, orgID, f.Model, filterReader, f.Mode.Gates())
}

// checkFilterRules refuses a pattern filter that could not be applied.
//
// Every rule is compiled now, not on first use: a filter fails closed, so a
// typo in a rule would refuse every request it covers.
func checkFilterRules(w http.ResponseWriter, f policy.Filter) bool {
	switch {
	case f.Model != "":
		badRequest(w, "'model' must be empty on a pattern filter, which runs no model; "+
			"use 'mode': 'rewrite' for one that does")
		return false
	case f.Prompt != "":
		badRequest(w, "'prompt' must be empty on a pattern filter, which applies 'rules' instead")
		return false
	case len(f.Rules) == 0:
		// One with none would change nothing, yet look like it works.
		badRequest(w, "'rules' needs at least one rule on a pattern filter")
		return false
	case len(f.Rules) > policy.MaxFilterRules:
		badRequest(w, fmt.Sprintf("'rules' has %d rules; the limit is %d, since each one is "+
			"matched against every request", len(f.Rules), policy.MaxFilterRules))
		return false
	}
	for _, rule := range f.Rules {
		if err := rule.Valid(); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_rule",
				err.Error())
			return false
		}
	}
	return true
}

// reader describes what runs on a model: a filter or a route step. Only the
// words differ.
type reader struct {
	name string
	// uses and answers end the refusals for a missing model and a model of
	// the wrong kind.
	uses, answers string
}

var (
	filterReader = reader{
		name:    "filter",
		uses:    "a filter runs on one of its models",
		answers: "answers in words",
	}
	routeReader = reader{
		name:    "route step",
		uses:    "a route step decides with one of its models",
		answers: "answers with an alias",
	}
)

// checkReaderModel refuses a model that cannot serve as a filter or route
// step, which would refuse every request it covers. choice says it is only
// asked to choose, which a decision model can also do.
func (s *Server) checkReaderModel(w http.ResponseWriter, r *http.Request, orgID, alias string,
	rd reader, choice bool,
) bool {
	m, err := s.st.Model(r.Context(), orgID, alias)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found",
			"this organisation has no model '"+alias+"'; "+rd.uses)
		return false
	case err != nil:
		s.fail(w, err)
		return false
	case m.Kind == policy.KindDecision && !choice:
		badRequest(w, "'"+alias+"' is a decision model: it answers a question and writes no "+
			"text, so it cannot rewrite a request. Use it for a gate")
		return false
	case m.Kind != policy.KindChat && m.Kind != policy.KindDecision:
		which := "a chat model"
		if choice {
			which = "a chat or a decision model"
		}
		badRequest(w, "'"+alias+"' is a "+string(m.Kind)+" model; a "+rd.name+" reads text and "+
			rd.answers+", which only "+which+" does")
		return false
	case m.Subscription:
		badRequest(w, subscriptionReader(alias, rd.name))
		return false
	}
	allowed, err := s.orgAllowsModel(r.Context(), orgID, alias)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if !allowed {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_allowed",
			readerModelRefusal(rd.name, alias, m))
		return false
	}
	return true
}

// orgAllowsModel reports whether an organisation's allow-list permits a
// model. No list at all allows every model.
func (s *Server) orgAllowsModel(ctx context.Context, orgID, alias string) (bool, error) {
	lim, err := s.storedLimits(ctx, policy.ScopeOrg, orgID)
	if err != nil {
		return false, err
	}
	return lim.AllowedModels == nil || slices.Contains(lim.AllowedModels, alias), nil
}

// readerModelRefusal says why a filter or route step may not run on a model
// the organisation does not allow: both read requests before redaction.
func readerModelRefusal(what, alias string, m policy.Model) string {
	msg := "this organisation's allow-list does not include '" + alias + "'"
	if m.Hosting() == policy.HostedExternal {
		msg += ", which is hosted at " + m.Endpoint()
	}
	return msg + ". A " + what + " reads a request before anything is redacted from it, so one " +
		"running on a model the organisation does not allow would send every request it " +
		"covers there in full. Allow the model for this organisation, or name one it already " +
		"allows"
}

// deleteFilter keeps a filter that is still in use: a guardrail naming a
// missing filter refuses every request it covers.
func (s *Server) deleteFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	s.deleteUnused(w, r, p, "filter", s.st.FilterUsers, s.st.DeleteFilter, func(alias, users string) string {
		return "the filter '" + alias + "' is still applied by " + users +
			". Every request those guardrails cover would be refused rather than sent " +
			"unfiltered, so take it off them first"
	})
}

// checkFilter runs one filter over a sample and shows what came back.
//
// A filter model too weak for the format refuses everything, and an eager
// instruction deletes code. Both show up here, not when a developer complains.
func (s *Server) checkFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	f, err := s.st.Filter(r.Context(), orgID, r.PathValue("alias"))
	if err != nil {
		s.fail(w, err)
		return
	}
	probe := s.opts.Gateway.CheckFilter(r.Context(), f)
	s.auditf(r, p, orgID, "filter.check", "filter", f.Alias, map[string]any{
		"ok": probe.OK, "error": probe.Error,
	})
	httpx.WriteJSON(w, http.StatusOK, probe)
}

// checkAllowList refuses an organisation allow-list that would leave a filter
// or deciding route step on a model outside it. It is the other half of
// checkReaderModel, since the writes can come in either order.
//
// Only the organisation scope is checked: filters and routers belong to the
// organisation, and a project's or key's list narrows only that key.
func (s *Server) checkAllowList(w http.ResponseWriter, r *http.Request,
	scope policy.ScopeType, orgID string, lim *policy.Limits,
) bool {
	if scope != policy.ScopeOrg || lim.AllowedModels == nil {
		return true
	}
	filters, err := s.st.ListFilters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	var stranded []string
	for _, f := range filters {
		// A pattern filter runs no model, so no allow-list can strand it.
		if m := f.ReadsWith(); m != "" && !slices.Contains(lim.AllowedModels, m) {
			stranded = append(stranded, "'"+f.Alias+"' (on "+f.Model+")")
		}
	}
	if len(stranded) > 0 {
		strandedRefusal(w, "filter_model_not_allowed", "filter", stranded)
		return false
	}
	routers, err := s.st.ListRouters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	for _, rtr := range routers {
		for _, rt := range rtr.Routes() {
			if m := rt.ReadsWith(); m != "" && !slices.Contains(lim.AllowedModels, m) {
				stranded = append(stranded, "'"+rtr.Alias+"' (on "+rt.Model+")")
			}
		}
	}
	if len(stranded) > 0 {
		strandedRefusal(w, "router_model_not_allowed", "router", stranded)
		return false
	}
	return true
}

func strandedRefusal(w http.ResponseWriter, code, what string, stranded []string) {
	conflict(w, code,
		"this allow-list leaves "+strings.Join(stranded, ", ")+" running on a model it does "+
			"not include. A "+what+" reads a request before anything is redacted from it, so "+
			"those requests would still go there in full - point the "+what+"s somewhere this "+
			"list allows first, or keep the model on it")
}

// checkGuardrailFilters refuses a guardrail that names an unknown filter, and
// cleans up the list in place.
//
// At request time an unknown filter refuses everything, so a typo would take a
// department offline.
func (s *Server) checkGuardrailFilters(w http.ResponseWriter, r *http.Request,
	orgID string, lim *policy.Limits,
) bool {
	if len(lim.Filters) == 0 {
		lim.Filters = nil
		return true
	}
	have, err := s.st.ListFilters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	known := make(map[string]bool, len(have))
	for _, f := range have {
		known[f.Alias] = true
	}
	kept := make([]string, 0, len(lim.Filters))
	for _, alias := range lim.Filters {
		alias = strings.TrimSpace(alias)
		switch {
		case alias == "":
			continue
		case !known[alias]:
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "filter_not_found",
				"this organisation has no filter called '"+alias+"'; a guardrail naming one "+
					"would refuse every request it covers rather than send them unfiltered")
			return false
		}
		// A filter named twice runs once: a second pass finds nothing new.
		if !slices.Contains(kept, alias) {
			kept = append(kept, alias)
		}
	}
	lim.Filters = kept
	return true
}

// subscriptionReader refuses a subscription model under a filter or route
// step. Only a caller's own sign-in reaches it, and that is for the caller's
// request alone.
func subscriptionReader(alias, what string) string {
	return "'" + alias + "' is paid by each person's own Claude subscription, so the gateway " +
		"cannot run a " + what + " on it; choose a model the organisation pays for"
}
