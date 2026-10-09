package store

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// queueBills adds an event's billable calls to the batch, so they are written
// with the event or not at all.
func queueBills(batch *pgx.Batch, e Event) {
	for _, b := range e.Bills {
		batch.Queue(`INSERT INTO billing_lines (ts, org_id, alias, provider, backend_model,
			currency, input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
			micros, provider_micros, credit_micros)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			e.TS, e.OrgID, b.Alias, b.Provider, b.BackendModel, b.Currency,
			b.InputTokens, b.CachedInputTokens, b.CacheWriteTokens, b.OutputTokens,
			b.Micros, b.ProviderMicros, b.CreditMicros)
	}
}

// queueCredit takes what a batch's billing lines cost from each
// organisation's credit. Summed per organisation, like spend, so a busy
// account row is locked once per batch; and in sorted order, so replicas
// take the locks in the same order.
func queueCredit(batch *pgx.Batch, events []Event) {
	used := map[string]int64{}
	for _, e := range events {
		for _, b := range e.Bills {
			if b.CreditMicros != 0 {
				used[e.OrgID] += b.CreditMicros
			}
		}
	}
	for _, org := range slices.Sorted(maps.Keys(used)) {
		batch.Queue(`INSERT INTO credit_accounts (org_id, balance_micros) VALUES ($1, -$2::bigint)
			ON CONFLICT (org_id) DO UPDATE
			SET balance_micros = credit_accounts.balance_micros - $2::bigint`, org, used[org])
	}
}

// BillingRow is one organisation's use of one provider model in a window.
type BillingRow struct {
	OrgID string `json:"org_id"`
	// OrgName is empty for an organisation deleted since.
	OrgName           string `json:"org_name"`
	Provider          string `json:"provider"`
	BackendModel      string `json:"backend_model"`
	Currency          string `json:"currency"`
	Calls             int64  `json:"calls"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	// Micros is what the organisation is billed.
	Micros int64 `json:"micros"`
	// ProviderMicros is what the provider charges the deployment. The
	// control API shows it only to operators.
	ProviderMicros int64 `json:"-"`
	// CreditMicros is what the use took from the credit, in CHF.
	CreditMicros int64 `json:"credit_micros"`
}

// Billing sums the billable calls in [from, to), per organisation and
// provider model. An empty orgID reads every organisation.
func (s *Store) Billing(ctx context.Context, orgID string, from, to time.Time) ([]BillingRow, error) {
	return queryAll(ctx, s.pool, scanBillingRow, `
		SELECT b.org_id, coalesce(o.name, ''), b.provider, b.backend_model, b.currency,
		       count(*), sum(b.input_tokens), sum(b.cached_input_tokens),
		       sum(b.cache_write_tokens), sum(b.output_tokens), sum(b.micros), sum(b.provider_micros),
		       sum(b.credit_micros)
		FROM billing_lines b LEFT JOIN orgs o ON o.id = b.org_id
		WHERE b.ts >= $1 AND b.ts < $2 AND ($3 = '' OR b.org_id = $3)
		GROUP BY b.org_id, o.name, b.provider, b.backend_model, b.currency
		ORDER BY coalesce(o.name, ''), b.org_id, b.provider, b.backend_model`,
		from, to, orgID)
}

func scanBillingRow(r row) (BillingRow, error) {
	var b BillingRow
	err := r.Scan(&b.OrgID, &b.OrgName, &b.Provider, &b.BackendModel, &b.Currency,
		&b.Calls, &b.InputTokens, &b.CachedInputTokens, &b.CacheWriteTokens,
		&b.OutputTokens,
		&b.Micros, &b.ProviderMicros, &b.CreditMicros)
	return b, err
}
