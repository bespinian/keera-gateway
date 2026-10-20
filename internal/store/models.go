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
	display_name, description, input_micros_per_mtok, output_micros_per_mtok, cached_input_micros_per_mtok,
	cache_write_micros_per_mtok, long_prompt_tokens, long_input_micros_per_mtok,
	long_output_micros_per_mtok, long_cached_input_micros_per_mtok,
	long_cache_write_micros_per_mtok, max_context, coalesce(to_char(release_date, 'YYYY-MM-DD'), ''), location,
	api_key_ct, subscription, enabled`

func scanModel(r row) (policy.Model, error) {
	var (
		m    policy.Model
		long policy.PriceTier
	)
	if err := r.Scan(&m.Alias, &m.OrgID, &m.Kind, &m.Backends, &m.BackendModel, &m.Provider,
		&m.DisplayName, &m.Description, &m.InputMicrosPerMTok, &m.OutputMicrosPerMTok, &m.CachedInputMicrosPerMTok,
		&m.CacheWriteMicrosPerMTok, &long.AboveTokens, &long.InputMicrosPerMTok,
		&long.OutputMicrosPerMTok, &long.CachedInputMicrosPerMTok,
		&long.CacheWriteMicrosPerMTok, &m.MaxContext, &m.ReleaseDate, &m.Location, &m.APIKeyCiphertext, &m.Subscription,
		&m.Enabled); err != nil {
		return policy.Model{}, err
	}
	if long.AboveTokens > 0 {
		m.LongPrompt = &long
	}
	m.HasAPIKey = len(m.APIKeyCiphertext) > 0
	return m, nil
}

func (s *Store) LoadModels(ctx context.Context) ([]policy.Model, error) {
	return queryAll(ctx, s.pool, scanModel, modelColumns+" FROM models ORDER BY org_id, alias")
}

func (s *Store) ListModels(ctx context.Context, orgID string) ([]policy.Model, error) {
	return queryAll(ctx, s.pool, scanModel, modelColumns+" FROM models WHERE org_id = $1 ORDER BY alias", orgID)
}

func (s *Store) Model(ctx context.Context, orgID, alias string) (policy.Model, error) {
	m, err := scanModel(s.pool.QueryRow(ctx,
		modelColumns+" FROM models WHERE org_id = $1 AND alias = $2", orgID, alias))
	return m, notFound(err)
}

// UpsertModel creates or replaces one of an organisation's models, and does
// with its stored credential what cred says.
func (s *Store) UpsertModel(ctx context.Context, m policy.Model, cred Credential) error {
	return upsertModel(ctx, s.pool, m, cred)
}

// Credential says what an upsert does with a row's stored credential. The
// zero value keeps it, so an edit that does not mention one leaves it alone.
//
// It is written in the same statement as the row, so a model or MCP server
// that moves to a new host never sits there with the old host's key.
type Credential struct {
	set    bool
	sealed []byte
}

// ReplaceCredential stores sealed in place of the stored credential. Nil
// clears it.
func ReplaceCredential(sealed []byte) Credential { return Credential{set: true, sealed: sealed} }

// querier is what a pool and a transaction share, so a write can run on its
// own or inside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func upsertModel(ctx context.Context, db querier, m policy.Model, cred Credential) error {
	var long policy.PriceTier
	if m.LongPrompt != nil {
		long = *m.LongPrompt
	}
	_, err := db.Exec(ctx, `INSERT INTO models (alias, org_id, kind, backends, backend_model,
		provider, description, input_micros_per_mtok, output_micros_per_mtok,
		cached_input_micros_per_mtok, cache_write_micros_per_mtok, long_prompt_tokens,
		long_input_micros_per_mtok, long_output_micros_per_mtok,
		long_cached_input_micros_per_mtok, long_cache_write_micros_per_mtok, max_context,
		release_date, location, subscription, enabled, display_name, api_key_ct, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
			NULLIF($18, '')::date, $19,$20,$21,$22,$24, now())
		ON CONFLICT (org_id, alias) DO UPDATE SET kind = EXCLUDED.kind, backends = EXCLUDED.backends,
			backend_model = EXCLUDED.backend_model, provider = EXCLUDED.provider,
			display_name = EXCLUDED.display_name, description = EXCLUDED.description,
			input_micros_per_mtok = EXCLUDED.input_micros_per_mtok,
			output_micros_per_mtok = EXCLUDED.output_micros_per_mtok,
			cached_input_micros_per_mtok = EXCLUDED.cached_input_micros_per_mtok,
			cache_write_micros_per_mtok = EXCLUDED.cache_write_micros_per_mtok,
			long_prompt_tokens = EXCLUDED.long_prompt_tokens,
			long_input_micros_per_mtok = EXCLUDED.long_input_micros_per_mtok,
			long_output_micros_per_mtok = EXCLUDED.long_output_micros_per_mtok,
			long_cached_input_micros_per_mtok = EXCLUDED.long_cached_input_micros_per_mtok,
			long_cache_write_micros_per_mtok = EXCLUDED.long_cache_write_micros_per_mtok,
			max_context = EXCLUDED.max_context, release_date = EXCLUDED.release_date,
			location = EXCLUDED.location, subscription = EXCLUDED.subscription,
			enabled = EXCLUDED.enabled,
			api_key_ct = CASE WHEN $23 THEN EXCLUDED.api_key_ct ELSE models.api_key_ct END,
			updated_at = now()`,
		m.Alias, m.OrgID, string(m.Kind), m.Backends, m.BackendModel, m.Provider, m.Description,
		m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok,
		m.CacheWriteMicrosPerMTok, long.AboveTokens, long.InputMicrosPerMTok,
		long.OutputMicrosPerMTok, long.CachedInputMicrosPerMTok,
		long.CacheWriteMicrosPerMTok, m.MaxContext,
		m.ReleaseDate, m.Location, m.Subscription, m.Enabled, m.DisplayName, cred.set, cred.sealed)
	return err
}

// DeleteModel removes one of an organisation's models, and takes it off the
// organisation's allow-list, where adding it put it.
func (s *Store) DeleteModel(ctx context.Context, orgID, alias string) error {
	return s.deleteAllowed(ctx, "models", orgID, alias)
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
	return queryMap(ctx, s.pool, func(st *ModelStat) []any {
		return []any{&st.TTFTMedianMS, &st.TTFTP95MS}
	}, `
		SELECT alias,
		       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft_ms))::bigint,
		       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms))::bigint
		FROM usage_events
		WHERE ts >= $1 AND ts < $2 AND org_id = $3 AND ttft_ms > 0
		GROUP BY alias`, from, to, orgID)
}
