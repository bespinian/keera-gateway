package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// guardrailScope reads the scope a guardrail route names, finds the
// organisation and project above it, and checks that the caller may read it,
// or with admin change it. Another tenant's scope answers 404, not 403, so ids
// cannot be probed.
func (s *Server) guardrailScope(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	admin bool) (scope policy.ScopeType, scopeID, orgID, projectID string, ok bool) {
	scope, scopeID = policy.ScopeType(r.PathValue("scope")), r.PathValue("id")
	switch scope {
	case policy.ScopeOrg, policy.ScopeProject, policy.ScopeKey:
	default:
		badRequest(w, "scope must be one of org, project, key")
		return
	}
	orgID, projectID, err := s.scopeChain(r.Context(), scope, scopeID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if admin {
		ok = s.requireOwnerAdmin(w, p, orgID)
	} else {
		ok = s.requireOwnerRead(w, p, orgID)
	}
	return
}

// storedLimits reads one scope's guardrail. A scope with none has the empty
// guardrail.
func (s *Server) storedLimits(ctx context.Context, scope policy.ScopeType, id string) (policy.Limits, error) {
	lim, err := s.st.GetGuardrail(ctx, scope, id)
	if errors.Is(err, store.ErrNotFound) {
		return policy.Limits{}, nil
	}
	return lim, err
}

// chainLimits reads the organisation's guardrail and the project's. project is
// nil when there is no project.
func (s *Server) chainLimits(ctx context.Context, orgID, projectID string) (
	org policy.Limits, project *policy.Limits, err error,
) {
	if org, err = s.storedLimits(ctx, policy.ScopeOrg, orgID); err != nil || projectID == "" {
		return org, nil, err
	}
	lim, err := s.storedLimits(ctx, policy.ScopeProject, projectID)
	return org, &lim, err
}

// checkAllowedRepos cleans up allowed_repos and checks who may change it.
//
// On an organisation, only an operator may: one forge credential reaches
// every tenant's repositories, and this list keeps a tenant to its own. An
// administrator's write keeps what the operator set. A project only narrows it,
// so its administrators may set theirs. A key cannot set it at all.
func (s *Server) checkAllowedRepos(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	scope policy.ScopeType, scopeID string, lim *policy.Limits,
) bool {
	for i, e := range lim.AllowedRepos {
		e = strings.Trim(strings.TrimSpace(e), "/")
		if !policy.ValidRepoPattern(e) {
			badRequest(w, fmt.Sprintf("'allowed_repos' has %q; use an owner or group such as "+
				"'bankb', a repository such as 'bankb/core', or '*' for all", e))
			return false
		}
		lim.AllowedRepos[i] = e
	}
	if scope != policy.ScopeOrg || p.Unrestricted() {
		return true
	}
	stored, err := s.storedLimits(r.Context(), scope, scopeID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if lim.AllowedRepos != nil && !slices.Equal(lim.AllowedRepos, stored.AllowedRepos) {
		forbid(w, "only an operator can change which repositories an organisation's "+
			"sandboxes may check out; leave 'allowed_repos' out to keep it")
		return false
	}
	lim.AllowedRepos = stored.AllowedRepos
	return true
}

// maxSystemPromptBytes bounds a scope's standing instruction. It is added to
// every chat request the scope makes and charged each time, so a pasted
// handbook would quietly cost money on every request.
const maxSystemPromptBytes = 16 << 10

// checkSystemPrompt trims a scope's standing instruction in place and reports
// whether it fits, with the size a refusal has to quote.
//
// An emptied field becomes nil, not "", so a scope that says nothing never
// looks like one that says something.
func checkSystemPrompt(lim *policy.Limits) (size int, ok bool) {
	if lim.SystemPrompt == nil {
		return 0, true
	}
	trimmed := strings.TrimSpace(*lim.SystemPrompt)
	switch {
	case len(trimmed) > maxSystemPromptBytes:
		return len(trimmed), false
	case trimmed == "":
		lim.SystemPrompt = nil
	default:
		lim.SystemPrompt = &trimmed
	}
	return len(trimmed), true
}

func (s *Server) getGuardrails(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	scope, scopeID, _, _, ok := s.guardrailScope(w, r, p, false)
	if !ok {
		return
	}
	lim, err := s.storedLimits(r.Context(), scope, scopeID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, lim)
}

// EffectiveLevel is one level of the chain above a scope, with what that level
// sets on its own.
type EffectiveLevel struct {
	Type   policy.ScopeType `json:"type"`
	ID     string           `json:"id"`
	Name   string           `json:"name,omitempty"`
	Limits policy.Limits    `json:"limits"`
}

// Effective is what a request against a scope actually meets: every level's
// guardrails, and what they combine to. It answers "why is this key refused"
// without combining three rows by hand.
//
// Rate limits and budgets stay per level, because each level is enforced on
// its own and one merged number would be wrong at some level.
type Effective struct {
	Type   policy.ScopeType `json:"type"`
	ID     string           `json:"id"`
	Levels []EffectiveLevel `json:"levels"`
	// AllowedModels nil means every enabled model. An empty list means none.
	AllowedModels   []string `json:"allowed_models"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	SystemPrompt    string   `json:"system_prompt,omitempty"`
	Filters         []string `json:"filters,omitempty"`
	// AllowedTools nil means every tool of every MCP server.
	AllowedTools []string               `json:"allowed_tools"`
	Scopes       []policy.Scope         `json:"scopes"`
	Sandbox      policy.ResolvedSandbox `json:"sandbox"`
}

// effectiveGuardrails answers with the whole chain above a scope, combined the
// way the gateway combines it.
func (s *Server) effectiveGuardrails(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	scope, scopeID, orgID, projectID, ok := s.guardrailScope(w, r, p, false)
	if !ok {
		return
	}
	out, err := s.effective(r.Context(), scope, scopeID, orgID, projectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// scopeChain finds the organisation and project above a scope. The organisation
// is what an administrator must be in, so another tenant's guardrails cannot
// be reached by guessing an id.
func (s *Server) scopeChain(ctx context.Context, scope policy.ScopeType, scopeID string) (
	orgID, projectID string, err error,
) {
	switch scope {
	case policy.ScopeOrg:
		_, err := s.st.OrgByID(ctx, scopeID)
		return scopeID, "", err
	case policy.ScopeProject:
		orgID, err := s.st.ProjectOrg(ctx, scopeID)
		return orgID, scopeID, err
	default: // policy.ScopeKey; guardrailScope refuses any other.
		o, err := s.st.KeyOwnerOf(ctx, scopeID)
		return o.OrgID, o.ProjectID, err
	}
}

// effective reads every level above a scope and combines them. orgID and
// projectID are the scope's chain, from scopeChain.
func (s *Server) effective(ctx context.Context, scope policy.ScopeType,
	scopeID, orgID, projectID string) (Effective, error) {
	org, project, err := s.chainLimits(ctx, orgID, projectID)
	if err != nil {
		return Effective{}, err
	}
	// The key level exists only when a key was asked about.
	var own *policy.Limits
	key := policy.Key{OrgID: orgID, ProjectID: projectID}
	if scope == policy.ScopeKey {
		lim, err := s.storedLimits(ctx, policy.ScopeKey, scopeID)
		if err != nil {
			return Effective{}, err
		}
		own = &lim
		key.ID = scopeID
	}

	res := policy.Resolve(key, &org, project, own)
	// Resolve always ends with a key level, because a real request has a key.
	// Asked about an org or a project, there is no key, so that level is dropped.
	if scope != policy.ScopeKey && len(res.Scopes) > 0 {
		res.Scopes = res.Scopes[:len(res.Scopes)-1]
	}

	out := Effective{
		Type: scope, ID: scopeID,
		Levels:          []EffectiveLevel{s.effectiveLevel(ctx, policy.ScopeOrg, orgID, org)},
		AllowedModels:   res.AllowedModels,
		MaxOutputTokens: res.MaxOutputTokens,
		SystemPrompt:    res.SystemPrompt,
		Filters:         res.Filters,
		AllowedTools:    res.AllowedTools,
		Scopes:          res.Scopes,
		Sandbox:         res.Sandbox,
	}
	if project != nil {
		out.Levels = append(out.Levels, s.effectiveLevel(ctx, policy.ScopeProject, projectID, *project))
	}
	if own != nil {
		out.Levels = append(out.Levels, s.effectiveLevel(ctx, policy.ScopeKey, scopeID, *own))
	}
	return out, nil
}

// effectiveLevel names one level. A name that cannot be read is left out; the
// id is still there.
func (s *Server) effectiveLevel(ctx context.Context, t policy.ScopeType, id string,
	lim policy.Limits,
) EffectiveLevel {
	name, _ := s.st.ScopeName(ctx, t, id)
	return EffectiveLevel{Type: t, ID: id, Name: name, Limits: lim}
}

func (s *Server) putGuardrails(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	scope, scopeID, owner, _, ok := s.guardrailScope(w, r, p, true)
	if !ok {
		return
	}

	var lim policy.Limits
	if !readJSON(w, r, &lim) {
		return
	}
	if lim.BudgetPeriod != nil && !lim.BudgetPeriod.Valid() {
		badRequest(w, "'budget_period' must be 'day' or 'month'")
		return
	}
	// A sandbox's key is minted for that sandbox and dies with it, so a key's
	// guardrail has nothing to limit there. Stored, it would look like it did.
	if scope == policy.ScopeKey && !lim.SandboxLimits.IsZero() {
		badRequest(w, "sandbox limits are set on an organisation or a project, not on a key")
		return
	}
	if size, ok := checkSystemPrompt(&lim); !ok {
		badRequest(w, fmt.Sprintf("'system_prompt' is %d bytes; the limit is %d, because this text is "+
			"sent and charged on every request this scope makes", size, maxSystemPromptBytes))
		return
	}
	// Held from the checks below until the guardrail is stored, so nothing they
	// find is deleted in between.
	unlock, err := s.st.LockReferences(r.Context(), owner)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer unlock()
	if !s.checkGuardrailFilters(w, r, owner, &lim) || !s.checkAllowList(w, r, scope, owner, &lim) ||
		!s.checkAllowedTools(w, r, owner, &lim) || !s.checkAllowedRepos(w, r, p, scope, scopeID, &lim) {
		return
	}
	if lim.BudgetMicros != nil && lim.BudgetPeriod == nil {
		period := policy.PeriodMonth
		lim.BudgetPeriod = &period
	}
	for _, f := range []struct {
		name string
		v    *int
	}{
		{"max_output_tokens", lim.MaxOutputTokens}, {"rpm", lim.RPM}, {"tpm", lim.TPM},
	} {
		if f.v != nil && *f.v < 0 {
			badRequest(w, "'"+f.name+"' cannot be negative; omit it to set no limit here")
			return
		}
	}
	if lim.BudgetMicros != nil && *lim.BudgetMicros < 0 {
		badRequest(w, "'budget_micros' cannot be negative; omit it to set no limit here")
		return
	}
	if err := s.st.PutGuardrail(r.Context(), scope, scopeID, lim); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, owner, "guardrail.put", string(scope), scopeID, lim)
	s.changed(r, owner)
	httpx.WriteJSON(w, http.StatusOK, lim)
}
