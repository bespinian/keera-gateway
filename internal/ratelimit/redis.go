package ratelimit

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis decides the same buckets as Limiter, kept in Redis, so all replicas
// share one allowance.
//
// Redis holds nothing that must be kept, so when it cannot be reached,
// decisions fall back to the in-memory buckets. See degrade.
type Redis struct {
	rdb     redis.Scripter
	local   *Limiter
	prefix  string
	timeout time.Duration
	log     *slog.Logger
	report  Reporter

	// mu guards the fields below.
	mu       sync.Mutex
	view     map[string]view
	complain time.Time
	// resting is when to try Redis again after a failure. Until then every
	// decision is local, so an outage does not add the timeout to every
	// request.
	resting time.Time

	chargeEvery time.Duration
	// owedMu guards owed: the charges held until the next Flush, by bucket.
	owedMu sync.Mutex
	owed   map[string]owed
}

// owed is what one bucket is to be charged at the next Flush.
type owed struct {
	req Requirement
	n   float64
}

// Reporter counts the decisions made locally because Redis could not be
// reached. An interface, so this package depends on nothing.
type Reporter interface {
	RateLimitFallback()
}

// view is one bucket as of Redis's last answer. Remaining and Retry only feed
// advisory headers, so they extrapolate from it rather than ask again.
type view struct {
	tokens float64
	at     time.Time
	seen   time.Time
}

// RedisOptions configures a Redis limiter. The zero value is usable.
type RedisOptions struct {
	// Prefix namespaces the keys, so two deployments can share one Redis.
	// Defaults to DefaultRedisPrefix.
	Prefix string
	// Timeout bounds one round trip. Short on purpose: a slow Redis should
	// cost accuracy, not latency.
	Timeout time.Duration
	// Metrics counts the fallbacks. Optional.
	Metrics Reporter
	// ChargeEvery holds ChargeAll's charges and sends them together this
	// often, from Run. A request then does not wait for Redis once it is
	// answered, and many requests share one round trip. Zero sends each charge
	// at once.
	ChargeEvery time.Duration
}

// DefaultRedisPrefix is the key prefix when none is set.
const DefaultRedisPrefix = "keera"

const (
	defaultRedisTimeout = 250 * time.Millisecond
	// complainEvery bounds the log while Redis is down.
	complainEvery = time.Minute
	// restFor is how long the limiter decides locally after Redis failed.
	restFor = 5 * time.Second
)

// NewRedis returns a limiter that keeps its buckets in rdb, falling back to
// local when Redis cannot answer. The caller passes local so it can sweep it.
//
// rdb must be a single endpoint, not a Redis Cluster: one script touches
// several buckets at once, and a cluster refuses keys in different slots, so
// every decision would quietly go local.
func NewRedis(rdb redis.Scripter, local *Limiter, opts RedisOptions, log *slog.Logger) *Redis {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Redis{
		rdb:     rdb,
		local:   local,
		prefix:  cmp.Or(opts.Prefix, DefaultRedisPrefix),
		timeout: opts.Timeout,
		log:     log,
		report:  opts.Metrics,
		view:    make(map[string]view),
	}
	if r.timeout <= 0 {
		r.timeout = defaultRedisTimeout
	}
	if opts.ChargeEvery > 0 {
		r.chargeEvery = opts.ChargeEvery
		r.owed = make(map[string]owed)
	}
	return r
}

// refillSource starts both scripts. refill reads one bucket and adds what has
// dripped in since its last write, uncapped. It returns the tokens and the
// time to store with them.
//
// The clock comes from the caller, not Redis TIME, so it matches the in-memory
// limiter and stays testable. The stored time never moves back, so an earlier
// clock, such as another replica's, can cost some refill but never mint
// tokens.
const refillSource = `
local function refill(key, now, perSec, burst)
  local h = redis.call('HMGET', key, 't', 'ts')
  if not h[1] then return burst, now end
  local t, ts = tonumber(h[1]), now
  local elapsed = (now - tonumber(h[2])) / 1000
  if elapsed > 0 then t = t + elapsed * perSec else ts = tonumber(h[2]) end
  return t, ts
end
`

// admitSource refills every bucket in the batch, decides them all, and charges
// the Take ones only if all held. One script, so one atomic decision.
const admitSource = refillSource + `
local now = tonumber(ARGV[1])
local n = #KEYS
local live, order, ttl, ts = {}, {}, {}, {}
local tokens = {}

for i = 1, n do
  local perSec = tonumber(ARGV[3 * i - 1])
  local burst = tonumber(ARGV[3 * i])
  local key = KEYS[i]
  local t = live[key]
  if t == nil then
    order[#order + 1] = key
    t, ts[key] = refill(key, now, perSec, burst)
  end
  if t > burst then t = burst end
  live[key] = t
  tokens[i] = t
  if perSec > 0 then
    local want = math.ceil(burst / perSec * 1000) + 60000
    if ttl[key] == nil or want > ttl[key] then ttl[key] = want end
  end
end

local failed = -1
for i = 1, n do
  local take = tonumber(ARGV[3 * i + 1])
  if (take == 1 and tokens[i] < 1) or (take == 0 and tokens[i] <= 0) then
    failed = i - 1
    break
  end
end

if failed < 0 then
  for i = 1, n do
    if tonumber(ARGV[3 * i + 1]) == 1 then
      live[KEYS[i]] = live[KEYS[i]] - 1
    end
  end
end

local out = {failed}
for i = 1, #order do
  local key = order[i]
  redis.call('HSET', key, 't', live[key], 'ts', ts[key])
  if ttl[key] then redis.call('PEXPIRE', key, ttl[key]) end
end
for i = 1, n do
  out[i + 1] = math.floor(live[KEYS[i]] * 1000)
end
return out
`

