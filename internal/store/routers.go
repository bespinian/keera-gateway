package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Routers, and the report on what they did.
//
// There is no router_runs table. A request passes through exactly one router,
// so the decision is stored on the request's own usage row: which router,
// its outcome, and how long it took. The destination is that row's alias.

const routerColumns = `SELECT org_id, alias, mode, model, prompt, destinations, ceilings,
	fallback, description, created_at, updated_at`

func scanRouter(r row) (policy.Router, error) {
	var (
		rt       policy.Router
		fallback *string
	)
	err := r.Scan(&rt.OrgID, &rt.Alias, &rt.Mode, &rt.Model, &rt.Prompt, &rt.Destinations,
		&rt.Ceilings, &fallback, &rt.Description, &rt.CreatedAt, &rt.UpdatedAt)
	if fallback != nil {
		rt.Fallback = *fallback
	}
	// Only a size router has ceilings. Every other mode gets nil, not an empty
	// map.
	if len(rt.Ceilings) == 0 {
		rt.Ceilings = nil
	}
	return rt, err
}

// LoadRouters reads every organisation's routers, for the gateway's cache.
// The gateway checks every request's model against them, so they must not be
// read from Postgres on the request path.
func (s *Store) LoadRouters(ctx context.Context) ([]policy.Router, error) {
	return queryAll(ctx, s.pool, scanRouter, routerColumns+" FROM routers ORDER BY org_id, alias")
}

// ListRouters reads one organisation's routers.
func (s *Store) ListRouters(ctx context.Context, orgID string) ([]policy.Router, error) {
	return queryAll(ctx, s.pool, scanRouter,
		routerColumns+" FROM routers WHERE org_id = $1 ORDER BY alias", orgID)
}

// Router reads one router, or ErrNotFound.
func (s *Store) Router(ctx context.Context, orgID, alias string) (policy.Router, error) {
	rt, err := scanRouter(s.pool.QueryRow(ctx,
		routerColumns+" FROM routers WHERE org_id = $1 AND alias = $2", orgID, alias))
	return rt, notFound(err)
}

// UpsertRouter creates or replaces one router.
func (s *Store) UpsertRouter(ctx context.Context, rt policy.Router) (policy.Router, error) {
	// The one place a missing mode gets its default.
	if rt.Mode == "" {
		rt.Mode = policy.RouterModeInstruction
	}
	// Marshalled here because the driver would send a nil map as JSON null,
	// which the column's check constraint refuses.
	own := rt.Ceilings
	if own == nil {
		own = map[string]int{}
	}
	ceilings, err := json.Marshal(own)
	if err != nil {
		return rt, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO routers
		(org_id, alias, mode, model, prompt, destinations, ceilings, fallback, description)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (org_id, alias) DO UPDATE SET mode = EXCLUDED.mode,
			model = EXCLUDED.model, prompt = EXCLUDED.prompt,
			destinations = EXCLUDED.destinations, ceilings = EXCLUDED.ceilings,
			fallback = EXCLUDED.fallback, description = EXCLUDED.description,
			updated_at = now()
		RETURNING created_at, updated_at`,
		rt.OrgID, rt.Alias, rt.Mode, rt.Model, rt.Prompt, rt.Destinations, ceilings,
		nullable(rt.Fallback), rt.Description,
	).Scan(&rt.CreatedAt, &rt.UpdatedAt)
	return rt, err
}

// DeleteRouter removes one router, and takes it off its organisation's
// allow-list, where adding it put it.
func (s *Store) DeleteRouter(ctx context.Context, orgID, alias string) error {
	return s.deleteAllowed(ctx, "routers", orgID, alias)
}

// deleteAllowed deletes a model or router and takes its alias off every
// allow-list in the organisation. Left there, the alias would allow whatever
// is added under it next. A list that named only it allows nothing either
// way. A router named by a project's or key's list is refused before this.
func (s *Store) deleteAllowed(ctx context.Context, table, orgID, alias string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE org_id = $1 AND alias = $2",
			orgID, alias)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE guardrails p
			SET allowed_models = array_remove(p.allowed_models, $2)
			WHERE $2 = ANY (p.allowed_models) AND (
			     (p.scope_type = 'org' AND p.scope_id = $1)
			  OR (p.scope_type = 'project' AND p.scope_id IN (SELECT id FROM projects WHERE org_id = $1))
			  OR (p.scope_type = 'key' AND p.scope_id IN (SELECT id FROM api_keys WHERE org_id = $1)))`,
			orgID, alias)
		return err
	})
}

// RouterUsers lists the guardrails inside one organisation whose allow-list
// names a router. It is read before a deletion, as FilterUsers is: those
// scopes could reach nothing once the router is gone. The organisation's own
// list counts only when it names nothing else, because DeleteRouter takes the
// router off it.
func (s *Store) RouterUsers(ctx context.Context, orgID, alias string) ([]GuardrailRef, error) {
	return s.scopesNaming(ctx, `$2 = ANY (p.allowed_models)
		AND (p.scope_type <> 'org' OR cardinality(p.allowed_models) = 1)`, orgID, alias)
}

