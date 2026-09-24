package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis decides the same buckets as Limiter with the tokens kept in Redis, so
// all replicas share one allowance instead of having one each.
//
// Redis holds only the buckets, nothing that must be kept. So when it cannot
// be reached, decisions fall back to the in-memory buckets rather than
// refusing traffic. See degrade.
type Redis struct {
	rdb     redis.Scripter
	local   *Limiter
	prefix  string
	timeout time.Duration
	log     *slog.Logger
	report  Reporter

	// view is what the last round trip saw, so response headers cost no extra
	// round trip. See Remaining.
	mu       sync.Mutex
	view     map[string]view
	complain time.Time
	// resting is when to try Redis again after a failed round trip. Until
	// then every decision is local, so an outage does not add the full
	// timeout to every request.
	resting time.Time
}

// Reporter counts the decisions made locally because Redis could not be
// reached. An interface, so this package depends on nothing.
type Reporter interface {
	RateLimitFallback()
}

// view is one bucket as of the last answer Redis gave about it. Remaining and
// Retry only feed advisory headers, so they extrapolate from it instead of
// paying two more round trips per request.
type view struct {
	tokens float64
	at     time.Time
	seen   time.Time
}

// RedisOptions configures a Redis limiter. The zero value is usable.
type RedisOptions struct {
	// Prefix namespaces the keys, so two deployments can share one Redis.
	// Defaults to "keera".
	Prefix string
	// Timeout bounds one round trip. It is short on purpose: every request
	// waits for it, and a slow Redis should cost accuracy, not latency.
	Timeout time.Duration
	// Metrics counts the fallbacks. Optional.
	Metrics Reporter
}

const (
	defaultRedisPrefix  = "keera"
	defaultRedisTimeout = 250 * time.Millisecond
	// complainEvery bounds the log while Redis is down.
	complainEvery = time.Minute
	// restFor is how long the limiter decides locally after Redis failed.
	restFor = 5 * time.Second
)

