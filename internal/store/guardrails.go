package store

import (
	"context"
	"encoding/json"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// limitColumns is a guardrail row as policy.Limits holds it, read from a table
// aliased p. limitTargets scans the same columns in the same order.
const limitColumns = `p.allowed_models, p.max_output_tokens, p.rpm, p.tpm,
	p.budget_micros, p.budget_period, p.system_prompt, p.filters,
	p.max_sandboxes, p.max_sandbox_ttl_seconds, p.sandbox_classes,
	p.max_sandbox_cpu_millis, p.max_sandbox_memory_mib, p.allowed_tools, p.block_hosted_tools`

// limitTargets points at the fields limitColumns fills. The budget period is
// nullable text, so it is scanned into period and converted afterwards.
func limitTargets(lim *policy.Limits, period **string) []any {
	return []any{&lim.AllowedModels, &lim.MaxOutputTokens, &lim.RPM, &lim.TPM,
		&lim.BudgetMicros, period, &lim.SystemPrompt, &lim.Filters,
		&lim.MaxSandboxes, &lim.MaxSandboxTTLSeconds, &lim.SandboxClasses,
		&lim.MaxSandboxCPU, &lim.MaxSandboxMemory, &lim.AllowedTools, &lim.BlockHostedTools}
}

// GetPolicy reads the limits attached to one scope.
func (s *Store) GetPolicy(ctx context.Context, scopeType policy.ScopeType, scopeID string) (policy.Limits, error) {
	var (
		lim    policy.Limits
		period *string
	)
	err := s.pool.QueryRow(ctx, `SELECT `+limitColumns+`
		FROM guardrails p WHERE p.scope_type = $1 AND p.scope_id = $2`,
		string(scopeType), scopeID,
	).Scan(limitTargets(&lim, &period)...)
	if err != nil {
		return policy.Limits{}, notFound(err)
	}
	lim.BudgetPeriod = periodPtr(period)
	return lim, nil
}

// PutPolicy replaces the limits attached to one scope.
func (s *Store) PutPolicy(ctx context.Context, scopeType policy.ScopeType, scopeID string, lim policy.Limits) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO guardrails
		(scope_type, scope_id, allowed_models, max_output_tokens, rpm, tpm, budget_micros,
		 budget_period, system_prompt, filters, max_sandboxes, max_sandbox_ttl_seconds,
		 sandbox_classes, max_sandbox_cpu_millis, max_sandbox_memory_mib, allowed_tools,
		 block_hosted_tools, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17, now())
		ON CONFLICT (scope_type, scope_id) DO UPDATE SET
			allowed_models = EXCLUDED.allowed_models, max_output_tokens = EXCLUDED.max_output_tokens,
			rpm = EXCLUDED.rpm, tpm = EXCLUDED.tpm, budget_micros = EXCLUDED.budget_micros,
			budget_period = EXCLUDED.budget_period, system_prompt = EXCLUDED.system_prompt,
			filters = EXCLUDED.filters, max_sandboxes = EXCLUDED.max_sandboxes,
			max_sandbox_ttl_seconds = EXCLUDED.max_sandbox_ttl_seconds,
			sandbox_classes = EXCLUDED.sandbox_classes,
			max_sandbox_cpu_millis = EXCLUDED.max_sandbox_cpu_millis,
			max_sandbox_memory_mib = EXCLUDED.max_sandbox_memory_mib,
			allowed_tools = EXCLUDED.allowed_tools,
			block_hosted_tools = EXCLUDED.block_hosted_tools, updated_at = now()`,
		string(scopeType), scopeID, lim.AllowedModels, lim.MaxOutputTokens, lim.RPM, lim.TPM,
		lim.BudgetMicros, periodStr(lim.BudgetPeriod), lim.SystemPrompt, lim.Filters,
		lim.MaxSandboxes, lim.MaxSandboxTTLSeconds, lim.SandboxClasses,
		lim.MaxSandboxCPU, lim.MaxSandboxMemory, lim.AllowedTools, lim.BlockHostedTools)
	return err
}

// FilterScope is one guardrail that names a filter or a router.
type FilterScope struct {
	ScopeType policy.ScopeType `json:"scope_type"`
	ScopeID   string           `json:"scope_id"`
	// Name is what that scope is called, so a refusal can name the team rather
	// than show an id.
	Name string `json:"name"`
}

// scopesNaming lists the guardrails inside one organisation whose column
// contains alias. column is always a constant from this package.
func (s *Store) scopesNaming(ctx context.Context, column, orgID, alias string) ([]FilterScope, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.scope_type, p.scope_id, COALESCE(o.name, t.name, k.alias, p.scope_id)
		FROM guardrails p
		LEFT JOIN orgs     o ON p.scope_type = 'org'  AND o.id = p.scope_id AND o.id = $1
		LEFT JOIN teams    t ON p.scope_type = 'team' AND t.id = p.scope_id AND t.org_id = $1
		LEFT JOIN api_keys k ON p.scope_type = 'key'  AND k.id = p.scope_id AND k.org_id = $1
		WHERE $2 = ANY (p.`+column+`)
		  AND (o.id IS NOT NULL OR t.id IS NOT NULL OR k.id IS NOT NULL)
		ORDER BY p.scope_type, p.scope_id`, orgID, alias)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (FilterScope, error) {
		var fs FilterScope
		err := r.Scan(&fs.ScopeType, &fs.ScopeID, &fs.Name)
		return fs, err
	})
}

