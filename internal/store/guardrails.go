package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// limitFields are a guardrail's columns, in the order limitTargets reads them
// and limitValues writes them. A new limit needs an entry here and in both.
var limitFields = []string{
	"allowed_models", "rate_limits", "budgets", "system_prompt", "filters",
	"max_sandboxes", "max_sandbox_ttl_seconds", "sandbox_classes",
	"max_sandbox_cpu_millis", "max_sandbox_memory_mib", "allowed_tools", "allowed_repos",
}

// limitColumns is a guardrail row as policy.Limits holds it, from a table
// aliased p.
var limitColumns = "p." + strings.Join(limitFields, ", p.")

// putGuardrailSQL writes one scope's limits, $1 and $2 being the scope and the
// rest limitValues.
var putGuardrailSQL = func() string {
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

// copyKeyGuardrailSQL gives key $2 the limits of key $1.
var copyKeyGuardrailSQL = `INSERT INTO guardrails (scope_type, scope_id, ` +
	strings.Join(limitFields, ", ") + `, updated_at)
	SELECT scope_type, $2, ` + strings.Join(limitFields, ", ") + `, now()
	FROM guardrails WHERE scope_type = 'key' AND scope_id = $1`

// limitTargets points at the fields limitColumns fills.
func limitTargets(lim *policy.Limits) []any {
	return []any{
		&lim.AllowedModels, &lim.RateLimits, &lim.Budgets, &lim.SystemPrompt, &lim.Filters,
		&lim.MaxSandboxes, &lim.MaxSandboxTTLSeconds, &lim.SandboxClasses,
		&lim.MaxSandboxCPU, &lim.MaxSandboxMemory, &lim.AllowedTools, &lim.AllowedRepos,
	}
}

// limitValues is what putGuardrailSQL writes, in limitFields' order.
func limitValues(lim policy.Limits) []any {
	return []any{
		lim.AllowedModels, jsonList(lim.RateLimits), jsonList(lim.Budgets),
		lim.SystemPrompt, lim.Filters,
		lim.MaxSandboxes, lim.MaxSandboxTTLSeconds, lim.SandboxClasses,
		lim.MaxSandboxCPU, lim.MaxSandboxMemory, lim.AllowedTools, lim.AllowedRepos,
	}
}

// jsonList writes an empty list as NULL, since the driver would send a nil
// slice as JSON null, which the column's check refuses.
func jsonList[T any](list []T) any {
	if len(list) == 0 {
		return nil
	}
	return list
}

func (s *Store) GetGuardrail(ctx context.Context, scopeType policy.ScopeType, scopeID string) (policy.Limits, error) {
	var lim policy.Limits
	err := s.pool.QueryRow(ctx, `SELECT `+limitColumns+`
		FROM guardrails p WHERE p.scope_type = $1 AND p.scope_id = $2`,
		string(scopeType), scopeID,
	).Scan(limitTargets(&lim)...)
	if err != nil {
		return policy.Limits{}, notFound(err)
	}
	return lim, nil
}

func (s *Store) PutGuardrail(ctx context.Context, scopeType policy.ScopeType, scopeID string, lim policy.Limits) error {
	args := append([]any{string(scopeType), scopeID}, limitValues(lim)...)
	_, err := s.pool.Exec(ctx, putGuardrailSQL, args...)
	return err
}

// LockReferences holds off every other holder in one organisation until
// unlock is called. Saves that check what they name exists, and deletes that
// check nothing names what they delete, hold it, so the check still holds at
// the write. Otherwise a guardrail saved during a delete could name a missing
// filter and refuse every request.
//
// The lock lives in a transaction that writes nothing, so a lost connection
// cannot leave it held. That transaction holds a connection and the checks
// need another, so at most half the pool may hold one, or no holder could
// finish.
func (s *Store) LockReferences(ctx context.Context, orgID string) (unlock func(), err error) {
	select {
	case s.referenceSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	free := func() { <-s.referenceSlots }
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		free()
		return nil, err
	}
	release := func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		free()
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		"references:"+orgID); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// GuardrailRef is one guardrail that names a filter, a router or an MCP server.
type GuardrailRef struct {
	ScopeType policy.ScopeType `json:"scope_type"`
	ScopeID   string           `json:"scope_id"`
	// Name is what that scope is called, so a refusal can show a name, not an id.
	Name string `json:"name"`
}

// scopesNaming lists the guardrails in one organisation that name alias.
// match is the SQL condition, with alias as $2; it is always a constant.
func (s *Store) scopesNaming(ctx context.Context, match, orgID, alias string) ([]GuardrailRef, error) {
	return queryAll(ctx, s.pool, scanGuardrailRef, `
		SELECT p.scope_type, p.scope_id, COALESCE(o.name, pr.name, k.name, p.scope_id)
		FROM guardrails p
		LEFT JOIN orgs      o ON p.scope_type = 'org'     AND o.id = p.scope_id AND o.id = $1
		LEFT JOIN projects pr ON p.scope_type = 'project' AND pr.id = p.scope_id AND pr.org_id = $1
		LEFT JOIN api_keys  k ON p.scope_type = 'key'     AND k.id = p.scope_id AND k.org_id = $1
		WHERE `+match+`
		  AND (o.id IS NOT NULL OR pr.id IS NOT NULL OR k.id IS NOT NULL)
		ORDER BY p.scope_type, p.scope_id`, orgID, alias)
}

func scanGuardrailRef(r row) (GuardrailRef, error) {
	var fs GuardrailRef
	err := r.Scan(&fs.ScopeType, &fs.ScopeID, &fs.Name)
	return fs, err
}

// MCPServerUsers lists the guardrails in one organisation whose tool
// allow-list names an MCP server or one of its tools, to check before a
// deletion: a guardrail naming a missing server cannot be saved again.
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
	// No rules is always nil, so callers see one form of "none".
	if len(f.Rules) == 0 {
		f.Rules = nil
	}
	return f, err
}

