package control

import (
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The developer's own screen. It answers the questions of the person using
// the gateway: are my keys alive, how much budget is left, why was I refused,
// and what may I call.
//
// It reads only what belongs to the caller. Even an administrator sees only
// their own keys here.

// accessScope is one level of the hierarchy as the developer meets it: a limit
// with what has been spent against it, and when it resets.
type accessScope struct {
	Type policy.ScopeType `json:"type"`
	// ID is sent with the name, so a developer can quote it to an
	// administrator.
	ID           string        `json:"id"`
	Name         string        `json:"name,omitempty"`
	RPM          int           `json:"rpm,omitempty"`
	TPM          int           `json:"tpm,omitempty"`
	BudgetMicros int64         `json:"budget_micros,omitempty"`
	SpendMicros  int64         `json:"spend_micros"`
	Period       policy.Period `json:"period,omitempty"`
	ResetsAt     *time.Time    `json:"resets_at,omitempty"`
	// SystemPrompt is this level's own instruction, not the joined chain, so
	// the developer sees who added what. It is text they never wrote, added
	// ahead of every chat request they make.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Filters is this level's own filters, shown for the same reason: their
	// own traffic will never tell them what is being taken out.
	Filters []string `json:"filters,omitempty"`
}

// accessKey is one of the caller's keys with everything that decides whether it
// works: its state, the chain of limits above it, and what it may call.
type accessKey struct {
	store.KeySummary
	TeamName        string        `json:"team_name,omitempty"`
	State           string        `json:"state"`
	Scopes          []accessScope `json:"scopes"`
	AllowedModels   []string      `json:"allowed_models,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
}

func (s *Server) access(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	now := time.Now()
	out := map[string]any{
		"email":       p.Email,
		"role":        p.Role,
		"currency":    s.opts.Currency,
		"gateway_url": s.gatewayURL(r),
		"keys":        []accessKey{},
		"refusals":    []store.Refusal{},
	}

	// The operator key is not a person and has no keys or spend. Saying so is
	// clearer than an empty screen.
	if p.Via == authn.MethodOperatorKey || p.UserID == "" || p.OrgID == "" {
		out["anonymous"] = true
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}

	orgName, _ := s.orgName(r.Context(), p.OrgID)
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
	// Only the last week: the question is about the agent that stopped
	// working today, not history.
	refusals, err := s.st.Refusals(r.Context(), p.OrgID, keyIDs, now.Add(-7*24*time.Hour), 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	out["refusals"] = refusals

	httpx.WriteJSON(w, http.StatusOK, out)
}

// accessKeys describes each of the caller's keys with the limits above it.
func (s *Server) accessKeys(r *http.Request, orgID, orgName string, keys []store.KeySummary,
	now time.Time,
) ([]accessKey, error) {
	teamNames, err := s.st.TeamNames(r.Context(), orgID)
	if err != nil {
		return nil, err
	}
	models, err := s.callable(r, orgID)
	if err != nil {
		return nil, err
	}
	// Each level above a key is read once, not once per key.
	orgLimits := s.limitsFor(r, policy.ScopeOrg, orgID)
	teamLimits := map[string]*policy.Limits{}

	shown := make([]accessKey, 0, len(keys))
	for _, k := range keys {
		if _, ok := teamLimits[k.TeamID]; !ok && k.TeamID != "" {
			teamLimits[k.TeamID] = s.limitsFor(r, policy.ScopeTeam, k.TeamID)
		}
		own := k.Limits
		resolved := policy.Resolve(
			policy.Key{ID: k.ID, OrgID: k.OrgID, TeamID: k.TeamID, UserID: k.UserID},
			orgLimits, teamLimits[k.TeamID], &own)

		shown = append(shown, accessKey{
			KeySummary: k,
			TeamName:   teamNames[k.TeamID],
			State:      keyState(k, now),
			// Resolved to real names, so the screen lists what the key can
			// call instead of saying "no restriction".
			AllowedModels:   allowedModels(resolved, models),
			MaxOutputTokens: resolved.MaxOutputTokens,
			Scopes: s.scopeStates(resolved, orgName, teamNames, k.Alias, now,
				map[policy.ScopeType]scopeSays{
					policy.ScopeOrg:  saysOf(orgLimits),
					policy.ScopeTeam: saysOf(teamLimits[k.TeamID]),
					policy.ScopeKey:  saysOf(&own),
				}),
		})
	}
	return shown, nil
}

// callable is every name a client may put in the model field: the catalogue
// and the organisation's routers. Without the routers, a key whose allow-list
// names only a router would look like it may call nothing.
func (s *Server) callable(r *http.Request, orgID string) ([]policy.Model, error) {
	models, err := s.st.LoadModels(r.Context())
	if err != nil {
		return nil, err
	}
	routers, err := s.st.ListRouters(r.Context(), orgID)
	if err != nil {
		return nil, err
	}
	for _, rt := range routers {
		models = append(models, policy.Model{Alias: rt.Alias, Enabled: true})
	}
	return models, nil
}

// scopeSays is what one level adds to a request: its instruction and its
// filters. Both add up down the chain, so they are reported per level.
type scopeSays struct {
	prompt  string
	filters []string
}

// saysOf reads what one level adds, or nothing when it adds nothing.
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
// limit rather than failing the screen: the gateway enforces the guardrail,
// this only describes it.
func (s *Server) limitsFor(r *http.Request, scope policy.ScopeType, id string) *policy.Limits {
	lim, err := s.st.GetPolicy(r.Context(), scope, id)
	if err != nil {
		return &policy.Limits{}
	}
	return &lim
}

// scopeStates turns a resolved chain into what each level allows and what it
// has spent, outermost first: the order the limits apply in.
func (s *Server) scopeStates(res *policy.Resolved, orgName string,
	teamNames map[string]string, keyAlias string, now time.Time,
	says map[policy.ScopeType]scopeSays) []accessScope {
	out := make([]accessScope, 0, len(res.Scopes))
	for _, sc := range res.Scopes {
		st := accessScope{
			Type: sc.Type, ID: sc.ID, RPM: sc.RPM, TPM: sc.TPM,
			BudgetMicros: sc.BudgetMicros,
			SystemPrompt: says[sc.Type].prompt, Filters: says[sc.Type].filters,
		}
		switch sc.Type {
		case policy.ScopeOrg:
			st.Name = orgName
		case policy.ScopeTeam:
			st.Name = teamNames[sc.ID]
		case policy.ScopeKey:
			st.Name = keyAlias
		}
		if sc.BudgetMicros > 0 {
			// The gateway's own spend figure, not a fresh query: it is the
			// number the budget is enforced against.
			st.SpendMicros = s.reg.Budgets().Spent(sc.Type, sc.ID, sc.Period, now)
			st.Period = sc.Period
			resets := sc.Period.Next(now)
			st.ResetsAt = &resets
		}
		out = append(out, st)
	}
	return out
}

// allowedModels is what a key may actually call: the enabled models its
// allow-list permits.
func allowedModels(res *policy.Resolved, models []policy.Model) []string {
	out := []string{}
	for _, m := range models {
		if m.Enabled && res.AllowsModel(m.Alias) {
			out = append(out, m.Alias)
		}
	}
	return out
}

func keyState(k store.KeySummary, now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && k.ExpiresAt.Before(now):
		return "expired"
	default:
		return "active"
	}
}