const filterColumns = `SELECT org_id, alias, model, mode, shadow, prompt, rules, description,
	created_at, updated_at`

func scanFilter(r row) (policy.Filter, error) {
	var f policy.Filter
	err := r.Scan(&f.OrgID, &f.Alias, &f.Model, &f.Mode, &f.Shadow, &f.Prompt, &f.Rules,
		&f.Description, &f.CreatedAt, &f.UpdatedAt)
	// No rules is always nil, so callers only ever see one form of "none".
	if len(f.Rules) == 0 {
		f.Rules = nil
	}
	return f, err
}

// LoadFilters reads every organisation's filters, for the gateway's cache.
// Nothing on the inference path may wait on Postgres.
func (s *Store) LoadFilters(ctx context.Context) ([]policy.Filter, error) {
	rows, err := s.pool.Query(ctx, filterColumns+" FROM filters ORDER BY org_id, alias")
	if err != nil {
		return nil, err
	}
	return collect(rows, scanFilter)
}

// ListFilters reads one organisation's filters.
func (s *Store) ListFilters(ctx context.Context, orgID string) ([]policy.Filter, error) {
	rows, err := s.pool.Query(ctx,
		filterColumns+" FROM filters WHERE org_id = $1 ORDER BY alias", orgID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanFilter)
}

// Filter reads one filter, or ErrNotFound.
func (s *Store) Filter(ctx context.Context, orgID, alias string) (policy.Filter, error) {
	f, err := scanFilter(s.pool.QueryRow(ctx,
		filterColumns+" FROM filters WHERE org_id = $1 AND alias = $2", orgID, alias))
	if err != nil {
		return policy.Filter{}, notFound(err)
	}
	return f, nil
}

// UpsertFilter creates or replaces one filter.
func (s *Store) UpsertFilter(ctx context.Context, f policy.Filter) (policy.Filter, error) {
	if f.Mode == "" {
		f.Mode = policy.FilterModeRewrite
	}
	// Marshalled here because the driver would send a nil slice as JSON null,
	// which the column's check constraint refuses.
	own := f.Rules
	if own == nil {
		own = []policy.FilterRule{}
	}
	rules, err := json.Marshal(own)
	if err != nil {
		return f, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO filters
		(org_id, alias, model, mode, shadow, prompt, rules, description)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (org_id, alias) DO UPDATE SET model = EXCLUDED.model,
			mode = EXCLUDED.mode, shadow = EXCLUDED.shadow, prompt = EXCLUDED.prompt,
			rules = EXCLUDED.rules,
			description = EXCLUDED.description, updated_at = now()
		RETURNING created_at, updated_at`,
		f.OrgID, f.Alias, f.Model, string(f.Mode), f.Shadow, f.Prompt, rules, f.Description,
	).Scan(&f.CreatedAt, &f.UpdatedAt)
	return f, err
}

// DeleteFilter removes one filter.
func (s *Store) DeleteFilter(ctx context.Context, orgID, alias string) error {
	return s.execOne(ctx, "DELETE FROM filters WHERE org_id = $1 AND alias = $2", orgID, alias)
}

// FilterUsers lists the guardrails inside one organisation that name a filter.
//
// It is read before a deletion. A guardrail that names a missing filter
// refuses every request, so removing a filter in use would break a team.
func (s *Store) FilterUsers(ctx context.Context, orgID, alias string) ([]FilterScope, error) {
	return s.scopesNaming(ctx, "filters", orgID, alias)
}
