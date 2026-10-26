package control

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The developer's own screen: are my keys alive, how much budget is left, why
// was I refused, and what may I call.
//
// It reads only what belongs to the caller. Even an administrator sees only
// their own keys here.

// accessScope is one level of the hierarchy as the developer meets it: its
// limits, what is used of them, and when they reset.
type accessScope struct {
	Type policy.ScopeType `json:"type"`
	// ID is sent so a developer can quote it to an administrator.
	ID      string         `json:"id"`
	Name    string         `json:"name,omitempty"`
	Rates   []AccessRate   `json:"rates,omitempty"`
	Budgets []AccessBudget `json:"budgets,omitempty"`
	// SystemPrompt is this level's own instruction, not the joined chain, so
	// the developer sees who added what to their requests.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Filters is this level's own, for the same reason: their own traffic
	// will never show what is taken out.
	Filters []string `json:"filters,omitempty"`
}

// AccessRate is one rate limit of a level. Per day or month, the Used fields
// say how much is gone.
type AccessRate struct {
	Requests     int64         `json:"requests,omitempty"`
	Tokens       int64         `json:"tokens,omitempty"`
	Period       policy.Period `json:"period"`
	RequestsUsed int64         `json:"requests_used,omitempty"`
	TokensUsed   int64         `json:"tokens_used,omitempty"`
}

// AccessBudget is one budget of a level, and what is spent of it.
type AccessBudget struct {
	BudgetMicros int64         `json:"budget_micros"`
	SpendMicros  int64         `json:"spend_micros"`
	Period       policy.Period `json:"period"`
	ResetsAt     time.Time     `json:"resets_at"`
}

// accessKey is one of the caller's keys with everything that decides whether
// it works: its state, the limits above it, and what it may call.
type accessKey struct {
	store.KeySummary
	ProjectName   string        `json:"project_name,omitempty"`
	State         string        `json:"state"`
	Scopes        []accessScope `json:"scopes"`
	AllowedModels []string      `json:"allowed_models,omitempty"`
}

// isPerson reports whether p is someone in an organisation who can hold
// keys. The operator key is not.
func isPerson(p *authn.Principal) bool {
	return p.Via != authn.MethodOperatorKey && p.UserID != "" && p.OrgID != ""
}

