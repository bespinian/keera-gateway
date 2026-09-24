package control

import (
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
// goes with every request it covers, and a filter's model then rewrites the
// whole conversation, so it costs even more.
const maxFilterPromptBytes = 16 << 10

// filterOrg resolves which organisation a filter request is about. A filter
// belongs to one organisation, even though its model is the operator's.
func (s *Server) filterOrg(w http.ResponseWriter, r *http.Request, p *authn.Principal) (string, bool) {
	return s.requireOrg(w, p, r.URL.Query().Get("org_id"),
		"choose an organisation first; a filter belongs to one")
}

// filterAdmin is filterOrg for a request that changes a filter.
func (s *Server) filterAdmin(w http.ResponseWriter, r *http.Request, p *authn.Principal) (string, bool) {
	orgID, ok := s.filterOrg(w, r, p)
	return orgID, ok && s.requireOrgAdmin(w, p, orgID)
}

// listFilters is readable by every member of the organisation, instruction
// and all. It is text added to the reader's own requests, not a credential.
func (s *Server) listFilters(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.filterOrg(w, r, p)
	if !ok {
		return
	}
	filters, err := s.st.ListFilters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeHookList(w, r, filters, func(from, to time.Time) (any, error) {
		return s.st.FilterStats(r.Context(), orgID, from, to)
	})
}

// filterReport is one filter's own screen: how often it fires, what it does,
// what it adds to latency and cost, and which teams it refuses.
//
// Every member may read it: these are counts of the organisation's own
// traffic, which members already see on the Overview.
func (s *Server) filterReport(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.filterOrg(w, r, p)
	if !ok {
		return
	}
	from, to, ok := queryWindow(w, r.URL.Query())
	if !ok {
		return
	}
	alias := r.PathValue("alias")

	report, err := s.st.FilterReportFor(r.Context(), orgID, alias, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	names, err := s.st.TeamNames(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{
		"report": report, "team_names": names, "currency": s.opts.Currency,
	}
	// A deleted filter keeps its traffic, so the report is served either way
	// and "filter" is left out rather than answering 404.
	f, err := s.st.Filter(r.Context(), orgID, alias)
	switch {
	case err == nil:
		out["filter"] = f
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) putFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.filterAdmin(w, r, p)
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
	if err := httpx.ReadJSON(r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}

	f := policy.Filter{
		OrgID: orgID,
		Alias: r.PathValue("alias"),
		Model: strings.TrimSpace(in.Model),
		Mode:  policy.FilterMode(strings.TrimSpace(string(in.Mode))),
		// A missing shadow field means enforce. A filter that silently stopped
		// enforcing because a client left a field out is a mistake nothing
		// else would report.
		Shadow:      in.Shadow,
		Prompt:      strings.TrimSpace(in.Prompt),
		Rules:       in.Rules,
		Description: strings.TrimSpace(in.Description),
	}
	// A missing mode means rewrite, the only mode older clients know.
	if f.Mode == "" {
		f.Mode = policy.FilterModeRewrite
	}
	if !policy.ValidFilterAlias(f.Alias) {
		badRequest(w, "a filter's alias is written into guardrails, command lines and URLs, so it is "+
			"lowercase letters, digits and inner hyphens - 'redact-secrets', not "+
			"'"+f.Alias+"'")
		return
	}
	if !policy.ValidFilterMode(f.Mode) {
		badRequest(w, "'mode' is 'rewrite' - the filter is shown the request's text and answers with "+
			"it rewritten - or 'gate', where it answers only whether the request may "+
			"go at all, or 'pattern', where a list of rules is applied to the text with "+
			"no model at all. Not '"+string(f.Mode)+"'")
		return
	}
	if !s.checkFilterFields(w, r, orgID, f) {
		return
	}

	saved, err := s.st.UpsertFilter(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, orgID, "filter.put", "filter", f.Alias, saved)
	s.changed(r)
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
		badRequest(w, "'rules' is set, and this is a "+string(f.Mode)+" filter: it reads the "+
			"request with a model and an instruction, so there is nothing for a "+
			"list of expressions to be applied by. Use 'mode': 'pattern' for one "+
			"that runs rules instead")
		return false
	case f.Prompt == "":
		badRequest(w, "'prompt' is required; it is the instruction the filter's model is given, "+
			"and a filter without one would read every request the guardrail "+
			"covers with nothing to judge it against")
		return false
	case len(f.Prompt) > maxFilterPromptBytes:
		badRequest(w, fmt.Sprintf("'prompt' is %d bytes; the limit is %d, because this text is "+
			"sent to the filter's model on every request the guardrail covers",
			len(f.Prompt), maxFilterPromptBytes))
		return false
	}
	return s.checkFilterModel(w, r, orgID, f.Model)
}

// checkFilterRules refuses a pattern filter that could not be applied.
//
// Every rule is compiled now, not on first use: a filter fails closed, so a
// typo in a rule would refuse every request it covers.
func checkFilterRules(w http.ResponseWriter, f policy.Filter) bool {
	switch {
	case f.Model != "":
		badRequest(w, "'model' is set, and this is a pattern filter: it applies its rules in the "+
			"gateway and runs no model at all, which is the whole of what the mode is "+
			"for. Leave it out, or use 'mode': 'rewrite' for a filter that reads the "+
			"request with a model")
		return false
	case f.Prompt != "":
		badRequest(w, "'prompt' is set, and this is a pattern filter: nothing reads prose, so there "+
			"is nothing an instruction would be given to. What it applies is 'rules'")
		return false
	case len(f.Rules) == 0:
		badRequest(w, "'rules' is required on a pattern filter; one with none would read every "+
			"request the guardrail covers and change nothing about any of them, which "+
			"is not a filter that passes everything - it is a filter somebody thinks "+
			"is working")
		return false
	case len(f.Rules) > policy.MaxFilterRules:
		badRequest(w, fmt.Sprintf("'rules' has %d rules; the limit is %d. Every one of them is "+
			"matched against every segment of every request the guardrail covers, and "+
			"a list this long is also past the point where anybody can say what the "+
			"filter does by reading it", len(f.Rules), policy.MaxFilterRules))
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

// checkFilterModel refuses a model that cannot serve as a filter, rather than
// storing a guardrail that will refuse every request it covers.
func (s *Server) checkFilterModel(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	if alias == "" {
		badRequest(w, "'model' is required; it names the model the filter runs on")
		return false
	}
	m, err := s.st.Model(r.Context(), alias)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found",
			"no model '"+alias+"' exists; a filter runs on a model from the catalogue")
		return false
	case err != nil:
		s.fail(w, err)
		return false
	case m.Kind != policy.KindChat:
		badRequest(w, "'"+alias+"' is a "+string(m.Kind)+" model; a filter reads text and answers in "+
			"words, which only a chat model does")
		return false
	}
	allowed, err := s.orgAllowsModel(r.Context(), orgID, alias)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if !allowed {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "model_not_allowed",
			filterModelRefusal(alias, m))
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

// filterModelRefusal says why a filter may not run on a model its organisation
// does not allow: the filter sees each request before redaction, so it would
// send them there in full.
func filterModelRefusal(alias string, m policy.Model) string {
	msg := "this organisation's allow-list does not include '" + alias + "'"
	if m.Hosting() == policy.HostedExternal {
		msg += ", which is hosted at " + m.Endpoint()
	}
	return msg + ". A filter reads a request before anything is redacted from it, so one " +
		"running on a model the organisation does not allow would send every request it " +
		"covers there in full. Allow the model for this organisation, or name one it already " +
		"allows"
}

func (s *Server) deleteFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.filterAdmin(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")

	// A guardrail naming a missing filter refuses every request it covers. So
	// a filter still in use is not deleted; the refusal lists who uses it.
	users, err := s.st.FilterUsers(r.Context(), orgID, alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(users) > 0 {
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "filter_in_use",
			"the filter '"+alias+"' is still applied by "+scopeList(users)+
				". Every request those guardrails cover would be refused rather than sent "+
				"unfiltered, so take it off them first")
		return
	}

	if err := s.st.DeleteFilter(r.Context(), orgID, alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, orgID, "filter", alias)
}

// checkFilter runs one filter over a sample and shows what came back.
//
// A filter model too weak for the format refuses everything, and an eager
// instruction deletes code. Both show up here in one click, instead of when a
// developer complains.
func (s *Server) checkFilter(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.filterAdmin(w, r, p)
	if !ok || !s.requireGateway(w) {
		return
	}
	f, err := s.st.Filter(r.Context(), orgID, r.PathValue("alias"))
	if err != nil {
		s.fail(w, err)
		return
	}
	// A pattern filter runs no model, so there is nothing to look up.
	var m policy.Model
	if f.UsesModel() {
		// The registry, not the store: it holds the decrypted credential the
		// data plane would present.
		live, found := s.reg.Model(f.Model)
		if !found {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"alias": f.Alias, "model": f.Model, "ok": false,
				"error": "the model '" + f.Model + "' is not in the catalogue any more, so this " +
					"filter would refuse every request it covers",
			})
			return
		}
		m = live
	}
	probe := s.opts.Gateway.CheckFilter(r.Context(), f, m)
	s.auditf(r, p, orgID, "filter.check", "filter", f.Alias, map[string]any{
		"ok": probe.OK, "error": probe.Error,
	})
	httpx.WriteJSON(w, http.StatusOK, probe)
}

// checkAllowList refuses an organisation allow-list that would leave one of
// its filters on a model outside it. It is the other half of
// checkFilterModel, since the two writes can come in either order.
//
// Only the organisation scope is checked: a filter belongs to the
// organisation, and a team's or key's list narrows only that key.
func (s *Server) checkAllowList(w http.ResponseWriter, r *http.Request,
	scope policy.ScopeType, orgID string, lim *policy.Limits) bool {
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
		if f.UsesModel() && !slices.Contains(lim.AllowedModels, f.Model) {
			stranded = append(stranded, "'"+f.Alias+"' (on "+f.Model+")")
		}
	}
	if len(stranded) == 0 {
		return true
	}
	httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "filter_model_not_allowed",
		"this allow-list leaves "+strings.Join(stranded, ", ")+" running on a model it does "+
			"not include. A filter reads a request before anything is redacted from it, so "+
			"those requests would still go there in full - point the filters somewhere this "+
			"list allows first, or keep the model on it")
	return false
}

// checkGuardrailFilters refuses a guardrail that names a filter its
// organisation does not have, and cleans up the list in place.
//
// At request time an unknown filter refuses everything, so a typo here would
// take a department offline. It is caught now, while the writer can see it.
func (s *Server) checkGuardrailFilters(w http.ResponseWriter, r *http.Request,
	orgID string, lim *policy.Limits) bool {
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
