package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// requireScopeRead checks that the caller may read the tenant owning a policy
// scope. Another tenant's scope answers 404, not 403, so ids cannot be probed.
func (s *Server) requireScopeRead(w http.ResponseWriter, r *http.Request,
	p *authn.Principal, scope policy.ScopeType, scopeID string) bool {
	if p.Unrestricted() {
		return true
	}
	owner, _, err := s.scopeChain(r.Context(), scope, scopeID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if !p.CanReadOrg(owner) {
		s.fail(w, store.ErrNotFound)
		return false
	}
	return true
}

func (s *Server) policyScope(w http.ResponseWriter, r *http.Request) (policy.ScopeType, string, bool) {
	scope := policy.ScopeType(r.PathValue("scope"))
	switch scope {
	case policy.ScopeOrg, policy.ScopeTeam, policy.ScopeKey:
	default:
		badRequest(w, "scope must be one of org, team, key")
		return "", "", false
	}
	return scope, r.PathValue("id"), true
}

// storedLimits reads one scope's guardrail. A scope with none has the empty
// guardrail.
func (s *Server) storedLimits(ctx context.Context, scope policy.ScopeType, id string) (policy.Limits, error) {
	lim, err := s.st.GetPolicy(ctx, scope, id)
	if errors.Is(err, store.ErrNotFound) {
		return policy.Limits{}, nil
	}
	return lim, err
}

// checkAllowedRepos cleans up allowed_repos and checks who may change it.
//
// On an organisation, only an operator may: one forge credential reaches
// every tenant's repositories, and this list keeps a tenant to its own. An
// administrator's write keeps what the operator set. Teams and keys only
// narrow it, so their administrators may set theirs.
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
		s.forbid(w, "only an operator can change which repositories an organisation's "+
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
	scope, scopeID, ok := s.policyScope(w, r)
	if !ok || !s.requireScopeRead(w, r, p, scope, scopeID) {
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
	Scope  policy.ScopeType `json:"scope"`
	ID     string           `json:"id"`
	Levels []EffectiveLevel `json:"levels"`
	// AllowedModels nil means every enabled model. An empty list means none.
	AllowedModels   []string `json:"allowed_models"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	SystemPrompt    string   `json:"system_prompt,omitempty"`
	Filters         []string `json:"filters,omitempty"`
	// AllowedTools nil means every tool of every MCP server.
	AllowedTools     []string               `json:"allowed_tools"`
	BlockHostedTools bool                   `json:"block_hosted_tools,omitempty"`
	Scopes           []policy.Scope         `json:"scopes"`
	Sandbox          policy.ResolvedSandbox `json:"sandbox"`
}

// effectiveGuardrails answers with the whole chain above a scope, combined the
// way the gateway combines it.
func (s *Server) effectiveGuardrails(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	scope, scopeID, ok := s.policyScope(w, r)
	if !ok || !s.requireScopeRead(w, r, p, scope, scopeID) {
		return
	}
	out, err := s.effective(r.Context(), scope, scopeID)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// scopeChain finds the organisation and team above a scope. The organisation
// is what an administrator must be in, so another tenant's guardrails cannot
// be reached by guessing an id.
func (s *Server) scopeChain(ctx context.Context, scope policy.ScopeType, scopeID string) (
	orgID, teamID string, err error,
) {
	switch scope {
	case policy.ScopeOrg:
		exists, err := s.st.OrgExists(ctx, scopeID)
		if err == nil && !exists {
			err = store.ErrNotFound
		}
		return scopeID, "", err
	case policy.ScopeTeam:
		orgID, err := s.st.TeamOrg(ctx, scopeID)
		return orgID, scopeID, err
	case policy.ScopeKey:
		return s.st.KeyScope(ctx, scopeID)
	default:
		return "", "", errors.New("scope must be one of org, team, key")
	}
}

// effective reads every level above a scope and combines them.
func (s *Server) effective(ctx context.Context, scope policy.ScopeType, scopeID string) (Effective, error) {
	orgID, teamID, err := s.scopeChain(ctx, scope, scopeID)
	if err != nil {
		return Effective{}, err
	}
	org, err := s.storedLimits(ctx, policy.ScopeOrg, orgID)
	if err != nil {
		return Effective{}, err
	}
	// The team level exists only when there is a team, and the key level only
	// when a key was asked about.
	var team, own *policy.Limits
	if teamID != "" {
		lim, err := s.storedLimits(ctx, policy.ScopeTeam, teamID)
		if err != nil {
			return Effective{}, err
		}
		team = &lim
	}
	key := policy.Key{OrgID: orgID, TeamID: teamID}
	if scope == policy.ScopeKey {
		lim, err := s.storedLimits(ctx, policy.ScopeKey, scopeID)
		if err != nil {
			return Effective{}, err
		}
		own = &lim
		key.ID = scopeID
	}

	res := policy.Resolve(key, &org, team, own)
	// Resolve always ends with a key level, because a real request has a key.
	// Asked about an org or a team, there is no key, so that level is dropped.
	if scope != policy.ScopeKey && len(res.Scopes) > 0 {
		res.Scopes = res.Scopes[:len(res.Scopes)-1]
	}

	out := Effective{
		Scope: scope, ID: scopeID,
		Levels:           []EffectiveLevel{s.effectiveLevel(ctx, policy.ScopeOrg, orgID, org)},
		AllowedModels:    res.AllowedModels,
		MaxOutputTokens:  res.MaxOutputTokens,
		SystemPrompt:     res.SystemPrompt,
		Filters:          res.Filters,
		AllowedTools:     res.AllowedTools,
		BlockHostedTools: res.BlockHostedTools,
		Scopes:           res.Scopes,
		Sandbox:          res.Sandbox,
	}
	if team != nil {
		out.Levels = append(out.Levels, s.effectiveLevel(ctx, policy.ScopeTeam, teamID, *team))
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
	name, err := s.st.ScopeName(ctx, t, id)
	if err != nil {
		name = ""
	}
	return EffectiveLevel{Type: t, ID: id, Name: name, Limits: lim}
}

func (s *Server) putGuardrails(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	scope, scopeID, ok := s.policyScope(w, r)
	if !ok {
		return
	}
	owner, _, err := s.scopeChain(r.Context(), scope, scopeID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.requireOwnerAdmin(w, p, owner) {
		return
	}

	var lim policy.Limits
	if err := httpx.ReadJSON(r, &lim); err != nil {
		badRequest(w, err.Error())
		return
	}
	if lim.BudgetPeriod != nil && !lim.BudgetPeriod.Valid() {
		badRequest(w, "'budget_period' must be 'day' or 'month'")
		return
	}
	// A sandbox's key is minted for that sandbox and dies with it, so a key's
	// guardrail has nothing to limit there. Stored, it would look like it did.
	if scope == policy.ScopeKey && !lim.SandboxLimits.IsZero() {
		badRequest(w, "sandbox limits are set on an organisation or a team, not on a key")
		return
	}
	if size, ok := checkSystemPrompt(&lim); !ok {
		badRequest(w, fmt.Sprintf("'system_prompt' is %d bytes; the limit is %d, because this text is "+
			"sent and charged on every request this scope makes", size, maxSystemPromptBytes))
		return
	}
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
			badRequest(w, "'"+f.name+"' cannot be negative; omit it for unlimited")
			return
		}
	}
	if lim.BudgetMicros != nil && *lim.BudgetMicros < 0 {
		badRequest(w, "'budget_micros' cannot be negative; omit it for unlimited")
		return
	}
	if err := s.st.PutPolicy(r.Context(), scope, scopeID, lim); err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, owner, "guardrail.put", string(scope), scopeID, lim)
	s.changed(r)
	httpx.WriteJSON(w, http.StatusOK, lim)
}

// -------------------------------------------------------------------- models

// listModels is readable by anyone signed in: a developer needs to know which
// models exist and their limits.
//
// Backend URLs and whether a key is stored are for the organisation's
// administrators.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	models, err := s.st.ListModels(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	admin := p.CanAdminOrg(orgID)
	for i := range models {
		if !admin {
			models[i].Backends = nil
			models[i].HasAPIKey = false
		}
		// Already left out of JSON by its tag; cleared as well to be safe.
		models[i].APIKeyCiphertext = nil
	}
	s.writeHookList(w, r, models, func(from, to time.Time) (any, error) {
		return s.st.ModelStats(r.Context(), orgID, from, to)
	})
}

// listProviders serves the hosted providers this binary knows, so adding a
// hosted model in the panel needs only a model id. It is the same table the
// catalogue file is read against.
//
// Anyone signed in may read it, for the panel's model catalog. Only someone who
// can add a model sees where a provider is reached: the form fills it in.
func (s *Server) listProviders(w http.ResponseWriter, _ *http.Request, p *authn.Principal) {
	providers := catalog.Providers()
	if !p.CanAdminOrg(p.OrgID) {
		for i := range providers {
			providers[i].Endpoint = ""
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": providers})
}

func (s *Server) putModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	// The credential is write-only, so it is part of the request and not of
	// the model. Nil leaves the stored one alone.
	var body struct {
		policy.Model
		APIKey *string `json:"api_key"`
	}
	if err := httpx.ReadJSON(r, &body); err != nil {
		badRequest(w, err.Error())
		return
	}
	m := body.Model
	m.Alias = r.PathValue("alias")
	m.OrgID = orgID
	if !policy.ValidAlias(m.Alias) {
		badRequest(w, "an alias must be lowercase letters, digits and interior hyphens: it is rendered "+
			"into the client configurations `keera connect` prints, which quote none of it")
		return
	}
	credential := ""
	if body.APIKey != nil {
		credential = strings.TrimSpace(*body.APIKey)
	}
	if msg := normalizeModel(&m); msg != "" {
		badRequest(w, msg)
		return
	}
	if !s.checkModelAlias(w, r, orgID, m.Alias) {
		return
	}
	if err := s.st.UpsertModel(r.Context(), m); err != nil {
		s.fail(w, err)
		return
	}
	// The model marshals without its credential.
	s.auditf(r, p, orgID, "model.put", "model", m.Alias, m)

	if body.APIKey != nil {
		if err := s.setModelCredential(r, p, orgID, m.Alias, credential); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.changed(r)
	// Read back, so has_api_key says what is stored and not what this body
	// happened to mention.
	stored, err := s.st.Model(r.Context(), orgID, m.Alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, stored)
}

// normalizeModel fills in a model's defaults and says what is wrong with it,
// or "" when nothing is.
func normalizeModel(m *policy.Model) string {
	switch {
	case len(m.Backends) == 0:
		return "at least one backend is required"
	case m.BackendModel == "":
		return "'backend_model' is required - it is the name the inference plane serves, " +
			"which for vLLM is --served-model-name"
	}
	if m.Kind == "" {
		m.Kind = policy.KindChat
	}
	if !m.Kind.Valid() {
		return "'kind' must be chat, completion or embedding"
	}
	// The provider must be one this build knows, because the gateway reads
	// it to choose how to talk to the backend. Its defaults are not applied
	// here: the caller sends every value, and may have changed any of them.
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	provider, known := catalog.ProviderByName(m.Provider)
	if m.Provider != "" && !known {
		return "unknown provider " + strconv.Quote(m.Provider) + "; Keera Gateway knows: " +
			catalog.ProviderNames()
	}
	// Every model runs somewhere, so an unstated location is the provider's,
	// or read off the backend, as for a catalogue file.
	m.Location = strings.ToLower(strings.TrimSpace(m.Location))
	switch {
	case m.Location == "" && known:
		m.Location = provider.Location
	case m.Location == "":
		loc, err := m.LocationFromBackends()
		if err != nil {
			return err.Error()
		}
		m.Location = loc
	}
	m.ReleaseDate = strings.TrimSpace(m.ReleaseDate)
	switch {
	case !policy.ValidLocation(m.Location):
		return "'location' must be a short lowercase name, such as ch, usa or onprem"
	case m.ReleaseDate != "" && !policy.ValidReleaseDate(m.ReleaseDate):
		return "'release_date' must be a day written as YYYY-MM-DD"
	}
	return ""
}

// checkModelAlias refuses a new model named like one of the organisation's
// routers. A model is looked up first, so the router would become
// unreachable.
func (s *Server) checkModelAlias(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	switch _, err := s.st.Model(r.Context(), orgID, alias); {
	case err == nil:
		return true // an edit
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return false
	}
	_, err := s.st.Router(r.Context(), orgID, alias)
	return !s.aliasTaken(w, err, "'"+alias+"' is already one of this organisation's routers, "+
		"which a model of the same alias would make unreachable - give this model a "+
		"different alias")
}

// aliasTaken refuses an alias that another kind of thing already has, and
// reports whether it did. err is what looking that thing up returned.
func (s *Server) aliasTaken(w http.ResponseWriter, err error, msg string) bool {
	switch {
	case err == nil:
		httpx.WriteError(w, http.StatusConflict, "invalid_request_error", "alias_in_use", msg)
		return true
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return true
	}
	return false
}

// setModelCredential seals a pasted credential, or clears the stored one when
// the field was emptied.
func (s *Server) setModelCredential(r *http.Request, p *authn.Principal,
	orgID, alias, credential string,
) error {
	var sealed []byte
	if credential != "" {
		var err error
		if sealed, err = s.opts.Secrets.Seal(registry.ModelSecretName(orgID, alias), credential); err != nil {
			return err
		}
	}
	if err := s.st.SetModelCredential(r.Context(), orgID, alias, sealed); err != nil {
		return err
	}
	action := "model.credential.set"
	if credential == "" {
		action = "model.credential.clear"
	}
	// Nothing about the credential goes into the audit log, not even its
	// length. Who set it and when is the record.
	s.auditf(r, p, orgID, action, "model", alias, nil)
	return nil
}

// checkModel tests one model against its own backends. It is for the
// organisation's administrators, because it reveals where the model runs and
// puts load on it.
//
// It offers the model a tool and a question only that tool can answer, so a
// vLLM tool-call parser that does not match the model shows up here.
func (s *Server) checkModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	// The registry, not the store: it holds the decrypted credential the data
	// plane would present.
	m, found := s.reg.Model(orgID, alias)
	if !found {
		s.fail(w, store.ErrNotFound)
		return
	}
	probe := s.opts.Gateway.CheckModel(r.Context(), m)
	// Kept, so an operator can later show when a model last worked.
	s.auditf(r, p, orgID, "model.check", "model", alias, map[string]any{
		"ok": probe.OK, "status": probe.Status, "tool_calls": probe.ToolCalls,
		"tool_call_as_text": probe.ToolCallAsText, "error": probe.Error,
	})
	httpx.WriteJSON(w, http.StatusOK, probe)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	if err := s.st.DeleteModel(r.Context(), orgID, alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, orgID, "model", alias)
}
