package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
)

// refusal is a request the gateway will not forward.
type refusal struct {
	status int
	typ    string
	code   string
	msg    string
	// advise appends the "here is where to look" sentence to what the client
	// is told. It is a flag, so the usage row keeps only the refusal itself.
	advise bool
	// retry is how long the client should wait, for limits that lift on their
	// own.
	retry time.Duration
}

// refuse answers a request the gateway turned away, and records it.
//
// A refusal gets a usage row like any other request, because it is the event
// a developer asks about later ("my editor stopped working"). It charges
// nothing.
func (s *Server) refuse(c *call, ref refusal) {
	s.limitHeaders(c.w, c.res, c.tr.start)
	if ref.retry > 0 {
		retryAfter(c.w, ref.retry)
	}
	msg := ref.msg
	if ref.advise {
		msg = s.advise(msg)
	}
	c.surf.shape.writeError(c.w, ref.status, ref.typ, ref.code, msg)

	ev := c.ev
	ev.Status = ref.status
	// The status says a guardrail stopped the request; the message says which
	// one and what it was set to.
	ev.Error = ref.msg
	ev.Latency = time.Since(c.tr.start)
	// The step the request was stopped in is closed and named by the code.
	ev.Spans = c.tr.steps(ref.code)
	s.sink.Record(ev)
	s.metrics.Observe(metricModel(s.src, ev.Alias), ev.OrgID, ev.Status, ev.Latency.Seconds(), 0)
}

// unknownModel is the model label a refusal carries when the model it names is
// not one the catalogue holds.
const unknownModel = "unknown"

// metricModel bounds the model label to the catalogue.
//
// Metric series are never cleaned up, and a refused request's model is
// whatever the client sent. Without this, a client sending a new name on
// every request would grow the registry until the process ran out of memory.
// Forwarded requests have already been matched against the catalogue.
func metricModel(src policy.Source, alias string) string {
	if alias == "" {
		return ""
	}
	if _, found := src.Model(alias); !found {
		return unknownModel
	}
	return alias
}

// advise appends where to go and look. From inside an editor the message is
// the only place a developer learns the panel exists.
func (s *Server) advise(msg string) string {
	if s.opts.PanelURL == "" {
		return msg
	}
	return msg + ". Your keys and their limits are at " + s.opts.PanelURL + "/access"
}

// checkRates checks the rate limits of every scope.
//
// Each scope has its own limits, so an org-wide ceiling and a per-key ceiling
// both hold. All of them are decided at once: charging the outer scopes and
// then refusing at an inner one would spend allowance on a request that was
// never sent. Outer scopes come first, and requests before tokens, so the
// limit reported is the one an administrator expects to bind first.
//
// It only decides. The refusal is written by refuse, like every other one.
func (s *Server) checkRates(res *policy.Resolved, now time.Time) (refusal, bool) {
	type check struct {
		scope policy.Scope
		limit int
		unit  string
		code  string
	}
	reqs := make([]ratelimit.Requirement, 0, len(res.Scopes)*2)
	checks := make([]check, 0, len(res.Scopes)*2)
	for _, sc := range res.Scopes {
		reqs = append(reqs,
			ratelimit.Requirement{Key: bucketKey(sc, "rpm"), PerMinute: sc.RPM, Take: true},
			ratelimit.Requirement{Key: bucketKey(sc, "tpm"), PerMinute: sc.TPM})
		checks = append(checks,
			check{scope: sc, limit: sc.RPM, unit: "requests", code: "rate_limit_exceeded"},
			check{scope: sc, limit: sc.TPM, unit: "tokens", code: "token_rate_limit_exceeded"})
	}

	i := s.limiter.Admit(reqs, now)
	if i < 0 {
		return refusal{}, true
	}
	c := checks[i]
	return refusal{
		status: http.StatusTooManyRequests,
		typ:    "rate_limit_error", code: c.code,
		msg:    rateMessage(c.scope, c.limit, c.unit),
		advise: true,
		retry:  s.limiter.Retry(reqs[i].Key, c.limit, now),
	}, false
}

// rateMessage names the limit that bound and its number, for the developer
// whose agent just stopped.
func rateMessage(sc policy.Scope, limit int, unit string) string {
	return fmt.Sprintf("%s is over its rate limit of %d %s per minute",
		sc.Type.Possessive(), limit, unit)
}

// limitHeaders states what is left of this key's allowance. They go on every
// answer, not only on refusals, so a client can see a limit coming.
//
// Only the tightest scope is reported, so the client does not have to work out
// which one binds.
func (s *Server) limitHeaders(w http.ResponseWriter, res *policy.Resolved, now time.Time) {
	if res == nil {
		return
	}
	s.budgetHeaders(w.Header(), res, now)
	s.rateHeaders(w.Header(), res, now)
}

func (s *Server) budgetHeaders(h http.Header, res *policy.Resolved, now time.Time) {
	tightest, left := policy.Scope{}, int64(-1)
	for _, sc := range res.Scopes {
		if sc.BudgetMicros <= 0 {
			continue
		}
		remaining := max(sc.BudgetMicros-s.budgets.Spent(sc.Type, sc.ID, sc.Period, now), 0)
		if left < 0 || remaining < left {
			tightest, left = sc, remaining
		}
	}
	if left < 0 {
		return
	}
	h.Set("X-Keera-Budget-Scope", string(tightest.Type))
	h.Set("X-Keera-Budget-Limit", policy.FormatMicros(tightest.BudgetMicros))
	h.Set("X-Keera-Budget-Remaining", policy.FormatMicros(left))
	h.Set("X-Keera-Budget-Reset", tightest.Period.Next(now).Format(time.RFC3339))
	if s.opts.Currency != "" {
		h.Set("X-Keera-Budget-Currency", s.opts.Currency)
	}
}

// rateHeaders uses the OpenAI header names, so clients that already read them
// need nothing new.
func (s *Server) rateHeaders(h http.Header, res *policy.Resolved, now time.Time) {
	tightest, left := policy.Scope{}, -1
	for _, sc := range res.Scopes {
		if sc.RPM <= 0 {
			continue
		}
		remaining := s.limiter.Remaining(bucketKey(sc, "rpm"), sc.RPM, now)
		if left < 0 || remaining < left {
			tightest, left = sc, remaining
		}
	}
	if left < 0 {
		return
	}
	h.Set("X-RateLimit-Limit-Requests", strconv.Itoa(tightest.RPM))
	h.Set("X-RateLimit-Remaining-Requests", strconv.Itoa(left))
	h.Set("X-Keera-RateLimit-Scope", string(tightest.Type))
}

// bucketKey names one scope's bucket for one kind of rate limit.
func bucketKey(sc policy.Scope, limit string) string {
	return string(sc.Type) + ":" + sc.ID + "|" + limit
}

func retryAfter(w http.ResponseWriter, d time.Duration) {
	secs := max(int(d.Seconds()), 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}