// RouterOutcome is what one routing decision came to: the request went where
// the router meant it to, somewhere else, or nowhere.
type RouterOutcome string

const (
	// RouterChose is a decision made and honoured. On a fallback router it is
	// the first destination answering.
	RouterChose RouterOutcome = "chose"
	// RouterFellBack is a decision that could not be made, on a router that
	// has a fallback. On a fallback router it is a later destination answering.
	RouterFellBack RouterOutcome = "fallback"
	// RouterError is a decision that could not be made on a router with no
	// fallback, so the request was refused. On a fallback router it is every
	// destination having failed.
	RouterError RouterOutcome = "error"
)

// RouterDestination is where one router sent its traffic over a window, and
// what happened there. Requests that ended in an error, refused or failed at
// every destination, are left out; they are counted in RouterReport.Errored.
type RouterDestination struct {
	Alias string `json:"alias"`
	Cell
}

// RouterReport is one router's own screen. It shows where traffic went and how
// often the router could not decide, since both failures are otherwise
// invisible: every request still gets an answer.
type RouterReport struct {
	Router string `json:"router"`
	// Total is every request this router was asked to place.
	Total Cell `json:"total"`
	// Destinations is one row per model it placed traffic on, most-used first.
	Destinations []RouterDestination `json:"destinations"`
	// Chose, FellBack and Errored split Total by outcome. A rising fallback
	// rate means the router's model is in trouble.
	Chose    int64 `json:"chose"`
	FellBack int64 `json:"fell_back"`
	Errored  int64 `json:"errored"`
	// DecisionMedianMS and DecisionP95MS are what the decision added to the
	// wait before the first token.
	DecisionMedianMS int64 `json:"decision_median_ms"`
	DecisionP95MS    int64 `json:"decision_p95_ms"`
}

// RouterReportFor reads what one router has been doing over a window.
func (s *Store) RouterReportFor(ctx context.Context, orgID, alias string, from, to time.Time) (
	RouterReport, error,
) {
	rep := RouterReport{Router: alias, Destinations: []RouterDestination{}}

	err := s.pool.QueryRow(ctx, `
		SELECT `+cellColumns+`,
		       `+countOf("router_outcome", RouterChose)+`,
		       `+countOf("router_outcome", RouterFellBack)+`,
		       `+countOf("router_outcome", RouterError)+`,
		       COALESCE(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY router_ms)
		                      FILTER (WHERE router_ms > 0)), 0)::bigint,
		       COALESCE(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY router_ms)
		                      FILTER (WHERE router_ms > 0)), 0)::bigint
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND router = $4`,
		from, to, orgID, alias,
	).Scan(append(cellTargets(&rep.Total),
		&rep.Chose, &rep.FellBack, &rep.Errored,
		&rep.DecisionMedianMS, &rep.DecisionP95MS)...)
	if err != nil {
		return rep, err
	}

	rep.Destinations, err = queryAll(ctx, s.pool, scanRouterDestination, `
		SELECT COALESCE(alias, ''), `+cellColumns+`
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND router = $4
		  AND router_outcome <> '`+string(RouterError)+`'
		GROUP BY 1
		ORDER BY 2 DESC`,
		from, to, orgID, alias)
	return rep, err
}

func scanRouterDestination(r row) (RouterDestination, error) {
	var d RouterDestination
	err := r.Scan(append([]any{&d.Alias}, cellTargets(&d.Cell)...)...)
	return d, err
}

// RouterStat is one router's headline for the list.
type RouterStat struct {
	Requests int64 `json:"requests"`
	Chose    int64 `json:"chose"`
	FellBack int64 `json:"fell_back"`
	Errored  int64 `json:"errored"`
	// CostMicros is what the requests this router placed cost, the router's
	// own generations included. Lowering it is what a router is for.
	CostMicros int64 `json:"cost_micros"`
}

// RouterStats reads every router's headline in one query, for the list.
func (s *Store) RouterStats(ctx context.Context, orgID string, from, to time.Time) (
	map[string]RouterStat, error,
) {
	return queryMap(ctx, s.pool, func(st *RouterStat) []any {
		return []any{&st.Requests, &st.Chose, &st.FellBack, &st.Errored, &st.CostMicros}
	}, `
		SELECT router, count(*),
		       `+countOf("router_outcome", RouterChose)+`,
		       `+countOf("router_outcome", RouterFellBack)+`,
		       `+countOf("router_outcome", RouterError)+`,
		       COALESCE(sum(cost_micros), 0)
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND router IS NOT NULL
		GROUP BY router`, from, to, orgID)
}
