package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Tests that need no Redis: what happens when it cannot be reached. The
// gateway keeps serving with the per-replica buckets.

// deadScripter answers every script with an error, like a Redis that is down
// or too slow.
type deadScripter struct {
	err   error
	calls int
}

// charge takes n units from one bucket.
func charge(c interface {
	ChargeAll([]Requirement, float64, time.Time)
}, key string, perMinute int, n float64, now time.Time,
) {
	c.ChargeAll([]Requirement{{Key: key, PerMinute: perMinute}}, n, now)
}

func (d *deadScripter) cmd(ctx context.Context) *redis.Cmd {
	d.calls++
	c := redis.NewCmd(ctx)
	c.SetErr(d.err)
	return c
}

func (d *deadScripter) Eval(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return d.cmd(ctx)
}

func (d *deadScripter) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return d.cmd(ctx)
}

func (d *deadScripter) EvalRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return d.cmd(ctx)
}

func (d *deadScripter) EvalShaRO(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	return d.cmd(ctx)
}

func (d *deadScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	c := redis.NewBoolSliceCmd(ctx)
	c.SetErr(d.err)
	return c
}

func (d *deadScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	c := redis.NewStringCmd(ctx)
	c.SetErr(d.err)
	return c
}

// counter is the metrics registry's side of the fallback, without the registry.
type counter struct{ n int }

func (c *counter) RateLimitFallback() { c.n++ }

func TestRedisFallsBackToTheLocalBucketsWhenItCannotBeReached(t *testing.T) {
	// A Redis outage only costs the coordination: limits bind per replica.
	seen := &counter{}
	r := NewRedis(&deadScripter{err: errors.New("dial tcp: connection refused")},
		New(), RedisOptions{Metrics: seen}, nil)
	now := time.Now()
	reqs := []Requirement{{Key: "org|rpm", PerMinute: 5, Take: true}}

	for i := range 5 {
		if got := r.Admit(reqs, now); got != -1 {
			t.Fatalf("request %d: Admit = %d, want -1 - the local buckets must still admit", i, got)
		}
	}
	if got := r.Admit(reqs, now); got != 0 {
		t.Errorf("Admit = %d, want 0: with Redis gone the limit still has to bind locally", got)
	}
	if seen.n != 6 {
		t.Errorf("counted %d fallbacks, want 6 - an operator cannot see a degraded "+
			"ceiling that is not counted", seen.n)
	}
}

func TestRedisIsNotTriedAgainRightAfterItFailed(t *testing.T) {
	// After one failure the limiter decides locally for a while, so requests
	// do not each wait out the timeout.
	dead := &deadScripter{err: errors.New("i/o timeout")}
	seen := &counter{}
	r := NewRedis(dead, New(), RedisOptions{Metrics: seen}, nil)
	now := time.Now()
	reqs := []Requirement{{Key: "org|rpm", PerMinute: 100, Take: true}}

	for range 5 {
		r.Admit(reqs, now)
	}
	r.ChargeAll([]Requirement{{Key: "org|tpm", PerMinute: 1000}}, 10, now)
	if dead.calls != 1 {
		t.Errorf("Redis was tried %d times, want once before resting", dead.calls)
	}
	if seen.n != 6 {
		t.Errorf("counted %d fallbacks, want 6: a decision made locally counts either way", seen.n)
	}

	r.mu.Lock()
	r.resting = time.Time{}
	r.mu.Unlock()
	r.Admit(reqs, now)
	if dead.calls != 2 {
		t.Errorf("Redis was tried %d times, want a second try once the rest is over", dead.calls)
	}
}

func TestRedisFallsBackForChargeAndTheHeadersFollow(t *testing.T) {
	// A charge made locally must show in the headers too.
	r := NewRedis(&deadScripter{err: errors.New("i/o timeout")}, New(), RedisOptions{}, nil)
	now := time.Now()

	charge(r, "org|tpm", 1000, 5000, now)
	if got := r.Remaining("org|tpm", 1000, now); got != 0 {
		t.Errorf("Remaining = %d, want 0: the bucket is overdrawn locally", got)
	}
	if got := r.Retry("org|tpm", 1000, now); got <= 0 {
		t.Errorf("Retry = %v, want a positive wait", got)
	}
}

