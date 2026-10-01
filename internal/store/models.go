package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The release date is read as text, so an unstated one is simply empty.
const modelColumns = `SELECT alias, org_id, kind, backends, backend_model, provider,
	description, input_micros_per_mtok, output_micros_per_mtok, cached_input_micros_per_mtok,
	max_context, coalesce(to_char(release_date, 'YYYY-MM-DD'), ''), location,
	api_key_ct, subscription, enabled`

func scanModel(r row) (policy.Model, error) {
	var m policy.Model
	if err := r.Scan(&m.Alias, &m.OrgID, &m.Kind, &m.Backends, &m.BackendModel, &m.Provider,
		&m.Description, &m.InputMicrosPerMTok, &m.OutputMicrosPerMTok, &m.CachedInputMicrosPerMTok,
		&m.MaxContext, &m.ReleaseDate, &m.Location, &m.APIKeyCiphertext, &m.Subscription,
		&m.Enabled); err != nil {
		return policy.Model{}, err
	}
	m.HasAPIKey = len(m.APIKeyCiphertext) > 0
	return m, nil
}

// LoadModels reads every organisation's models.
func (s *Store) LoadModels(ctx context.Context) ([]policy.Model, error) {
	return s.queryModels(ctx, modelColumns+" FROM models ORDER BY org_id, alias")
}

// ListModels reads one organisation's models.
func (s *Store) ListModels(ctx context.Context, orgID string) ([]policy.Model, error) {
	return s.queryModels(ctx, modelColumns+" FROM models WHERE org_id = $1 ORDER BY alias", orgID)
}

func (s *Store) queryModels(ctx context.Context, sql string, args ...any) ([]policy.Model, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanModel)
}

// Model reads one of an organisation's models, or ErrNotFound.
func (s *Store) Model(ctx context.Context, orgID, alias string) (policy.Model, error) {
	m, err := scanModel(s.pool.QueryRow(ctx,
		modelColumns+" FROM models WHERE org_id = $1 AND alias = $2", orgID, alias))
	if err != nil {
		return policy.Model{}, notFound(err)
	}
	return m, nil
}

// UpsertModel creates or replaces one of an organisation's models.
//
// It leaves api_key_ct alone: a credential is set on its own, with
// SetModelCredential, so an edit that does not mention one keeps it.
func (s *Store) UpsertModel(ctx context.Context, m policy.Model) error {
	return upsertModel(ctx, s.pool, m)
}

// querier is what a pool and a transaction share, so a write can run on its
// own or inside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func upsertModel(ctx context.Context, db querier, m policy.Model) error {
	_, err := db.Exec(ctx, `INSERT INTO models (alias, org_id, kind, backends, backend_model,
		provider, description, input_micros_per_mtok, output_micros_per_mtok,
		cached_input_micros_per_mtok, max_context, release_date, location,
		subscription, enabled, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12, '')::date,$13,$14,$15, now())
		ON CONFLICT (org_id, alias) DO UPDATE SET kind = EXCLUDED.kind, backends = EXCLUDED.backends,
			backend_model = EXCLUDED.backend_model, provider = EXCLUDED.provider,
			description = EXCLUDED.description,
			input_micros_per_mtok = EXCLUDED.input_micros_per_mtok,
			output_micros_per_mtok = EXCLUDED.output_micros_per_mtok,
			cached_input_micros_per_mtok = EXCLUDED.cached_input_micros_per_mtok,
			max_context = EXCLUDED.max_context, release_date = EXCLUDED.release_date,
			location = EXCLUDED.location, subscription = EXCLUDED.subscription,
			enabled = EXCLUDED.enabled, updated_at = now()`,
		m.Alias, m.OrgID, string(m.Kind), m.Backends, m.BackendModel, m.Provider, m.Description,
		m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok, m.MaxContext,
		m.ReleaseDate, m.Location, m.Subscription, m.Enabled)
	return err
}

// SetModelCredential stores the sealed credential for one model. Nil clears
// it, so a key can be removed without removing the model.
func (s *Store) SetModelCredential(ctx context.Context, orgID, alias string, sealed []byte) error {
	return s.execOne(ctx,
		"UPDATE models SET api_key_ct = $3, updated_at = now() WHERE org_id = $1 AND alias = $2",
		orgID, alias, sealed)
}

// DeleteModel removes one of an organisation's models.
func (s *Store) DeleteModel(ctx context.Context, orgID, alias string) error {
	return s.execOne(ctx, "DELETE FROM models WHERE org_id = $1 AND alias = $2", orgID, alias)
}

// ModelStat is how fast one model has been answering, for the list.
type ModelStat struct {
	TTFTMedianMS int64 `json:"ttft_median_ms"`
	TTFTP95MS    int64 `json:"ttft_p95_ms"`
}

// ModelStats reads every model's time to first token in one query. It counts
// the same requests as the model's own screen, so the two agree.
func (s *Store) ModelStats(ctx context.Context, orgID string, from, to time.Time) (
	map[string]ModelStat, error,
) {
	rows, err := s.pool.Query(ctx, `
		SELECT alias,
		       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms))::bigint,
		       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms))::bigint
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND ttft_ms > 0
		GROUP BY alias`, from, to, orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]ModelStat{}
	var (
		alias string
		st    ModelStat
	)
	_, err = pgx.ForEachRow(rows, []any{&alias, &st.TTFTMedianMS, &st.TTFTP95MS},
		func() error {
			out[alias] = st
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