// chargeSource takes the amounts known at the end of requests from the buckets
// in KEYS, each its own amount. Each may go negative, as in memory.
const chargeSource = refillSource + `
local now = tonumber(ARGV[1])
local out = {}

for i = 1, #KEYS do
  local n = tonumber(ARGV[3 * i - 1]) / 1000
  local perSec = tonumber(ARGV[3 * i])
  local burst = tonumber(ARGV[3 * i + 1])
  local t, ts = refill(KEYS[i], now, perSec, burst)
  if t > burst then t = burst end
  t = t - n

  redis.call('HSET', KEYS[i], 't', t, 'ts', ts)
  if perSec > 0 then
    redis.call('PEXPIRE', KEYS[i], math.ceil(burst / perSec * 1000) + 60000)
  end
  out[i] = math.floor(t * 1000)
end
return out
`

// Named, so a test can tell which script it is answering: on the wire it is
// only a digest.
var (
	admitScript  = redis.NewScript(admitSource)
	chargeScript = redis.NewScript(chargeSource)
)

// Admit decides one request against every requirement at once, in Redis. It
// returns the index into reqs of the first failed requirement, or -1.
func (r *Redis) Admit(reqs []Requirement, now time.Time) int {
	keys, args, at := r.admitArgs(reqs, []any{now.UnixMilli()})
	if len(keys) == 0 {
		return -1
	}
	if r.skip() {
		return r.local.Admit(reqs, now)
	}

	ctx, cancel := r.context()
	defer cancel()
	out, err := admitScript.Run(ctx, r.rdb, keys, args...).Int64Slice()
	if err != nil || len(out) != len(keys)+1 {
		r.degrade("admitting a request", err)
		return r.local.Admit(reqs, now)
	}

	r.remember(keys, out[1:], now)
	if failed := int(out[0]); failed >= 0 {
		return at[failed]
	}
	return -1
}

// admitArgs adds each limited requirement's key, rate and the unit it takes to
// args. Unlimited ones are left out, so at maps each key back to its index in
// reqs.
func (r *Redis) admitArgs(reqs []Requirement, args []any) (keys []string, _ []any, at []int) {
	for i, req := range reqs {
		if req.PerMinute <= 0 {
			continue
		}
		perSec, burst := rate(req.PerMinute)
		take := 0
		if req.Take {
			take = 1
		}
		keys = append(keys, r.key(req.Key))
		args = append(args, perSec, burst, take)
		at = append(at, i)
	}
	return keys, args, at
}

// ChargeAll takes n units from the bucket of every requirement, in one round
// trip. With ChargeEvery set, it only notes the charge for the next Flush.
// Take is not used.
func (r *Redis) ChargeAll(reqs []Requirement, n float64, now time.Time) {
	if n == 0 {
		return
	}
	if r.chargeEvery <= 0 {
		r.charge(reqs, func(int) float64 { return n }, now)
		return
	}
	r.owedMu.Lock()
	defer r.owedMu.Unlock()
	for _, req := range reqs {
		if req.PerMinute <= 0 {
			continue
		}
		o := r.owed[req.Key]
		o.req, o.n = req, o.n+n
		r.owed[req.Key] = o
	}
}

// maxChargeKeys bounds one charge script, so a flush after a busy interval
// does not hold Redis for long.
const maxChargeKeys = 256

// Flush sends the charges ChargeAll held, as of now. They reach Redis up to
// ChargeEvery late, which a limit per minute does not notice.
func (r *Redis) Flush(now time.Time) {
	r.owedMu.Lock()
	held := r.owed
	if len(held) > 0 {
		r.owed = make(map[string]owed, len(held))
	}
	r.owedMu.Unlock()

	reqs := make([]Requirement, 0, min(len(held), maxChargeKeys))
	ns := make([]float64, 0, cap(reqs))
	for _, o := range held {
		reqs, ns = append(reqs, o.req), append(ns, o.n)
		if len(reqs) == maxChargeKeys {
			r.charge(reqs, func(i int) float64 { return ns[i] }, now)
			reqs, ns = reqs[:0], ns[:0]
		}
	}
	if len(reqs) > 0 {
		r.charge(reqs, func(i int) float64 { return ns[i] }, now)
	}
}

