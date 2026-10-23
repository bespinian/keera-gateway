package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Routers, and the report on where their traffic went.
//
// There is no table of runs. The router is stored on the request's own
// usage row, with what its last route step decided: its outcome, and how long
// it took. The destination is that row's alias. Its filters write their runs
// as they would anywhere else.

const routerColumns = `SELECT org_id, alias, display_name, description, steps,
	created_at, updated_at`

func scanRouter(r row) (policy.Router, error) {
	var (
		p     policy.Router
		steps []byte
	)
	err := r.Scan(&p.OrgID, &p.Alias, &p.DisplayName, &p.Description, &steps,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Start = &policy.Step{}
	return p, json.Unmarshal(steps, p.Start)
}

// LoadRouters reads every organisation's routers, for the gateway's
// cache. The gateway checks every request's model against them, so they must
// not be read from Postgres on the request path.
func (s *Store) LoadRouters(ctx context.Context) ([]policy.Router, error) {
	return queryAll(ctx, s.pool, scanRouter, routerColumns+" FROM routers ORDER BY org_id, alias")
}

func (s *Store) ListRouters(ctx context.Context, orgID string) ([]policy.Router, error) {
	return queryAll(ctx, s.pool, scanRouter,
		routerColumns+" FROM routers WHERE org_id = $1 ORDER BY alias", orgID)
}

func (s *Store) Router(ctx context.Context, orgID, alias string) (policy.Router, error) {
	p, err := scanRouter(s.pool.QueryRow(ctx,
		routerColumns+" FROM routers WHERE org_id = $1 AND alias = $2", orgID, alias))
	return p, notFound(err)
}

func (s *Store) UpsertRouter(ctx context.Context, p policy.Router) (policy.Router, error) {
	steps, err := json.Marshal(p.Start)
	if err != nil {
		return p, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO routers
		(org_id, alias, display_name, description, steps)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (org_id, alias) DO UPDATE SET display_name = EXCLUDED.display_name,
			description = EXCLUDED.description, steps = EXCLUDED.steps, updated_at = now()
		RETURNING created_at, updated_at`,
		p.OrgID, p.Alias, p.DisplayName, p.Description, steps,
	).Scan(&p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// DeleteRouter removes one router, and takes it off its organisation's
// allow-list, where adding it put it. Its limits' counts go with it: they are
// kept under the alias and step ids, so a new router of the same alias would
// otherwise start where the old one stopped.
func (s *Store) DeleteRouter(ctx context.Context, orgID, alias string) error {
	return s.deleteAllowed(ctx, "routers", orgID, alias, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM spend
			WHERE scope_type = $1 AND starts_with(scope_id, $2)`,
			string(policy.ScopeRouter), orgID+"/"+alias+"/")
		return err
	})
}

// RouterUsers lists the guardrails inside one organisation whose allow-list
// names a router. It is read before a deletion, as FilterUsers is: those
// scopes could reach nothing once the router is gone. The organisation's own
// list counts only when it names nothing else, because DeleteRouter takes
// the router off it.
func (s *Store) RouterUsers(ctx context.Context, orgID, alias string) ([]GuardrailRef, error) {
	return s.scopesNaming(ctx, `$2 = ANY (p.allowed_models)
		AND (p.scope_type <> 'org' OR cardinality(p.allowed_models) = 1)`, orgID, alias)
}

// RouterDestination is where one router sent its traffic over a window.
// Requests it placed on no model are left out: those a filter or limit step
// refused first, which only Total counts, and those a route step refused or
// failed at every destination, which Errored counts too.
type RouterDestination struct {
	Alias string `json:"alias"`
	Cell
}

// RouterReport is one router's own screen: how much went through it, which
// models answered, and how often its route steps could not decide. Both are
// otherwise invisible, since every request still gets an answer.
type RouterReport struct {
	Router string `json:"router"`
	// Total is every request that named the router.
	Total Cell `json:"total"`
	// Destinations is one row per model it placed traffic on, most-used first.
	Destinations []RouterDestination `json:"destinations"`
	// Chose, FellBack and Errored split the requests a route step placed by
	// outcome. A rising fallback rate means a step's model is in trouble.
	Chose    int64 `json:"chose"`
	FellBack int64 `json:"fell_back"`
	Errored  int64 `json:"errored"`
	// DecisionMedianMS and DecisionP95MS are what deciding added to the wait
	// before the first token.
	DecisionMedianMS int64 `json:"decision_median_ms"`
	DecisionP95MS    int64 `json:"decision_p95_ms"`
}

func (s *Store) RouterReportFor(ctx context.Context, orgID, alias string, from, to time.Time) (
	RouterReport, error,
) {
	rep := RouterReport{Router: alias, Destinations: []RouterDestination{}}
	err := s.pool.QueryRow(ctx, `SELECT `+cellColumns+`,
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
	rep.Destinations, err = queryAll(ctx, s.pool, func(r row) (RouterDestination, error) {
		var d RouterDestination
		err := r.Scan(append([]any{&d.Alias}, cellTargets(&d.Cell)...)...)
		return d, err
	}, `SELECT alias, `+cellColumns+`
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND router = $4
		  AND alias <> ''
		  AND router_outcome IS DISTINCT FROM '`+string(RouterError)+`'
		GROUP BY 1
		ORDER BY 2 DESC`,
		from, to, orgID, alias)
	return rep, err
}

// RouterStat is one router's headline for the list.
type RouterStat struct {
	Cell
	Chose    int64 `json:"chose"`
	FellBack int64 `json:"fell_back"`
	Errored  int64 `json:"errored"`
}

// RouterStats reads every router's headline in one query, for the list.
func (s *Store) RouterStats(ctx context.Context, orgID string, from, to time.Time) (
	map[string]RouterStat, error,
) {
	return queryMap(ctx, s.pool, func(st *RouterStat) []any {
		return append(cellTargets(&st.Cell), &st.Chose, &st.FellBack, &st.Errored)
	}, `
		SELECT router, `+cellColumns+`,
		       `+countOf("router_outcome", RouterChose)+`,
		       `+countOf("router_outcome", RouterFellBack)+`,
		       `+countOf("router_outcome", RouterError)+`
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND router IS NOT NULL
		GROUP BY router`, from, to, orgID)
}

// deleteAllowed deletes a model or router and takes its alias off every
// allow-list in the organisation. Left there, the alias would allow whatever
// is added under it next. A list that named only it allows nothing either
// way. A router named by a project's or key's list is refused before this.
//
// also, when set, removes what else goes with the row, in the same
// transaction.
func (s *Store) deleteAllowed(ctx context.Context, table, orgID, alias string,
	also func(pgx.Tx) error,
) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE org_id = $1 AND alias = $2",
			orgID, alias)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if also != nil {
			if err := also(tx); err != nil {
				return err
			}
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

// RouterOutcome is what one route step's decision came to: the request went
// where the step meant it to, somewhere else, or nowhere.
type RouterOutcome string

const (
	// RouterChose is a decision made and honoured. On a fallback step it is
	// the first destination answering.
	RouterChose RouterOutcome = "chose"
	// RouterFellBack is a decision that could not be made, on a step that
	// has a fallback. On a fallback step it is a later destination answering.
	RouterFellBack RouterOutcome = "fallback"
	// RouterError is a decision that could not be made on a step with no
	// fallback, so the request was refused. On a fallback step it is every
	// destination having failed.
	RouterError RouterOutcome = "error"
)
