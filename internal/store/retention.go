package store

import (
	"context"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// purgeBatch bounds one DELETE, so retention does not hold locks on the busy
// usage log for long, and an interrupted run still makes progress.
const purgeBatch = 5000

// PurgeUsage deletes usage events, filter runs and tool calls older than
// before, and the closed spend windows, which nothing reads. It returns how
// many rows went.
func (s *Store) PurgeUsage(ctx context.Context, before time.Time) (int64, error) {
	// Filter runs and tool calls describe the same requests, so they go too.
	var events int64
	for _, table := range []string{"usage_events", "filter_runs", "tool_calls"} {
		n, err := s.purgeBefore(ctx, table, before)
		events += n
		if err != nil {
			return events, err
		}
	}
	// Only windows that ended before the cutoff go. A month that began before
	// it may still be open, and deleting it would reset every monthly budget.
	tag, err := s.pool.Exec(ctx, `DELETE FROM spend
		WHERE (period = 'day' AND period_start < $1)
		   OR (period = 'month' AND period_start < $2)`,
		policy.PeriodDay.Start(before), policy.PeriodMonth.Start(before))
	if err != nil {
		return events, err
	}
	return events + tag.RowsAffected(), nil
}

// PurgeEnded deletes the sandboxes that ended before before, and the keys
// revoked or expired before it. It runs with usage retention, since a
// sandbox's row is itself usage.
//
// A sandbox has ended when it is no longer live (see liveSandbox). A failed
// row has no end time, so the last time it was seen working stands in.
//
// Sandboxes go first, so their keys can go in the same run. A key a sandbox
// still names stays, since that sandbox may be revived.
func (s *Store) PurgeEnded(ctx context.Context, before time.Time) (int64, error) {
	sandboxes, err := inBatches(ctx, func() (int64, error) {
		tag, err := s.pool.Exec(ctx, `DELETE FROM sandboxes WHERE id IN (
			SELECT id FROM sandboxes WHERE NOT `+liveSandbox+`
			  AND COALESCE(terminated_at, GREATEST(created_at, ready_at, active_at)) < $1
			LIMIT $2)`,
			before, purgeBatch)
		return tag.RowsAffected(), err
	})
	if err != nil {
		return sandboxes, err
	}
	keys, err := inBatches(ctx, func() (int64, error) {
		// Guardrails and spend have no foreign key to the key, so they are
		// cleared in the same statement.
		var n int64
		err := s.pool.QueryRow(ctx, `
			WITH gone AS (
				DELETE FROM api_keys WHERE id IN (
					SELECT k.id FROM api_keys k
					WHERE (k.revoked_at < $1 OR k.expires_at < $1)
					  AND NOT EXISTS (SELECT 1 FROM sandboxes sb WHERE sb.key_id = k.id)
					LIMIT $2)
				RETURNING id
			), cleared_guardrails AS (
				DELETE FROM guardrails WHERE scope_type = 'key' AND scope_id IN (SELECT id FROM gone)
			), cleared_spend AS (
				DELETE FROM spend WHERE scope_type = 'key' AND `+scopeOwner+` IN (SELECT id FROM gone)
			)
			SELECT count(*) FROM gone`, before, purgeBatch).Scan(&n)
		return n, err
	})
	return sandboxes + keys, err
}

// PurgeAudit deletes audit entries older than before. It has its own retention
// because compliance, not finance, reads the audit log.
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	return s.purgeBefore(ctx, "audit_log", before)
}

func (s *Store) purgeBefore(ctx context.Context, table string, before time.Time) (int64, error) {
	return inBatches(ctx, func() (int64, error) {
		tag, err := s.pool.Exec(ctx, `DELETE FROM `+table+` WHERE id IN (
			SELECT id FROM `+table+` WHERE ts < $1 ORDER BY ts LIMIT $2)`,
			before, purgeBatch)
		return tag.RowsAffected(), err
	})
}

// inBatches runs batch, which deletes at most purgeBatch rows, until a batch
// comes back short. On error it still reports how many rows went.
func inBatches(ctx context.Context, batch func() (int64, error)) (int64, error) {
	var total int64
	for {
		n, err := batch()
		if err != nil {
			return total, err
		}
		total += n
		if n < purgeBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
