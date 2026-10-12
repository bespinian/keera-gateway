package store

import (
	"testing"
	"time"
)

func TestTakeTurnGivesOneReplicaEachInterval(t *testing.T) {
	st, ctx := db(t)

	first, err := st.TakeTurn(ctx, "test-job", time.Hour)
	if err != nil {
		t.Fatalf("TakeTurn: %v", err)
	}
	second, err := st.TakeTurn(ctx, "test-job", time.Hour)
	if err != nil {
		t.Fatalf("TakeTurn: %v", err)
	}
	if !first || second {
		t.Errorf("turns = %v, %v; want the first replica to run and the second to skip", first, second)
	}

	other, err := st.TakeTurn(ctx, "other-job", time.Hour)
	if err != nil {
		t.Fatalf("TakeTurn: %v", err)
	}
	if !other {
		t.Error("one job's turn kept another job from running")
	}

	// Once the interval has passed, the next one to ask runs it again.
	if _, err := st.pool.Exec(ctx,
		"UPDATE job_runs SET ran_at = now() - interval '2 hours' WHERE job = 'test-job'"); err != nil {
		t.Fatalf("aging the turn: %v", err)
	}
	again, err := st.TakeTurn(ctx, "test-job", time.Hour)
	if err != nil {
		t.Fatalf("TakeTurn: %v", err)
	}
	if !again {
		t.Error("the job was not run again after its interval")
	}
}
