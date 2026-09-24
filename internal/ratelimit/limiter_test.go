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

// inCredit is the token-per-minute question: is this bucket still positive,
// taking nothing from it. It is one requirement without Take.
func inCredit(l *Limiter, key string, perMinute int, now time.Time) bool {
	return l.Admit([]Requirement{{Key: key, PerMinute: perMinute}}, now) < 0
}

func TestChargeCanOverdrawSoOneHugeRequestIsPaidForLater(t *testing.T) {
	// Token limits are charged after the fact, because the true cost is not
	// known until the completion finishes. One very large request should
	// therefore hold back the ones after it.
	l := New()
	now := time.Now()

	if !inCredit(l, "k", 1000, now) {
		t.Fatal("a fresh bucket should admit a request")
	}
	l.Charge("k", 1000, 5000, now)
	if inCredit(l, "k", 1000, now) {
		t.Error("the bucket is overdrawn and should refuse")
	}
	// 1000/min is ~16.7/s, so 4000 tokens of debt needs about four minutes.
	if inCredit(l, "k", 1000, now.Add(2*time.Minute)) {
		t.Error("the debt was forgiven too early")
	}
	if !inCredit(l, "k", 1000, now.Add(5*time.Minute)) {
		t.Error("the debt was never repaid")
	}
}

func TestRetryReportsWhenTheNextRequestFits(t *testing.T) {
	l := New()
	now := time.Now()
	l.Allow("k", 60, now)
	l.Charge("k", 60, 60, now) // drain it

	d := l.Retry("k", 60, now)
	if d <= 0 {
		t.Fatalf("Retry = %v, want a positive wait", d)
	}
	if !l.Allow("k", 60, now.Add(d+time.Millisecond)) {
		t.Errorf("waiting the advertised %v was not enough", d)
	}
	if got := l.Retry("other", 60, now); got != 0 {
		t.Errorf("an untouched bucket should need no wait, got %v", got)
	}
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
				l.Charge("shared-tokens", 100000, 3, now)
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

func TestAdmitChargesEveryLevelOrNone(t *testing.T) {
	// The org allows plenty and the team almost nothing. Requests refused by
	// the team must not spend the org's allowance: the org is what protects the
	// inference plane from every other team.
	l := New()
	now := time.Now()
	reqs := []Requirement{
		{Key: "org:o1|rpm", PerMinute: 600, Take: true},
		{Key: "team:t1|rpm", PerMinute: 2, Take: true},
	}

	for i := range 2 {
		if got := l.Admit(reqs, now); got != -1 {
			t.Fatalf("request %d: Admit = %d, want -1", i, got)
		}
	}
	for range 50 {
		if got := l.Admit(reqs, now); got != 1 {
			t.Fatalf("Admit = %d, want 1 - the team's limit is what binds", got)
		}
	}

	// Two admitted requests, so two units gone from the org and no more.
	if got := l.Remaining("org:o1|rpm", 600, now); got != 598 {
		t.Errorf("the org has %d of 600 left, want 598: it was charged for "+
			"requests the team refused", got)
	}
}

func TestAdmitReportsTheOutermostLimitThatBinds(t *testing.T) {
	l := New()
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
	l2 := New()
	l2.Charge("org|tpm", 100, 500, now)
	if got := l2.Admit(reqs, now); got != 1 {
		t.Errorf("Admit = %d, want 1 - the token limit is what binds", got)
	}
	if got := l2.Remaining("org|rpm", 1, now); got != 1 {
		t.Errorf("the request bucket holds %d, want 1: a request refused by the "+
			"token limit must cost nothing", got)
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
		{Key: "team|tpm"}, // unlimited, so not charged
	}, 30, now)

	if got := l.Remaining("org|tpm", 100, now); got != 70 {
		t.Errorf("org has %d left, want 70", got)
	}
	if got := l.Remaining("key|tpm", 50, now); got != 20 {
		t.Errorf("key has %d left, want 20", got)
	}
	if got := l.Remaining("team|tpm", 0, now); got != -1 {
		t.Errorf("Remaining = %d for an unlimited bucket, want -1", got)
	}
}
