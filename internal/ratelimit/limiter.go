// Package ratelimit provides the token buckets behind Keera Gateway's per-scope
// request and token rate limits.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	perSec   float64
	burst    float64
	last     time.Time
	lastSeen time.Time
}

func (b *bucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.perSec)
		b.last = now
	}
	b.lastSeen = now
}

// Limiter holds one bucket per (scope, limit) pair, in memory.
//
// Buckets are per replica, so with N replicas the real ceiling is N times the
// configured one. That is fine for a brake on runaway clients; the exact
// spending control is the budget. Setting KEERA_REDIS_URL shares the buckets
// through [Redis], which keeps this one as its fallback.
type Limiter struct {
	mu sync.Mutex
	b  map[string]*bucket
}

// New returns an empty limiter.
func New() *Limiter { return &Limiter{b: make(map[string]*bucket)} }

// get returns the bucket for key, creating it or retuning it if the configured
// rate has changed since it was made.
func (l *Limiter) get(key string, perMinute int, now time.Time) *bucket {
	b, ok := l.b[key]
	perSec, burst := rate(perMinute)
	if !ok {
		// Set lastSeen here too, or Sweep would take a new bucket for one idle
		// since the zero time.
		b = &bucket{tokens: burst, perSec: perSec, burst: burst, last: now, lastSeen: now}
		l.b[key] = b
		return b
	}
	if b.perSec != perSec {
		b.perSec, b.burst = perSec, burst
		b.tokens = min(b.tokens, burst)
	}
	b.refill(now)
	return b
}

// Allow takes one unit from key's bucket and reports whether it was there.
// A perMinute of zero or less means unlimited.
func (l *Limiter) Allow(key string, perMinute int, now time.Time) bool {
	if perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key, perMinute, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Requirement is one bucket a request has to satisfy.
//
// Take consumes a unit on admission, as a requests-per-minute limit does.
// Without it the bucket only has to be in credit, as for tokens per minute:
// the real cost is known only at the end and is charged then.
//
// A PerMinute of zero or less is unlimited and always satisfied.
type Requirement struct {
	Key       string
	PerMinute int
	Take      bool
}

// Admit decides one request against every requirement at once and charges the
// Take requirements only if all of them hold. It returns the index of the
// first requirement that failed, or -1 when the request is admitted.
//
// Deciding and charging under one lock keeps a refusal free: otherwise an
// org's bucket could pay for a request its team's bucket then refused.
func (l *Limiter) Admit(reqs []Requirement, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, req := range reqs {
		if req.PerMinute <= 0 {
			continue
		}
		b := l.get(req.Key, req.PerMinute, now)
		if !holds(req.Take, b.tokens) {
			return i
		}
	}
	for _, req := range reqs {
		if req.PerMinute <= 0 || !req.Take {
			continue
		}
		l.get(req.Key, req.PerMinute, now).tokens--
	}
	return -1
}

// Charge takes n units from key's bucket, allowing it to go negative so that
// one very large request is paid for by the requests that follow it.
func (l *Limiter) Charge(key string, perMinute int, n float64, now time.Time) {
	if perMinute <= 0 || n == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.get(key, perMinute, now).tokens -= n
}

// ChargeAll takes n units from the bucket of every requirement. Take is not
// used.
func (l *Limiter) ChargeAll(reqs []Requirement, n float64, now time.Time) {
	if n == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, req := range reqs {
		if req.PerMinute > 0 {
			l.get(req.Key, req.PerMinute, now).tokens -= n
		}
	}
}

// Remaining reports how many units key's bucket still holds, or -1 when the
// limit is unlimited. It goes on every response, so a client can slow down
// before it hits a 429.
func (l *Limiter) Remaining(key string, perMinute int, now time.Time) int {
	if perMinute <= 0 {
		return -1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return remaining(l.get(key, perMinute, now).tokens)
}

// Retry returns how long key's bucket needs to hold one unit again.
func (l *Limiter) Retry(key string, perMinute int, now time.Time) time.Duration {
	if perMinute <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key, perMinute, now)
	return retryAfter(b.tokens, b.perSec)
}

// Sweep forgets buckets untouched for idle, so many short-lived keys do not
// grow memory for ever.
func (l *Limiter) Sweep(idle time.Duration, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.b {
		if now.Sub(b.lastSeen) > idle {
			delete(l.b, k)
		}
	}
}

// holds reports whether a bucket with tokens left satisfies a requirement. A
// Take needs a whole unit. Otherwise any credit will do: Charge puts the bucket
// into debt, and the next request pays for it.
func holds(take bool, tokens float64) bool {
	if take {
		return tokens >= 1
	}
	return tokens > 0
}

// remaining is what a client is told is left. A bucket in debt from Charge
// still has "none left".
func remaining(tokens float64) int {
	if tokens > 0 {
		return int(tokens)
	}
	return 0
}

// retryAfter is how long a bucket needs to refill to one whole unit.
func retryAfter(tokens, perSec float64) time.Duration {
	if tokens >= 1 || perSec == 0 {
		return 0
	}
	return time.Duration((1 - tokens) / perSec * float64(time.Second))
}

// rate turns a per-minute limit into the two numbers a bucket is made of, in
// one place so the in-memory and Redis limiters cannot drift.
func rate(perMinute int) (perSec, burst float64) {
	return float64(perMinute) / 60, float64(perMinute)
}
