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

// The half of this that needs no Redis: what happens when the Redis a
// deployment configured cannot be reached. That is the case the design turns
// on - the buckets are a brake, not a ledger, so a gateway that cannot reach
// them keeps serving with the per-replica ones underneath.

// deadScripter answers every script with an error, which is what a Redis that
// is down, unreachable or too slow looks like from here.
type deadScripter struct {
	err   error
	calls int
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
	// A Redis outage must cost the coordination and nothing else. The limits
	// go back to binding per replica - which is what this gateway would do with
	// no Redis configured at all - rather than the inference plane going down
	// with the thing that counts its requests.
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
	// Each try costs a request the whole timeout while Redis is down. After
	// one failure the limiter decides locally for a while without trying.
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
	// A token charge that cannot reach Redis is charged locally, and the
	// headers then have to read the same buckets - not an empty view of Redis,
	// which would tell a client it had its whole allowance left.
	r := NewRedis(&deadScripter{err: errors.New("i/o timeout")}, New(), RedisOptions{}, nil)
	now := time.Now()

	r.Charge("org|tpm", 1000, 5000, now)
	if got := r.Remaining("org|tpm", 1000, now); got != 0 {
		t.Errorf("Remaining = %d, want 0: the bucket is overdrawn locally", got)
	}
	if got := r.Retry("org|tpm", 1000, now); got <= 0 {
		t.Errorf("Retry = %v, want a positive wait", got)
	}
}

func TestRedisAnswersAnUnknownKeyFromTheLocalBuckets(t *testing.T) {
	// Remaining and Retry read what the last decision saw rather than paying a
	// round trip. A key no decision has touched has no such answer, and an
	// invented one would be worse than the local number.
	r := NewRedis(&deadScripter{err: errors.New("down")}, New(), RedisOptions{}, nil)
	now := time.Now()
	if got := r.Remaining("never-seen|rpm", 60, now); got != 60 {
		t.Errorf("Remaining = %d, want 60", got)
	}
	if got := r.Remaining("never-seen|rpm", 0, now); got != -1 {
		t.Errorf("Remaining = %d, want -1 for an unlimited bucket", got)
	}
}

// The other half, against a real Redis. What is covered here is the part no
// fake can be: the Lua, run by the server, deciding one batch atomically for
// two replicas at once.

