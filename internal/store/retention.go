package store

import (
	"context"
	"time"
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
	// A window that began before the cutoff is closed, because the cutoff is
	// in the past.
	tag, err := s.pool.Exec(ctx, "DELETE FROM spend WHERE period_start < $1", before)
	if err != nil {
		return events, err
	}
	return events + tag.RowsAffected(), nil
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
