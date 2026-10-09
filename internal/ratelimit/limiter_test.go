package ratelimit

import (
	"testing"
	"time"
)

func TestAllowSpendsTheBurstThenRefills(t *testing.T) {
	l := New()
	now := time.Now()

	// A bucket starts full at one minute's worth.
	for i := range 60 {
		if !l.Allow("k", 60, now) {
			t.Fatalf("request %d was refused while the bucket should still be full", i)
		}
	}
	if l.Allow("k", 60, now) {
		t.Error("the bucket was not exhausted after its whole burst was spent")
	}

	// 60 per minute is one per second.
	if !l.Allow("k", 60, now.Add(time.Second)) {
		t.Error("the bucket did not refill after a second")
	}
}

func TestUnlimitedWhenNoRateIsSet(t *testing.T) {
	l := New()
	now := time.Now()
	for range 1000 {
		if !l.Allow("k", 0, now) {
			t.Fatal("a zero rate must mean unlimited, not blocked")
		}
	}
}

// limiter is what the in-memory and the Redis limiters both are.
type limiter interface {
	Admit(reqs []Requirement, now time.Time) int
	ChargeAll(reqs []Requirement, n float64, now time.Time)
	Remaining(key string, perMinute int, now time.Time) int
	Retry(key string, perMinute int, now time.Time) time.Duration
}

func TestLimiterContract(t *testing.T) {
	testContract(t, func(*testing.T) limiter { return New() })
}

// testContract is the behaviour both limiters keep: a deployment switching
// Redis on must not find its limits behaving differently. newLimiter gives an
// empty limiter.
func testContract(t *testing.T, newLimiter func(t *testing.T) limiter) {
	t.Run("Admit charges every level or none", func(t *testing.T) {
		// The org allows plenty and the project almost nothing. Requests
		// refused by the project must not spend the org's allowance: the org
		// is what protects the inference plane from every other project.
		l := newLimiter(t)
		now := time.Now()
		reqs := []Requirement{
			{Key: "org:o1|rpm", PerMinute: 600, Take: true},
			{Key: "project:t1|rpm", PerMinute: 2, Take: true},
		}
		for i := range 2 {
			if got := l.Admit(reqs, now); got != -1 {
				t.Fatalf("request %d: Admit = %d, want -1", i, got)
			}
		}
		for range 50 {
			if got := l.Admit(reqs, now); got != 1 {
				t.Fatalf("Admit = %d, want 1 - the project's limit is what binds", got)
			}
		}
		// Two admitted requests, so two units gone from the org and no more.
		if got := l.Remaining("org:o1|rpm", 600, now); got != 598 {
			t.Errorf("the org has %d of 600 left, want 598: it was charged for "+
				"requests the project refused", got)
		}
	})

	t.Run("Admit reports the outermost limit that binds", func(t *testing.T) {
		l := newLimiter(t)
		now := time.Now()
		reqs := []Requirement{
			{Key: "org|rpm", PerMinute: 1, Take: true},
			{Key: "org|tpm", PerMinute: 100},
			{Key: "key|rpm", PerMinute: 1, Take: true},
		}
		if got := l.Admit(reqs, now); got != -1 {
			t.Fatalf("Admit = %d, want -1", got)
		}
		if got := l.Admit(reqs, now); got != 0 {
			t.Errorf("Admit = %d, want 0 - the org is checked before the key", got)
		}

		// A token bucket in debt refuses at its own position, and the request
		// limits before it are not charged for the attempt.
		l = newLimiter(t)
		charge(l, "org|tpm", 100, 500, now)
		if got := l.Admit(reqs, now); got != 1 {
			t.Errorf("Admit = %d, want 1 - the token limit is what binds", got)
		}
		if got := l.Remaining("org|rpm", 1, now); got != 1 {
			t.Errorf("the request bucket holds %d, want 1: a request refused by the "+
				"token limit must cost nothing", got)
		}

		// The index is an index into the caller's requirements, including
		// the unlimited ones, which Redis is never sent.
		l = newLimiter(t)
		reqs = []Requirement{
			{Key: "org|tpm", PerMinute: 0},
			{Key: "org|rpm", PerMinute: 1, Take: true},
			{Key: "key|rpm", PerMinute: 1, Take: true},
		}
		if got := l.Admit(reqs, now); got != -1 {
			t.Fatalf("Admit = %d, want -1", got)
		}
		if got := l.Admit(reqs, now); got != 1 {
			t.Errorf("Admit = %d, want 1 - the org is checked before the key, and the "+
				"unlimited requirement still occupies index 0", got)
		}
	})

	t.Run("a charge can overdraw, so one huge request is paid for later", func(t *testing.T) {
		// Token limits are charged after the fact, because the true cost is
		// not known until the completion finishes. One very large request
		// should therefore hold back the ones after it.
		l := newLimiter(t)
		now := time.Now()
		// The token-per-minute question: is the bucket still positive, taking
		// nothing from it.
		inCredit := func(at time.Time) bool {
			return l.Admit([]Requirement{{Key: "k", PerMinute: 1000}}, at) < 0
		}
		if !inCredit(now) {
			t.Fatal("a fresh bucket should admit a request")
		}
		charge(l, "k", 1000, 5000, now)
		if inCredit(now) {
			t.Error("the bucket is overdrawn and should refuse")
		}
		// 1000/min is ~16.7/s, so 4000 tokens of debt needs about four minutes.
		if inCredit(now.Add(2 * time.Minute)) {
			t.Error("the debt was forgiven too early")
		}
		if !inCredit(now.Add(5 * time.Minute)) {
			t.Error("the debt was never repaid")
		}
	})

	t.Run("Retry reports when the next request fits", func(t *testing.T) {
		l := newLimiter(t)
		now := time.Now()
		reqs := []Requirement{{Key: "k", PerMinute: 60, Take: true}}
		l.Admit(reqs, now)
		charge(l, "k", 60, 60, now) // drain it
		if got := l.Admit(reqs, now); got != 0 {
			t.Fatalf("Admit = %d, want 0 - the burst should be spent", got)
		}

		d := l.Retry("k", 60, now)
		if d <= 0 {
			t.Fatalf("Retry = %v, want a positive wait", d)
		}
		if got := l.Admit(reqs, now.Add(d+time.Millisecond)); got != -1 {
			t.Errorf("waiting the advertised %v was not enough", d)
		}
		if got := l.Retry("other", 60, now); got != 0 {
			t.Errorf("an untouched bucket should need no wait, got %v", got)
		}
	})
}