// redisFor returns a client, and a limiter whose keys are unique to this test
// so that a shared Redis does not make one test's traffic another's.
func redisFor(t *testing.T) (*redis.Client, string) {
	t.Helper()
	url := os.Getenv("KEERA_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set KEERA_TEST_REDIS_URL to run the Redis rate-limiting tests (make redis-up)")
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

func TestRedisSpendsOneAllowanceAcrossReplicas(t *testing.T) {
	// The whole point. Two gateways, one limit of 10: between them they get
	// ten requests, not ten each.
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

func TestRedisAdmitChargesEveryLevelOrNone(t *testing.T) {
	// The same rule the in-memory limiter keeps, now across a round trip: an
	// org must not pay for requests its team refused.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	now := time.Now()
	reqs := []Requirement{
		{Key: "org:o1|rpm", PerMinute: 600, Take: true},
		{Key: "team:t1|rpm", PerMinute: 2, Take: true},
	}

	for i := range 2 {
		if got := r.Admit(reqs, now); got != -1 {
			t.Fatalf("request %d: Admit = %d, want -1", i, got)
		}
	}
	for range 20 {
		if got := r.Admit(reqs, now); got != 1 {
			t.Fatalf("Admit = %d, want 1 - the team's limit is what binds", got)
		}
	}
	if got := r.Remaining("org:o1|rpm", 600, now); got != 598 {
		t.Errorf("the org has %d of 600 left, want 598: it was charged for requests "+
			"the team refused", got)
	}
}

func TestRedisReportsTheOutermostLimitThatBinds(t *testing.T) {
	// The index has to be an index into the caller's requirements, including
	// the unlimited ones that are never sent to Redis at all.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	now := time.Now()
	reqs := []Requirement{
		{Key: "org|tpm", PerMinute: 0},
		{Key: "org|rpm", PerMinute: 1, Take: true},
		{Key: "key|rpm", PerMinute: 1, Take: true},
	}
	if got := r.Admit(reqs, now); got != -1 {
		t.Fatalf("Admit = %d, want -1", got)
	}
	if got := r.Admit(reqs, now); got != 1 {
		t.Errorf("Admit = %d, want 1 - the org is checked before the key, and the "+
			"unlimited requirement still occupies index 0", got)
	}
}

func TestRedisChargeCanOverdrawSoOneHugeRequestIsPaidForLater(t *testing.T) {
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	now := time.Now()
	inCredit := func(at time.Time) bool {
		return r.Admit([]Requirement{{Key: "org|tpm", PerMinute: 1000}}, at) < 0
	}

	if !inCredit(now) {
		t.Fatal("a fresh bucket should admit a request")
	}
	r.Charge("org|tpm", 1000, 5000, now)
	if inCredit(now) {
		t.Error("the bucket is overdrawn and should refuse")
	}
	if inCredit(now.Add(2 * time.Minute)) {
		t.Error("the debt was forgiven too early")
	}
	if !inCredit(now.Add(5 * time.Minute)) {
		t.Error("the debt was never repaid")
	}
}

func TestRedisChargeAllChargesEveryBucketLikeMemory(t *testing.T) {
	// One round trip charges every scope's bucket, and must leave each as the
	// in-memory limiter would.
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	l := New()
	now := time.Now()
	reqs := []Requirement{
		{Key: "org|tpm", PerMinute: 1000},
		{Key: "team|tpm"}, // unlimited, so left out
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
	// Tokens are charged when a request ends, by whichever replica served it.
	// A limit that only the charging replica then felt would be no limit at
	// all for a client that is being load-balanced.
	rdb, prefix := redisFor(t)
	a, b := replica(t, rdb, prefix), replica(t, rdb, prefix)
	now := time.Now()

	a.Charge("org|tpm", 1000, 5000, now)
	if got := b.Admit([]Requirement{{Key: "org|tpm", PerMinute: 1000}}, now); got != 0 {
		t.Errorf("Admit = %d, want 0: the other replica did not see the charge", got)
	}
}

func TestRedisRetryReportsWhenTheNextRequestFits(t *testing.T) {
	rdb, prefix := redisFor(t)
	r := replica(t, rdb, prefix)
	now := time.Now()
	reqs := []Requirement{{Key: "org|rpm", PerMinute: 60, Take: true}}
	for range 60 {
		r.Admit(reqs, now)
	}
	if got := r.Admit(reqs, now); got != 0 {
		t.Fatalf("Admit = %d, want 0 - the burst should be spent", got)
	}

	d := r.Retry("org|rpm", 60, now)
	if d <= 0 {
		t.Fatalf("Retry = %v, want a positive wait", d)
	}
	if got := r.Admit(reqs, now.Add(d+10*time.Millisecond)); got != -1 {
		t.Errorf("waiting the advertised %v was not enough", d)
	}
}

func TestRedisHeadersFollowTheDecision(t *testing.T) {
	// Remaining is read from what the decision itself learned, so it has to be
	// the number Redis actually holds - including a charge another replica made.
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
	// The in-memory limiter is swept; the Redis one cannot be, because no
	// replica owns the keys. A TTL one full refill long is that bound,
	// expressed where the data is - otherwise a gateway that has seen many
	// short-lived keys leaves them in Redis for ever.
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
	// The two are one behaviour with two storage choices, and a deployment
	// switching Redis on must not find its limits behaving differently. The
	// same sequence of calls, decided both ways.
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
			l.Charge("org|tpm", 2000, 150, at)
			r.Charge("org|tpm", 2000, 150, at)
		}
		if a, b := r.Remaining("org|rpm", 30, at), l.Remaining("org|rpm", 30, at); a != b {
			t.Fatalf("request %d: Redis has %d left, memory has %d", i, a, b)
		}
	}
}

// liveScripter is a Redis that answers, so that the bookkeeping either side of
// a round trip can be tested without one. What it returns is the shape the
// scripts return - a token count in thousandths - and not their arithmetic,
// which is covered against a real server above.
type liveScripter struct{ tokens int64 }

// cmd answers as whichever script was asked for. The two cannot be told apart
// by their keys - admitting one requirement and charging a bucket both send
// exactly one - so they are told apart by the digest, which is what EvalSha is
// given and is the only thing on the wire that names the script.
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
	// Run falls back to EVAL with the source when a digest is not cached, so
	// this has to recognise the same script written out in full.
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
	// The buckets in Redis expire on their own. The views in front of them do
	// not: they are written on every decision and read by every response
	// header, so without this the map grows with every distinct key ever
	// presented - which on a public endpoint is every scan.
	//
	// What makes it observable is that a swept key is answered from the local
	// buckets again, exactly as a key no decision has touched is.
	r := NewRedis(&liveScripter{tokens: 7000}, New(), RedisOptions{}, nil)
	now := time.Now()

	r.Admit([]Requirement{{Key: "org|rpm", PerMinute: 60, Take: true}}, now)
	if got := r.Remaining("org|rpm", 60, now); got != 7 {
		t.Fatalf("Remaining = %d, want 7 from what the round trip saw", got)
	}

	// Still inside the idle window: a bucket in use is not swept out from
	// under the headers that are reading it.
	r.Sweep(time.Hour, now.Add(time.Minute))
	if got := r.Remaining("org|rpm", 60, now); got != 7 {
		t.Errorf("Remaining = %d after a sweep that should have kept it, want 7", got)
	}

	// Past it: the view goes, and the answer comes from the local buckets,
	// which have seen nothing and so report a full allowance.
	r.Sweep(time.Hour, now.Add(2*time.Hour))
	if got := r.Remaining("org|rpm", 60, now); got != 60 {
		t.Errorf("Remaining = %d after the view was swept, want the local 60", got)
	}
}

