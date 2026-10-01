package store

import (
	"context"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// purgeBatch bounds one DELETE. Every replica writes to the usage log all the
// time, so retention deletes a few thousand rows at a time instead of holding
// locks for one huge delete. An interrupted run still makes progress.
const purgeBatch = 5000

// PurgeUsage deletes usage events, filter runs and tool calls older than
// before, and the closed spend windows that go with them. Nothing reads a
// closed window: budgets check the open day and month, and reports read the
// events.
//
// It returns how many rows went, so an operator can see retention working.
func (s *Store) PurgeUsage(ctx context.Context, before time.Time) (int64, error) {
	events, err := s.purgeBefore(ctx, "usage_events", before)
	if err != nil {
		return events, err
	}
	// Filter runs describe the same requests, so they are kept no longer.
	runs, err := s.purgeBefore(ctx, "filter_runs", before)
	events += runs
	if err != nil {
		return events, err
	}
	// Tool calls are the same log's other half.
	calls, err := s.purgeBefore(ctx, "tool_calls", before)
	events += calls
	if err != nil {
		return events, err
	}
	// Only windows that ended before the cutoff go. A month that began before
	// it may still be open: with seven days' retention, the current month
	// started before the cutoff from the 8th on, and deleting it would reset
	// every monthly budget.
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
// revoked or expired before it. It runs with usage retention: by then the
// usage they would name is gone, and a sandbox's own row is its usage, read by
// creation time, which came earlier still.
//
// A sandbox has ended when it is no longer live: terminated, or a failed
// agent sandbox. Live ones stay however old they are. An expired one can be
// revived, and a failed engineer one keeps its volume until somebody
// terminates it. A failed row has no end time, so the last time it was seen
// working stands in for one.
//
// Sandboxes go first, so that their keys are free to go in the same run. A
// key that a sandbox still names stays: that sandbox may be revived, and its
// row says whose it was.
func (s *Store) PurgeEnded(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM sandboxes WHERE id IN (
			SELECT id FROM sandboxes WHERE NOT `+liveSandbox+`
			  AND COALESCE(terminated_at, GREATEST(created_at, ready_at, active_at)) < $1
			LIMIT $2)`,
			before, purgeBatch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < purgeBatch {
			break
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
	for {
		// Guardrails and spend name a key by plain id with no foreign key, so
		// they are cleared in the same statement.
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
				DELETE FROM spend WHERE scope_type = 'key' AND scope_id IN (SELECT id FROM gone)
			)
			SELECT count(*) FROM gone`, before, purgeBatch).Scan(&n)
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

// PurgeAudit deletes audit entries older than before.
//
// It is set apart from usage retention because the two serve different
// readers: finance for usage, compliance for the audit log.
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	return s.purgeBefore(ctx, "audit_log", before)
}

// purgeBefore deletes a table's rows older than before, one batch at a time,
// until a batch comes back short. On error it still reports how many rows it
// deleted, so the numbers stay readable.
func (s *Store) purgeBefore(ctx context.Context, table string, before time.Time) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM `+table+` WHERE id IN (
			SELECT id FROM `+table+` WHERE ts < $1 ORDER BY ts LIMIT $2)`,
			before, purgeBatch)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n < purgeBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