func TestChangingTheConfiguredRateRetunesTheBucket(t *testing.T) {
	// An administrator lowering a limit must take effect without a restart.
	l := New()
	now := time.Now()
	for range 100 {
		l.Allow("k", 600, now)
	}
	// Now the guardrail drops to 10/min. The bucket must not still be holding the
	// larger burst it was allowed a moment ago.
	admitted := 0
	for range 50 {
		if l.Allow("k", 10, now) {
			admitted++
		}
	}
	if admitted > 10 {
		t.Errorf("admitted %d requests after the limit dropped to 10/min", admitted)
	}
}

func TestSweepForgetsIdleBuckets(t *testing.T) {
	l := New()
	now := time.Now()
	l.Allow("k", 60, now)
	l.Sweep(time.Minute, now.Add(2*time.Minute))
	if len(l.b) != 0 {
		t.Errorf("%d buckets survived the sweep, want 0", len(l.b))
	}
}

func TestConcurrentUse(_ *testing.T) {
	// Run with -race: the limiter is on the hot path of every request.
	l := New()
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 200 {
				now := time.Now()
				l.Allow("shared", 100000, now)
				l.Admit([]Requirement{
					{Key: "shared", PerMinute: 100000, Take: true},
					{Key: "shared-tokens", PerMinute: 100000},
				}, now)
				charge(l, "shared-tokens", 100000, 3, now)
			}
		}()
	}
	for range 8 {
		<-done
	}
}

func TestSweepKeepsABucketThatWasJustUsed(t *testing.T) {
	// A bucket created and then used once must carry the time it was made.
	// Left at the zero time it looks idle since year one, and the next sweep
	// forgets a limit that is still being enforced.
	l := New()
	now := time.Now()
	l.Allow("k", 60, now)
	l.Sweep(10*time.Minute, now.Add(time.Minute))
	if len(l.b) != 1 {
		t.Errorf("%d buckets survived the sweep, want 1", len(l.b))
	}
}

func TestAdmitTakesNothingFromAnUnlimitedRequirement(t *testing.T) {
	l := New()
	now := time.Now()
	reqs := []Requirement{{Key: "k", PerMinute: 0, Take: true}}
	for range 100 {
		if got := l.Admit(reqs, now); got != -1 {
			t.Fatalf("Admit = %d, want -1: a zero rate is unlimited", got)
		}
	}
	if len(l.b) != 0 {
		t.Errorf("%d buckets were created for an unlimited requirement, want 0", len(l.b))
	}
}

func TestChargeAllChargesEveryLimitedBucket(t *testing.T) {
	l := New()
	now := time.Now()
	l.ChargeAll([]Requirement{
		{Key: "org|tpm", PerMinute: 100},
		{Key: "key|tpm", PerMinute: 50},
		{Key: "project|tpm"}, // unlimited, so not charged
	}, 30, now)

	if got := l.Remaining("org|tpm", 100, now); got != 70 {
		t.Errorf("org has %d left, want 70", got)
	}
	if got := l.Remaining("key|tpm", 50, now); got != 20 {
		t.Errorf("key has %d left, want 20", got)
	}
	if got := l.Remaining("project|tpm", 0, now); got != -1 {
		t.Errorf("Remaining = %d for an unlimited bucket, want -1", got)
	}
}