// NewRedis returns a limiter that keeps its buckets in rdb, falling back to
// local for any decision Redis could not answer. The caller passes local so it
// keeps the one it sweeps.
//
// rdb must be a single endpoint, not a Redis Cluster: one script touches the
// org's, team's and key's buckets at once, and a cluster would refuse keys in
// different slots, so every decision would quietly go local.
func NewRedis(rdb redis.Scripter, local *Limiter, opts RedisOptions, log *slog.Logger) *Redis {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Redis{
		rdb:     rdb,
		local:   local,
		prefix:  orDefault(opts.Prefix, defaultRedisPrefix),
		timeout: opts.Timeout,
		log:     log,
		report:  opts.Metrics,
		view:    make(map[string]view),
	}
	if r.timeout <= 0 {
		r.timeout = defaultRedisTimeout
	}
	return r
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// admitSource refills every bucket in the batch, decides them all, and charges
// the Take ones only if all of them held. It is one script so it is one atomic
// decision, as Limiter.Admit is under its lock.
//
// The clock comes from the caller, not Redis TIME, so the arithmetic matches
// the in-memory limiter and stays testable. A negative elapsed time is ignored,
// so clock skew can cost some refill but never mint tokens.
const admitSource = `
local now = tonumber(ARGV[1])
local n = #KEYS
local live, order, ttl = {}, {}, {}
local tokens = {}

for i = 1, n do
  local perSec = tonumber(ARGV[3 * i - 1])
  local burst = tonumber(ARGV[3 * i])
  local key = KEYS[i]
  local t = live[key]
  if t == nil then
    order[#order + 1] = key
    local h = redis.call('HMGET', key, 't', 'ts')
    if h[1] then
      t = tonumber(h[1])
      local elapsed = (now - tonumber(h[2])) / 1000
      if elapsed > 0 then t = t + elapsed * perSec end
    else
      t = burst
    end
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
  redis.call('HSET', key, 't', live[key], 'ts', now)
  if ttl[key] then redis.call('PEXPIRE', key, ttl[key]) end
end
for i = 1, n do
  out[i + 1] = math.floor(live[KEYS[i]] * 1000)
end
return out
`

// chargeSource takes an amount known only once the request is over from
// every bucket in KEYS, in one round trip. Like the in-memory bucket each may
// go negative, so the requests that follow pay for a very large one.
const chargeSource = `
local now = tonumber(ARGV[1])
local n = tonumber(ARGV[2]) / 1000
local out = {}

for i = 1, #KEYS do
  local perSec = tonumber(ARGV[2 * i + 1])
  local burst = tonumber(ARGV[2 * i + 2])
  local t
  local h = redis.call('HMGET', KEYS[i], 't', 'ts')
  if h[1] then
    t = tonumber(h[1])
    local elapsed = (now - tonumber(h[2])) / 1000
    if elapsed > 0 then t = t + elapsed * perSec end
  else
    t = burst
  end
  if t > burst then t = burst end
  t = t - n

  redis.call('HSET', KEYS[i], 't', t, 'ts', now)
  if perSec > 0 then
    redis.call('PEXPIRE', KEYS[i], math.ceil(burst / perSec * 1000) + 60000)
  end
  out[i] = math.floor(t * 1000)
end
return out
`

// The Lua stays in named constants so a test can tell which script it is
// answering; on the wire it is only a digest.
var (
	admitScript  = redis.NewScript(admitSource)
	chargeScript = redis.NewScript(chargeSource)
)

// Admit decides one request against every requirement at once, in Redis. It
// returns the index into reqs of the first failed requirement, or -1.
func (r *Redis) Admit(reqs []Requirement, now time.Time) int {
	keys, args, at := r.admitArgs(reqs, now)
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

// admitArgs builds the script's keys and arguments. Unlimited requirements are
// left out, so at maps each key back to its index in reqs.
func (r *Redis) admitArgs(reqs []Requirement, now time.Time) (keys []string, args []any, at []int) {
	args = append(args, now.UnixMilli())
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

// Charge takes n units from key's bucket.
func (r *Redis) Charge(key string, perMinute int, n float64, now time.Time) {
	r.ChargeAll([]Requirement{{Key: key, PerMinute: perMinute}}, n, now)
}

// ChargeAll takes n units from the bucket of every requirement, in one round
// trip. Take is not used.
func (r *Redis) ChargeAll(reqs []Requirement, n float64, now time.Time) {
	if n == 0 {
		return
	}
	var keys []string
	args := []any{now.UnixMilli(), n * 1000}
	for _, req := range reqs {
		if req.PerMinute <= 0 {
			continue
		}
		perSec, burst := rate(req.PerMinute)
		keys = append(keys, r.key(req.Key))
		args = append(args, perSec, burst)
	}
	if len(keys) == 0 {
		return
	}
	if r.skip() {
		r.local.ChargeAll(reqs, n, now)
		return
	}

	ctx, cancel := r.context()
	defer cancel()
	left, err := chargeScript.Run(ctx, r.rdb, keys, args...).Int64Slice()
	if err != nil || len(left) != len(keys) {
		r.degrade("charging a request", err)
		r.local.ChargeAll(reqs, n, now)
		return
	}
	r.remember(keys, left, now)
}

// Remaining reports how many units key's bucket still holds, from what the last
// round trip saw. A key this replica has not seen yet is answered from the
// in-memory buckets, since a local number beats an invented one.
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
// in Redis expire on their own, one full refill after their last write.
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
	v, ok := r.view[r.key(key)]
	if !ok {
		return 0, false
	}
	v.seen = now
	r.view[r.key(key)] = v

	perSec, burst := rate(perMinute)
	t := v.tokens
	if elapsed := now.Sub(v.at).Seconds(); elapsed > 0 {
		t += elapsed * perSec
	}
	return min(t, burst), true
}

// degrade records that a decision was made locally because Redis could not
// answer it. The gateway keeps serving: Redis is optional coordination, and
// the exact control is the budget in Postgres. The log says the limits are per
// replica until Redis is back.
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
// locally without trying it. Such a decision is counted as a fallback too.
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

// context bounds one round trip. It is detached from the request, so a client
// that hangs up cannot leave a shared bucket half charged.
func (r *Redis) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), r.timeout)
}
