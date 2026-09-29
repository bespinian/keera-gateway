package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// limitFields are a guardrail's columns, in the order limitTargets reads them
// and limitValues writes them. Every query over them is built from this list,
// so a new limit is one entry here and one in each of those two.
var limitFields = []string{"allowed_models", "max_output_tokens", "rpm", "tpm",
	"budget_micros", "budget_period", "system_prompt", "filters",
	"max_sandboxes", "max_sandbox_ttl_seconds", "sandbox_classes",
	"max_sandbox_cpu_millis", "max_sandbox_memory_mib", "allowed_tools", "block_hosted_tools",
	"allowed_repos"}

// limitColumns is a guardrail row as policy.Limits holds it, read from a table
// aliased p.
var limitColumns = "p." + strings.Join(limitFields, ", p.")

// putPolicySQL writes one scope's limits, $1 and $2 being the scope and the
// rest limitValues.
var putPolicySQL = func() string {
	values := make([]string, len(limitFields))
	updates := make([]string, len(limitFields))
	for i, f := range limitFields {
		values[i] = fmt.Sprintf("$%d", i+3)
		updates[i] = f + " = EXCLUDED." + f
	}
	return `INSERT INTO guardrails (scope_type, scope_id, ` + strings.Join(limitFields, ", ") +
		`, updated_at) VALUES ($1, $2, ` + strings.Join(values, ", ") + `, now())
		ON CONFLICT (scope_type, scope_id) DO UPDATE SET ` + strings.Join(updates, ", ") +
		`, updated_at = now()`
}()

// copyKeyPolicySQL gives key $2 the limits of key $1.
var copyKeyPolicySQL = `INSERT INTO guardrails (scope_type, scope_id, ` +
	strings.Join(limitFields, ", ") + `, updated_at)
	SELECT scope_type, $2, ` + strings.Join(limitFields, ", ") + `, now()
	FROM guardrails WHERE scope_type = 'key' AND scope_id = $1`

// limitTargets points at the fields limitColumns fills. The budget period is
// nullable text, so it is scanned into period and converted afterwards.
func limitTargets(lim *policy.Limits, period **string) []any {
	return []any{&lim.AllowedModels, &lim.MaxOutputTokens, &lim.RPM, &lim.TPM,
		&lim.BudgetMicros, period, &lim.SystemPrompt, &lim.Filters,
		&lim.MaxSandboxes, &lim.MaxSandboxTTLSeconds, &lim.SandboxClasses,
		&lim.MaxSandboxCPU, &lim.MaxSandboxMemory, &lim.AllowedTools, &lim.BlockHostedTools,
		&lim.AllowedRepos}
}

// limitValues is what putPolicySQL writes, in limitFields' order.
func limitValues(lim policy.Limits) []any {
	return []any{lim.AllowedModels, lim.MaxOutputTokens, lim.RPM, lim.TPM,
		lim.BudgetMicros, periodStr(lim.BudgetPeriod), lim.SystemPrompt, lim.Filters,
		lim.MaxSandboxes, lim.MaxSandboxTTLSeconds, lim.SandboxClasses,
		lim.MaxSandboxCPU, lim.MaxSandboxMemory, lim.AllowedTools, lim.BlockHostedTools,
		lim.AllowedRepos}
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
	args := append([]any{string(scopeType), scopeID}, limitValues(lim)...)
	_, err := s.pool.Exec(ctx, putPolicySQL, args...)
	return err
}

// GuardrailRef is one guardrail that names a filter, a router or an MCP server.
type GuardrailRef struct {
	ScopeType policy.ScopeType `json:"scope_type"`
	ScopeID   string           `json:"scope_id"`
	// Name is what that scope is called, so a refusal can name the team rather
	// than show an id.
	Name string `json:"name"`
}

// scopesNaming lists the guardrails inside one organisation that name alias.
// match is the SQL condition, with alias as $2; it is always a constant from
// this package.
func (s *Store) scopesNaming(ctx context.Context, match, orgID, alias string) ([]GuardrailRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.scope_type, p.scope_id, COALESCE(o.name, t.name, k.alias, p.scope_id)
		FROM guardrails p
		LEFT JOIN orgs     o ON p.scope_type = 'org'  AND o.id = p.scope_id AND o.id = $1
		LEFT JOIN teams    t ON p.scope_type = 'team' AND t.id = p.scope_id AND t.org_id = $1
		LEFT JOIN api_keys k ON p.scope_type = 'key'  AND k.id = p.scope_id AND k.org_id = $1
		WHERE `+match+`
		  AND (o.id IS NOT NULL OR t.id IS NOT NULL OR k.id IS NOT NULL)
		ORDER BY p.scope_type, p.scope_id`, orgID, alias)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r row) (GuardrailRef, error) {
		var fs GuardrailRef
		err := r.Scan(&fs.ScopeType, &fs.ScopeID, &fs.Name)
		return fs, err
	})
}

// MCPServerUsers lists the guardrails inside one organisation whose tool
// allow-list names an MCP server, for all its tools or one of them. It is read
// before a deletion, as FilterUsers is: a guardrail naming a missing server
// cannot be saved again.
func (s *Store) MCPServerUsers(ctx context.Context, orgID, alias string) ([]GuardrailRef, error) {
	return s.scopesNaming(ctx,
		"EXISTS (SELECT 1 FROM unnest(p.allowed_tools) e WHERE split_part(e, '/', 1) = $2)",
		orgID, alias)
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
	// The one place a missing mode gets its default.
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
func (s *Store) FilterUsers(ctx context.Context, orgID, alias string) ([]GuardrailRef, error) {
	return s.scopesNaming(ctx, "$2 = ANY (p.filters)", orgID, alias)
}