func TestRedisSweepKeepsAViewTheHeadersAreStillReading(t *testing.T) {
	// Reading a view marks it seen. A key quiet enough to be taking no new
	// decisions, but still having response headers written for it, must not be
	// swept out from under them: the next read would fall through to the local
	// buckets and report a full allowance for a bucket that is nearly spent.
	//
	// Every bucket refills completely in a minute, whatever its rate, so the
	// whole test lives inside one - past that the view and a full bucket are
	// the same number and nothing could be told apart.
	r := NewRedis(&liveScripter{tokens: 3000}, New(), RedisOptions{}, nil)
	now := time.Now()
	r.Charge("org|tpm", 600, 1, now)

	// Ten seconds on, the view says three tokens refilled at ten a second. The
	// local buckets, which have seen nothing, would say 600.
	read := now.Add(10 * time.Second)
	if got := r.Remaining("org|tpm", 600, read); got != 103 {
		t.Fatalf("Remaining = %d, want 103 from the view", got)
	}

	// Idle is measured from that read, not from the decision that filled it.
	// Twenty seconds after the charge is ten after the read, so a fifteen
	// second window keeps it - and would not have, had the read not counted.
	r.Sweep(15*time.Second, now.Add(20*time.Second))
	if got := r.Remaining("org|tpm", 600, now.Add(20*time.Second)); got != 203 {
		t.Errorf("Remaining = %d after the sweep, want 203: the view a header "+
			"had just read was swept as idle", got)
	}
}

func TestRedisSweepAlsoBoundsTheLocalBucketsUnderneath(t *testing.T) {
	// The fallback limiter is the one this process keeps when Redis cannot be
	// reached, and it is filled by exactly those decisions. Sweeping only the
	// views would leave it growing unbounded on a gateway whose Redis is down -
	// which is the moment it is most in use.
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