// LoadFilters reads every organisation's filters, for the gateway's cache, so
// the inference path never waits on Postgres.
func (s *Store) LoadFilters(ctx context.Context) ([]policy.Filter, error) {
	return queryAll(ctx, s.pool, scanFilter, filterColumns+" FROM filters ORDER BY org_id, alias")
}

func (s *Store) ListFilters(ctx context.Context, orgID string) ([]policy.Filter, error) {
	return queryAll(ctx, s.pool, scanFilter,
		filterColumns+" FROM filters WHERE org_id = $1 ORDER BY alias", orgID)
}

func (s *Store) Filter(ctx context.Context, orgID, alias string) (policy.Filter, error) {
	f, err := scanFilter(s.pool.QueryRow(ctx,
		filterColumns+" FROM filters WHERE org_id = $1 AND alias = $2", orgID, alias))
	return f, notFound(err)
}

func (s *Store) UpsertFilter(ctx context.Context, f policy.Filter) (policy.Filter, error) {
	// The one place a missing mode gets its default.
	if f.Mode == "" {
		f.Mode = policy.FilterModeRewrite
	}
	// Marshalled here because the driver would send a nil slice as JSON null.
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

func (s *Store) DeleteFilter(ctx context.Context, orgID, alias string) error {
	return s.execOne(ctx, "DELETE FROM filters WHERE org_id = $1 AND alias = $2", orgID, alias)
}

// FilterUsers lists the guardrails in one organisation that name a filter, to
// check before a deletion: a guardrail naming a missing filter refuses every
// request.
func (s *Store) FilterUsers(ctx context.Context, orgID, alias string) ([]GuardrailRef, error) {
	return s.scopesNaming(ctx, "$2 = ANY (p.filters)", orgID, alias)
}

// AllowInOrg adds alias to an organisation's allowed models, if it keeps that
// list, so a new model is not refused from the start. It reports whether the
// list changed.
func (s *Store) AllowInOrg(ctx context.Context, orgID, alias string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE guardrails
		SET allowed_models = array_append(allowed_models, $2)
		WHERE scope_type = $3 AND scope_id = $1
		  AND allowed_models IS NOT NULL AND NOT ($2 = ANY (allowed_models))`,
		orgID, alias, string(policy.ScopeOrg))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