func (s *Server) access(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	now := time.Now()
	out := map[string]any{
		"email":       p.Email,
		"role":        p.Role,
		"gateway_url": s.gatewayURL(r),
		"keys":        []accessKey{},
		"refusals":    []store.Refusal{},
	}

	// The operator key has no keys or spend. Saying so is clearer than an
	// empty screen.
	if !isPerson(p) {
		out["anonymous"] = true
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}

	// In the panel the operator key signs in as a stand-in user. The screen
	// says so, and still lists any keys it issued for itself.
	if u, err := s.st.UserByID(r.Context(), p.UserID); err == nil &&
		u.ExternalID == operatorKeyExternalID {
		out["operator_key"] = true
	}

	orgName := s.orgName(r.Context(), p.OrgID)
	out["org"] = map[string]string{"id": p.OrgID, "name": orgName}

	keys, err := s.st.KeySummaries(r.Context(), store.KeyQuery{
		OrgID: p.OrgID, UserID: p.UserID,
		Since: policy.PeriodMonth.Start(now),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	shown, err := s.accessKeys(r, p.OrgID, orgName, keys, now)
	if err != nil {
		s.fail(w, err)
		return
	}
	out["keys"] = shown

	keyIDs := make([]string, 0, len(keys))
	for _, k := range keys {
		keyIDs = append(keyIDs, k.ID)
	}
	// Only the last week: the question is why something stopped working
	// today, not history.
	refusals, err := s.st.Refusals(r.Context(), p.OrgID, keyIDs, now.Add(-7*24*time.Hour), 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	out["refusals"] = refusals

	// Ever, not this month: the panel's getting started guide ends with the
	// first request and must not come back when a month turns.
	used, err := s.st.AnyUsed(r.Context(), p.OrgID, keyIDs)
	if err != nil {
		s.fail(w, err)
		return
	}
	out["used"] = used

	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) accessKeys(r *http.Request, orgID, orgName string, keys []store.KeySummary,
	now time.Time,
) ([]accessKey, error) {
	projectNames, err := s.st.ProjectNames(r.Context(), orgID)
	if err != nil {
		return nil, err
	}
	models, err := s.callable(r, orgID)
	if err != nil {
		return nil, err
	}
	// Each level is read once, not once per key.
	orgLimits := s.limitsFor(r, policy.ScopeOrg, orgID)
	projectLimits := map[string]*policy.Limits{}

	shown := make([]accessKey, 0, len(keys))
	for _, k := range keys {
		if _, ok := projectLimits[k.ProjectID]; !ok && k.ProjectID != "" {
			projectLimits[k.ProjectID] = s.limitsFor(r, policy.ScopeProject, k.ProjectID)
		}
		own := k.Limits
		resolved := policy.Resolve(
			policy.Key{ID: k.ID, OrgID: k.OrgID, ProjectID: k.ProjectID, UserID: k.UserID, Kind: k.Kind},
			orgLimits, projectLimits[k.ProjectID], &own)

		shown = append(shown, accessKey{
			KeySummary:  k,
			ProjectName: projectNames[k.ProjectID],
			State:       k.State(now),
			// Resolved to real names, so the screen lists what the key can
			// call instead of "no restriction".
			AllowedModels: allowedModels(resolved, models),
			Scopes: s.scopeStates(resolved, orgName, projectNames, k.Name, now,
				map[policy.ScopeType]scopeSays{
					policy.ScopeOrg:     saysOf(orgLimits),
					policy.ScopeProject: saysOf(projectLimits[k.ProjectID]),
					policy.ScopeKey:     saysOf(&own),
				}),
		})
	}
	return shown, nil
}

// callable is every name a client may put in the model field: the
// organisation's models and routers. Without routers, a key whose allow-list
// names only one would look like it may call nothing.
func (s *Server) callable(r *http.Request, orgID string) ([]policy.Model, error) {
	models, err := s.st.ListModels(r.Context(), orgID)
	if err != nil {
		return nil, err
	}
	routers, err := s.st.ListRouters(r.Context(), orgID)
	if err != nil {
		return nil, err
	}
	for _, p := range routers {
		models = append(models, policy.Model{Alias: p.Alias, Enabled: true})
	}
	return models, nil
}

// scopeSays is what one level adds to a request: its instruction and
// filters. Both add up down the chain, so they are reported per level.
type scopeSays struct {
	prompt  string
	filters []string
}

func saysOf(lim *policy.Limits) scopeSays {
	if lim == nil {
		return scopeSays{}
	}
	says := scopeSays{filters: lim.Filters}
	if lim.SystemPrompt != nil {
		says.prompt = *lim.SystemPrompt
	}
	return says
}

// limitsFor reads one scope's stored limits. A failed read counts as no
// limit rather than failing the screen: this only describes the guardrail.
func (s *Server) limitsFor(r *http.Request, scope policy.ScopeType, id string) *policy.Limits {
	lim, err := s.storedLimits(r.Context(), scope, id)
	if err != nil {
		return &policy.Limits{}
	}
	return &lim
}

// scopeStates turns a resolved chain into what each level allows and has
// spent, outermost first, as the limits apply. A level may have one scope per
// budget period; they are shown as one.
func (s *Server) scopeStates(res *policy.Resolved, orgName string,
	projectNames map[string]string, keyName string, now time.Time,
	says map[policy.ScopeType]scopeSays,
) []accessScope {
	out := make([]accessScope, 0, len(res.Scopes))
	for _, sc := range res.Scopes {
		if n := len(out); n == 0 || out[n-1].Type != sc.Type || out[n-1].ID != sc.ID {
			out = append(out, s.levelState(res, sc, orgName, projectNames, keyName, now, says))
		}
		st := &out[len(out)-1]
		if sc.BudgetMicros > 0 {
			// The gateway's own spend figure, not a fresh query: the budget is
			// enforced against it.
			st.Budgets = append(st.Budgets, AccessBudget{
				BudgetMicros: sc.BudgetMicros, Period: sc.Period,
				SpendMicros: s.reg.Budgets().Spent(sc.Type, sc.ID, sc.Period, now),
				ResetsAt:    sc.Period.Next(now),
			})
		}
	}
	return out
}

// levelState is one level with its rate limits, before its budgets.
func (s *Server) levelState(res *policy.Resolved, sc policy.Scope, orgName string,
	projectNames map[string]string, keyName string, now time.Time,
	says map[policy.ScopeType]scopeSays,
) accessScope {
	st := accessScope{
		Type: sc.Type, ID: sc.ID,
		SystemPrompt: says[sc.Type].prompt, Filters: says[sc.Type].filters,
	}
	switch sc.Type {
	case policy.ScopeOrg:
		st.Name = orgName
	case policy.ScopeProject:
		st.Name = projectNames[sc.ID]
	case policy.ScopeKey:
		st.Name = keyName
	}
	if sc.RPM > 0 || sc.TPM > 0 {
		st.Rates = append(st.Rates, AccessRate{
			Requests: int64(sc.RPM), Tokens: int64(sc.TPM), Period: policy.PeriodMinute,
		})
	}
	// A rate limit per day or month is a quota per count; the two counts of
	// one period are one limit.
	for _, q := range res.Quotas {
		if q.Type != sc.Type || !strings.HasPrefix(q.ID, sc.ID+"|") {
			continue
		}
		i := slices.IndexFunc(st.Rates, func(r AccessRate) bool { return r.Period == q.Period })
		if i < 0 {
			st.Rates = append(st.Rates, AccessRate{Period: q.Period})
			i = len(st.Rates) - 1
		}
		used := s.reg.Budgets().Spent(q.Type, q.ID, q.Period, now)
		if q.Meter == policy.MeterRequests {
			st.Rates[i].Requests, st.Rates[i].RequestsUsed = q.Quota, used
		} else {
			st.Rates[i].Tokens, st.Rates[i].TokensUsed = q.Quota, used
		}
	}
	return st
}

// allowedModels is what a key may actually call: the enabled models its
// kind reaches and its allow-list permits.
func allowedModels(res *policy.Resolved, models []policy.Model) []string {
	out := []string{}
	for _, m := range models {
		if m.Enabled && res.MayUse(m) {
			out = append(out, m.Alias)
		}
	}
	return out
}
