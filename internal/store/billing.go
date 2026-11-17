package store

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// queueBills adds an event's billable calls to the batch, to be written with
// the event, or alone if the event cannot be (see WriteEvents).
func queueBills(batch *pgx.Batch, e Event) {
	for _, b := range e.Bills {
		batch.Queue(`INSERT INTO billing_lines (ts, org_id, alias, provider, backend_model,
			input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
			micros, credit_micros, gateway_fee)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			e.TS, e.OrgID, b.Alias, b.Provider, b.BackendModel,
			b.InputTokens, b.CachedInputTokens, b.CacheWriteTokens, b.OutputTokens,
			b.Micros, b.CreditMicros, b.GatewayFee)
	}
}

// queueCredit takes a batch's billing cost from each organisation's credit.
func queueCredit(batch *pgx.Batch, events []Event) {
	used := map[string]int64{}
	for _, e := range events {
		for _, b := range e.Bills {
			if b.CreditMicros != 0 {
				used[e.OrgID] += b.CreditMicros
			}
		}
	}
	queueDebits(batch, used)
}

// queueDebits takes what each organisation used from its credit, summed per
// organisation so a busy row is locked once per batch, and sorted so replicas
// lock in the same order.
func queueDebits(batch *pgx.Batch, used map[string]int64) {
	for _, org := range slices.Sorted(maps.Keys(used)) {
		batch.Queue(`INSERT INTO credit_accounts (org_id, balance_micros) VALUES ($1, -$2::bigint)
			ON CONFLICT (org_id) DO UPDATE
			SET balance_micros = credit_accounts.balance_micros - $2::bigint`, org, used[org])
	}
}

// BillingRow is one organisation's use of one model, or of one item another
// service billed, in a window.
type BillingRow struct {
	OrgID string `json:"org_id"`
	// OrgName is empty for an organisation deleted since.
	OrgName string `json:"org_name"`
	// Service is empty for the gateway's own rows, and names the service
	// that billed the row otherwise, such as "agents" for Keera Agents.
	Service string `json:"service"`
	// Item, Unit and QuantityMicros are what a service billed and how much,
	// in millionths of the unit. Empty and zero on the gateway's own rows.
	Item           string `json:"item"`
	Unit           string `json:"unit"`
	QuantityMicros int64  `json:"quantity_micros"`
	// Provider is empty for a self-hosted model.
	Provider     string `json:"provider"`
	BackendModel string `json:"backend_model"`
	// GatewayFee says the row is the gateway fee on a model the deployment
	// does not bill otherwise, not a list price.
	GatewayFee bool `json:"gateway_fee"`
	// Calls is 0 on a service's row: its lines are hours, not calls.
	Calls             int64 `json:"calls"`
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	CacheWriteTokens  int64 `json:"cache_write_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	// BilledMicros is what the organisation is billed.
	BilledMicros int64 `json:"billed_micros"`
	// CreditMicros is the part paid from credit.
	CreditMicros int64 `json:"credit_micros"`
}

// Billing sums the billed lines in [from, to), per organisation and model, or
// per organisation, service and item. An organisation's own rows come first.
// An empty orgID reads every organisation.
func (s *Store) Billing(ctx context.Context, orgID string, from, to time.Time) ([]BillingRow, error) {
	return queryAll(ctx, s.pool, scanBillingRow, `
		SELECT b.org_id, coalesce(o.name, ''), b.service, b.item, b.unit,
		       sum(b.quantity_micros), b.provider, b.backend_model, b.gateway_fee,
		       count(*) FILTER (WHERE b.service = ''), sum(b.input_tokens), sum(b.cached_input_tokens),
		       sum(b.cache_write_tokens), sum(b.output_tokens), sum(b.micros),
		       sum(b.credit_micros)
		FROM billing_lines b LEFT JOIN orgs o ON o.id = b.org_id
		WHERE b.ts >= $1 AND b.ts < $2 AND ($3 = '' OR b.org_id = $3)
		GROUP BY b.org_id, o.name, b.service, b.item, b.unit, b.provider, b.backend_model,
		         b.gateway_fee
		ORDER BY coalesce(o.name, ''), b.org_id, b.service, b.gateway_fee, b.provider,
		         b.backend_model, b.item, b.unit`,
		from, to, orgID)
}

func scanBillingRow(r row) (BillingRow, error) {
	var b BillingRow
	err := r.Scan(&b.OrgID, &b.OrgName, &b.Service, &b.Item, &b.Unit, &b.QuantityMicros,
		&b.Provider, &b.BackendModel, &b.GatewayFee,
		&b.Calls, &b.InputTokens, &b.CachedInputTokens, &b.CacheWriteTokens,
		&b.OutputTokens,
		&b.BilledMicros, &b.CreditMicros)
	return b, err
}

// ServiceAgents is the service of the lines Keera Agents bills.
const ServiceAgents = "agents"

// ServiceLine is one line another service bills, such as an hour of one
// organisation's CPU time in Keera Agents.
type ServiceLine struct {
	// ID is the service's own, so a line sent twice is recorded once.
	ID             string    `json:"id"`
	OrgID          string    `json:"org_id"`
	TS             time.Time `json:"ts"`
	Item           string    `json:"item"`
	Unit           string    `json:"unit"`
	QuantityMicros int64     `json:"quantity_micros"`
	Micros         int64     `json:"micros"`
}

// AddServiceLines records lines a service bills, and takes each new one from
// its organisation's credit when credit is true. A line whose ID was recorded
// before is skipped, credit included. It returns how many it added.
func (s *Store) AddServiceLines(ctx context.Context, service string, lines []ServiceLine, credit bool,
) (added int, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		inserts := &pgx.Batch{}
		for _, l := range lines {
			var creditMicros int64
			if credit {
				creditMicros = l.Micros
			}
			inserts.Queue(`INSERT INTO billing_lines (ts, org_id, alias, provider,
				backend_model, micros, credit_micros, service, item, unit, quantity_micros,
				external_id)
				VALUES ($1, $2, '', '', '', $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (service, external_id) WHERE external_id IS NOT NULL DO NOTHING`,
				l.TS, l.OrgID, l.Micros, creditMicros, service, l.Item, l.Unit,
				l.QuantityMicros, l.ID)
		}
		results := tx.SendBatch(ctx, inserts)
		used := map[string]int64{}
		for _, l := range lines {
			tag, err := results.Exec()
			if err != nil {
				_ = results.Close()
				return err
			}
			if tag.RowsAffected() > 0 {
				added++
				if credit && l.Micros > 0 {
					used[l.OrgID] += l.Micros
				}
			}
		}
		if err := results.Close(); err != nil {
			return err
		}
		if len(used) == 0 {
			return nil
		}
		debits := &pgx.Batch{}
		queueDebits(debits, used)
		return tx.SendBatch(ctx, debits).Close()
	})
	return added, err
}
