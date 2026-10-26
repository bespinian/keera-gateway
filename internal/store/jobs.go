package store

import (
	"context"
	"time"
)

// TakeTurn reports whether this replica runs a background job now: yes for
// the first to ask in each interval, so a job costs the same however many
// replicas there are. Every job is safe to run twice, so this only saves work.
// The interval is cut by a tenth, so a ticker that fires early still gets its
// turn.
func (s *Store) TakeTurn(ctx context.Context, job string, every time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO job_runs (job, ran_at) VALUES ($1, now())
		ON CONFLICT (job) DO UPDATE SET ran_at = now()
		WHERE job_runs.ran_at <= now() - make_interval(secs => $2)`,
		job, (every - every/10).Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