func TestRedisAnswersAnUnknownKeyFromTheLocalBuckets(t *testing.T) {
	// A key no decision has touched is answered from the local buckets.
	r := NewRedis(&deadScripter{err: errors.New("down")}, New(), RedisOptions{}, nil)
	now := time.Now()
	if got := r.Remaining("never-seen|rpm", 60, now); got != 60 {
		t.Errorf("Remaining = %d, want 60", got)
	}
	if got := r.Remaining("never-seen|rpm", 0, now); got != -1 {
		t.Errorf("Remaining = %d, want -1 for an unlimited bucket", got)
	}
}

// Tests against a real Redis, for the Lua scripts.

// redisFor returns a client, and a limiter whose keys are unique to this test.
func redisFor(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv("KEERA_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set KEERA_TEST_REDIS_URL to run the Redis rate-limiting tests (make test-integration)")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("KEERA_TEST_REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	ctx := t.Context()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	prefix := fmt.Sprintf("keeratest-%d-%d", os.Getpid(), rand.Uint32())
	t.Cleanup(func() {
		keys, err := rdb.Keys(context.Background(), prefix+":*").Result()
		if err == nil && len(keys) > 0 {
			_ = rdb.Del(context.Background(), keys...).Err()
		}
		_ = rdb.Close()
	})
	return rdb, prefix
}

// replica is one gateway process: its own in-memory fallback, the same Redis.
func replica(t *testing.T, rdb *redis.Client, prefix string) *Redis {
	t.Helper()
	return NewRedis(rdb, New(), RedisOptions{Prefix: prefix, Timeout: 5 * time.Second}, nil)
}

func TestRedisKeepsTheLimiterContract(t *testing.T) {
	rdb, _ := redisFor(t)
	testContract(t, func(t *testing.T) limiter {
		_, prefix := redisFor(t)
		return replica(t, rdb, prefix)
	})
}

func TestRedisSpendsOneAllowanceAcrossReplicas(t *testing.T) {
	// Two gateways, one limit of 10: ten requests between them, not each.
	rdb, prefix := redisFor(t)
	a, b := replica(t, rdb, prefix), replica(t, rdb, prefix)
	now := time.Now()
	reqs := []Requirement{{Key: "org:o1|rpm", PerMinute: 10, Take: true}}

	admitted := 0
	for i := range 40 {
		r := a
		if i%2 == 1 {
			r = b
		}
		if r.Admit(reqs, now) == -1 {
			admitted++
		}
	}
	if admitted != 10 {
		t.Errorf("admitted %d requests against a limit of 10 per minute; two replicas "+
			"sharing a Redis must share the ceiling", admitted)
	}

	// And it refills at the configured rate, not at twice it.
	if a.Admit(reqs, now.Add(6*time.Second)) != -1 {
		t.Error("the bucket did not refill after a tenth of a minute")
	}
	if b.Admit(reqs, now.Add(6*time.Second)) == -1 {
		t.Error("six seconds bought a second request; the refill is being applied per replica")
	}
}

func TestRedisAnEarlierClockDoesNotRefillTheBucket(t *testing.T) {
	// A long stream is charged late, and clocks differ. Storing an earlier
	// time would let the next request refill the debt away.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	now := time.Now()
	tpm := []Requirement{{Key: "org|tpm", PerMinute: 1000}}

	if r.Admit(tpm, now) != -1 {
		t.Fatal("a fresh bucket should admit a request")
	}
	charge(r, "org|tpm", 1000, 5000, now.Add(-5*time.Minute))
	if r.Admit(tpm, now) == -1 {
		t.Error("a charge stamped five minutes back erased five minutes of debt")
	}

	rpm := []Requirement{{Key: "org|rpm", PerMinute: 1, Take: true}}
	if r.Admit(rpm, now) != -1 {
		t.Fatal("a fresh bucket should admit a request")
	}
	r.Admit(rpm, now.Add(-time.Minute))
	if r.Admit(rpm, now) == -1 {
		t.Error("a request stamped a minute back refilled the bucket for the next one")
	}
}

func TestRedisChargeAllChargesEveryBucketLikeMemory(t *testing.T) {
	// One round trip charges every bucket, as the in-memory limiter would.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	l := New()
	now := time.Now()
	reqs := []Requirement{
		{Key: "org|tpm", PerMinute: 1000},
		{Key: "project|tpm"}, // unlimited, so left out
		{Key: "key|tpm", PerMinute: 300},
	}
	r.ChargeAll(reqs, 120, now)
	l.ChargeAll(reqs, 120, now)

	// A second replica has no view yet, so it has to read what Redis holds.
	other := replica(t, rdb, prefix)
	for _, req := range []Requirement{reqs[0], reqs[2]} {
		want := l.Remaining(req.Key, req.PerMinute, now)
		if got := r.Remaining(req.Key, req.PerMinute, now); got != want {
			t.Errorf("%s: Redis view has %d left, memory has %d", req.Key, got, want)
		}
		probe := []Requirement{{Key: req.Key, PerMinute: req.PerMinute, Take: true}}
		other.Admit(probe, now)
		if got := other.Remaining(req.Key, req.PerMinute, now); got != want-1 {
			t.Errorf("%s: Redis holds %d after one more request, want %d", req.Key, got, want-1)
		}
	}
}

func TestRedisChargeFromOneReplicaBindsTheOther(t *testing.T) {
	// A charge made by one replica must bind on every replica.
	rdb, prefix := redisFor(t)
	a, b := replica(t, rdb, prefix), replica(t, rdb, prefix)
	now := time.Now()

	charge(a, "org|tpm", 1000, 5000, now)
	if got := b.Admit([]Requirement{{Key: "org|tpm", PerMinute: 1000}}, now); got != 0 {
		t.Errorf("Admit = %d, want 0: the other replica did not see the charge", got)
	}
}

func TestRedisHeadersFollowTheDecision(t *testing.T) {
	// Remaining must include a charge another replica made.
	rdb, prefix := redisFor(t)
	a, b := replica(t, rdb, prefix), replica(t, rdb, prefix)
	now := time.Now()
	reqs := []Requirement{{Key: "org|rpm", PerMinute: 100, Take: true}}

	for range 30 {
		a.Admit(reqs, now)
	}
	if got := a.Remaining("org|rpm", 100, now); got != 70 {
		t.Errorf("Remaining = %d, want 70", got)
	}
	b.Admit(reqs, now)
	if got := b.Remaining("org|rpm", 100, now); got != 69 {
		t.Errorf("Remaining = %d on the second replica, want 69: it must report the "+
			"shared bucket, not its own", got)
	}
}

func TestRedisBucketsExpireOnTheirOwn(t *testing.T) {
	// Nothing sweeps the Redis keys, so they need a TTL.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	now := time.Now()
	r.Admit([]Requirement{{Key: "org|rpm", PerMinute: 60, Take: true}}, now)

	ttl, err := rdb.PTTL(t.Context(), prefix+":rl:org|rpm").Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	// A minute to refill the burst, plus the minute of slack the script adds.
	if ttl <= 0 || ttl > 3*time.Minute {
		t.Errorf("TTL = %v, want a bounded one of about two minutes", ttl)
	}
}

func TestRedisAgreesWithTheInMemoryLimiter(t *testing.T) {
	// Switching Redis on must not change how the limits behave.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	l := New()
	now := time.Now()
	reqs := []Requirement{
		{Key: "org|rpm", PerMinute: 30, Take: true},
		{Key: "org|tpm", PerMinute: 2000},
	}

	for i := range 40 {
		at := now.Add(time.Duration(i) * 900 * time.Millisecond)
		want := l.Admit(reqs, at)
		got := r.Admit(reqs, at)
		if got != want {
			t.Fatalf("request %d at %v: Redis said %d, memory said %d", i, at.Sub(now), got, want)
		}
		if want == -1 {
			charge(l, "org|tpm", 2000, 150, at)
			charge(r, "org|tpm", 2000, 150, at)
		}
		if a, b := r.Remaining("org|rpm", 30, at), l.Remaining("org|rpm", 30, at); a != b {
			t.Fatalf("request %d: Redis has %d left, memory has %d", i, a, b)
		}
	}
}

// liveScripter is a fake Redis that answers, to test the bookkeeping around a
// round trip. It returns the scripts' shape, not their arithmetic.
type liveScripter struct{ tokens int64 }

// cmd answers as whichever script was asked for, told apart by digest.
func (l *liveScripter) cmd(ctx context.Context, sha string, keys []string) *redis.Cmd {
	c := redis.NewCmd(ctx)
	out := make([]any, 0, len(keys)+1)
	if sha != chargeScript.Hash() {
		out = append(out, int64(-1)) // nothing failed
	}
	for range keys {
		out = append(out, l.tokens)
	}
	c.SetVal(out)
	return c
}

func (l *liveScripter) Eval(ctx context.Context, script string, keys []string, _ ...any) *redis.Cmd {
	// Run falls back to EVAL with the full source when a digest is not cached.
	sha := chargeScript.Hash()
	if script != chargeSource {
		sha = ""
	}
	return l.cmd(ctx, sha, keys)
}

func (l *liveScripter) EvalSha(ctx context.Context, sha string, keys []string, _ ...any) *redis.Cmd {
	return l.cmd(ctx, sha, keys)
}

func (l *liveScripter) EvalRO(ctx context.Context, script string, keys []string, a ...any) *redis.Cmd {
	return l.Eval(ctx, script, keys, a...)
}

func (l *liveScripter) EvalShaRO(ctx context.Context, sha string, keys []string, _ ...any) *redis.Cmd {
	return l.cmd(ctx, sha, keys)
}

func (l *liveScripter) ScriptExists(ctx context.Context, _ ...string) *redis.BoolSliceCmd {
	c := redis.NewBoolSliceCmd(ctx)
	c.SetVal([]bool{true})
	return c
}

func (l *liveScripter) ScriptLoad(ctx context.Context, _ string) *redis.StringCmd {
	return redis.NewStringCmd(ctx)
}

func TestRedisSweepReleasesTheViewsAndTheBucketsUnderThem(t *testing.T) {
	// Redis buckets expire on their own; the views must be swept. A swept key
	// is answered from the local buckets again.
	r := NewRedis(&liveScripter{tokens: 7000}, New(), RedisOptions{}, nil)
	now := time.Now()

	r.Admit([]Requirement{{Key: "org|rpm", PerMinute: 60, Take: true}}, now)
	if got := r.Remaining("org|rpm", 60, now); got != 7 {
		t.Fatalf("Remaining = %d, want 7 from what the round trip saw", got)
	}

	// Still inside the idle window: a bucket in use is kept.
	r.Sweep(time.Hour, now.Add(time.Minute))
	if got := r.Remaining("org|rpm", 60, now); got != 7 {
		t.Errorf("Remaining = %d after a sweep that should have kept it, want 7", got)
	}

	// Past it: the local buckets, which have seen nothing, report it full.
	r.Sweep(time.Hour, now.Add(2*time.Hour))
	if got := r.Remaining("org|rpm", 60, now); got != 60 {
		t.Errorf("Remaining = %d after the view was swept, want the local 60", got)
	}
}

func TestRedisSweepKeepsAViewTheHeadersAreStillReading(t *testing.T) {
	// Reading a view marks it seen, or a nearly spent bucket would be swept
	// and read as full. The test stays within a minute, the full refill time.
	r := NewRedis(&liveScripter{tokens: 3000}, New(), RedisOptions{}, nil)
	now := time.Now()
	charge(r, "org|tpm", 600, 1, now)

	// Ten seconds on, the view's 3 tokens have refilled by 100, at ten a
	// second. The local buckets would say 600.
	read := now.Add(10 * time.Second)
	if got := r.Remaining("org|tpm", 600, read); got != 103 {
		t.Fatalf("Remaining = %d, want 103 from the view", got)
	}

	// Idle counts from the read, not the decision, so a 15s window keeps it.
	r.Sweep(15*time.Second, now.Add(20*time.Second))
	if got := r.Remaining("org|tpm", 600, now.Add(20*time.Second)); got != 203 {
		t.Errorf("Remaining = %d after the sweep, want 203: the view a header "+
			"had just read was swept as idle", got)
	}
}

func TestRedisSweepAlsoBoundsTheLocalBucketsUnderneath(t *testing.T) {
	// The fallback limiter fills while Redis is down, so it is swept too.
	local := New()
	r := NewRedis(&deadScripter{err: errors.New("down")}, local, RedisOptions{}, nil)
	now := time.Now()

	r.Admit([]Requirement{{Key: "org|rpm", PerMinute: 60, Take: true}}, now)
	if got := local.Remaining("org|rpm", 60, now); got != 59 {
		t.Fatalf("the local bucket has %d left, want 59: the fallback did not charge it", got)
	}

	r.Sweep(time.Hour, now.Add(2*time.Hour))
	if got := local.Remaining("org|rpm", 60, now); got != 60 {
		t.Errorf("the local bucket has %d left after a sweep, want a forgotten 60", got)
	}
}