// Run flushes the held charges every ChargeEvery until ctx is done, and once
// more then. Without ChargeEvery there is nothing to hold, and it returns.
func (r *Redis) Run(ctx context.Context) {
	if r.chargeEvery <= 0 {
		return
	}
	t := time.NewTicker(r.chargeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Flush(time.Now())
			return
		case now := <-t.C:
			r.Flush(now)
		}
	}
}

// charge takes n(i) units from the bucket of reqs[i], in one round trip.
func (r *Redis) charge(reqs []Requirement, n func(i int) float64, now time.Time) {
	keys := make([]string, 0, len(reqs))
	args := make([]any, 1, 1+3*len(reqs))
	args[0] = now.UnixMilli()
	for i, req := range reqs {
		if req.PerMinute <= 0 {
			continue
		}
		perSec, burst := rate(req.PerMinute)
		keys = append(keys, r.key(req.Key))
		args = append(args, n(i)*1000, perSec, burst)
	}
	if len(keys) == 0 {
		return
	}
	if r.skip() {
		r.chargeLocal(reqs, n, now)
		return
	}

	ctx, cancel := r.context()
	defer cancel()
	left, err := chargeScript.Run(ctx, r.rdb, keys, args...).Int64Slice()
	if err != nil || len(left) != len(keys) {
		r.degrade("charging a request", err)
		r.chargeLocal(reqs, n, now)
		return
	}
	r.remember(keys, left, now)
}

func (r *Redis) chargeLocal(reqs []Requirement, n func(i int) float64, now time.Time) {
	for i := range reqs {
		r.local.ChargeAll(reqs[i:i+1], n(i), now)
	}
}

// Remaining reports how many units key's bucket still holds, from what the last
// round trip saw. A key not seen yet is answered from the in-memory buckets.
func (r *Redis) Remaining(key string, perMinute int, now time.Time) int {
	if perMinute <= 0 {
		return -1
	}
	t, ok := r.extrapolate(key, perMinute, now)
	if !ok {
		return r.local.Remaining(key, perMinute, now)
	}
	return remaining(t)
}

// Retry returns how long key's bucket needs to hold one unit again.
func (r *Redis) Retry(key string, perMinute int, now time.Time) time.Duration {
	if perMinute <= 0 {
		return 0
	}
	t, ok := r.extrapolate(key, perMinute, now)
	if !ok {
		return r.local.Retry(key, perMinute, now)
	}
	perSec, _ := rate(perMinute)
	return retryAfter(t, perSec)
}

// Sweep forgets the views and in-memory buckets untouched for idle. The buckets
// in Redis expire on their own.
func (r *Redis) Sweep(idle time.Duration, now time.Time) {
	r.local.Sweep(idle, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.view {
		if now.Sub(v.seen) > idle {
			delete(r.view, k)
		}
	}
}

// remember stores what the script reported, keyed by the full Redis key.
func (r *Redis) remember(keys []string, tokens []int64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, k := range keys {
		r.view[k] = view{tokens: float64(tokens[i]) / 1000, at: now, seen: now}
	}
}

// extrapolate refills the last seen value to now, as the next script run would.
func (r *Redis) extrapolate(key string, perMinute int, now time.Time) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.key(key)
	v, ok := r.view[k]
	if !ok {
		return 0, false
	}
	v.seen = now
	r.view[k] = v

	perSec, burst := rate(perMinute)
	t := v.tokens
	if elapsed := now.Sub(v.at).Seconds(); elapsed > 0 {
		t += elapsed * perSec
	}
	return min(t, burst), true
}

// degrade records a decision made locally because Redis could not answer. The
// gateway keeps serving: the budget in Postgres is the exact control.
func (r *Redis) degrade(doing string, err error) {
	if r.report != nil {
		r.report.RateLimitFallback()
	}
	if err == nil {
		err = errors.New("redis returned an answer of the wrong shape")
	}
	now := time.Now()
	r.mu.Lock()
	r.resting = now.Add(restFor)
	quiet := now.Sub(r.complain) < complainEvery
	if !quiet {
		r.complain = now
	}
	r.mu.Unlock()
	if quiet {
		return
	}
	r.log.Warn("rate limiting could not reach Redis and is deciding locally, so the "+
		"configured limits bind per replica until it is back",
		"doing", doing, "error", err)
}

// skip reports whether Redis failed so recently that this decision is made
// locally without trying. It counts as a fallback too.
func (r *Redis) skip() bool {
	r.mu.Lock()
	resting := time.Now().Before(r.resting)
	r.mu.Unlock()
	if resting && r.report != nil {
		r.report.RateLimitFallback()
	}
	return resting
}

func (r *Redis) key(key string) string { return r.prefix + ":rl:" + key }

// context bounds one round trip, detached from the request, so a client that
// hangs up cannot leave a shared bucket half charged.
func (r *Redis) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), r.timeout)
}
