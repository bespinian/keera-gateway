package store

import (
	"context"

	"github.com/bespinian/keera-gateway/internal/policy"
)

const modelColumns = `SELECT alias, kind, backends, backend_model, provider, description,
	input_micros_per_mtok, output_micros_per_mtok, cached_input_micros_per_mtok, max_context,
	api_key_env, api_key_ct, enabled, managed`

func scanModel(r row) (policy.Model, error) {
	var m policy.Model
	if err := r.Scan(&m.Alias, &m.Kind, &m.Backends, &m.BackendModel, &m.Provider, &m.Description,
		&m.InputMicrosPerMTok, &m.OutputMicrosPerMTok, &m.CachedInputMicrosPerMTok, &m.MaxContext,
		&m.APIKeyEnv, &m.APIKeyCiphertext, &m.Enabled, &m.Managed); err != nil {
		return policy.Model{}, err
	}
	m.HasAPIKey = len(m.APIKeyCiphertext) > 0
	return m, nil
}

// LoadModels reads the whole model catalogue. An empty catalogue is nil, not
// an empty slice.
func (s *Store) LoadModels(ctx context.Context) ([]policy.Model, error) {
	rows, err := s.pool.Query(ctx, modelColumns+" FROM models ORDER BY alias")
	if err != nil {
		return nil, err
	}
	models, err := collect(rows, scanModel)
	if err != nil || len(models) == 0 {
		return nil, err
	}
	return models, nil
}

// Model reads one catalogue entry, or ErrNotFound.
func (s *Store) Model(ctx context.Context, alias string) (policy.Model, error) {
	m, err := scanModel(s.pool.QueryRow(ctx, modelColumns+" FROM models WHERE alias = $1", alias))
	if err != nil {
		return policy.Model{}, notFound(err)
	}
	return m, nil
}

// UpsertModel creates or replaces one catalogue entry.
//
// It leaves api_key_ct alone. The catalogue file is applied on every start and
// has no credentials, so writing the column here would erase a key an operator
// set in the panel.
func (s *Store) UpsertModel(ctx context.Context, m policy.Model) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO models (alias, kind, backends, backend_model,
		provider, description, input_micros_per_mtok, output_micros_per_mtok,
		cached_input_micros_per_mtok, max_context, api_key_env, enabled, managed, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now())
		ON CONFLICT (alias) DO UPDATE SET kind = EXCLUDED.kind, backends = EXCLUDED.backends,
			backend_model = EXCLUDED.backend_model, provider = EXCLUDED.provider,
			description = EXCLUDED.description,
			input_micros_per_mtok = EXCLUDED.input_micros_per_mtok,
			output_micros_per_mtok = EXCLUDED.output_micros_per_mtok,
			cached_input_micros_per_mtok = EXCLUDED.cached_input_micros_per_mtok,
			max_context = EXCLUDED.max_context, api_key_env = EXCLUDED.api_key_env,
			enabled = EXCLUDED.enabled, managed = EXCLUDED.managed, updated_at = now()`,
		m.Alias, string(m.Kind), m.Backends, m.BackendModel, m.Provider, m.Description,
		m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok, m.MaxContext,
		m.APIKeyEnv, m.Enabled, m.Managed)
	return err
}

// UnmanageModels hands back to the operator every file-managed model that the
// catalogue file no longer names. The row is kept, not deleted: clients may
// still call the model by name.
func (s *Store) UnmanageModels(ctx context.Context, except []string) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE models SET managed = false, updated_at = now() WHERE managed AND alias <> ALL($1)",
		except)
	return err
}

// SetModelCredential stores the sealed credential for one model. Nil clears
// it, so an operator can remove a key without removing the model.
func (s *Store) SetModelCredential(ctx context.Context, alias string, sealed []byte) error {
	return s.execOne(ctx,
		"UPDATE models SET api_key_ct = $2, updated_at = now() WHERE alias = $1",
		alias, sealed)
}

// DeleteModel removes one catalogue entry.
func (s *Store) DeleteModel(ctx context.Context, alias string) error {
	return s.execOne(ctx, "DELETE FROM models WHERE alias = $1", alias)
}
