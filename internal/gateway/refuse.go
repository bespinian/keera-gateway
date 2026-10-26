package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/ratelimit"
	"github.com/bespinian/keera-gateway/internal/store"
)

// refusal is a request the gateway will not forward.
type refusal struct {
	status int
	typ    string
	code   string
	msg    string
	// advise adds the "where to look" sentence for the client only; the
	// usage row keeps just the refusal.
	advise bool
	// retry is how long the client should wait, for limits that lift on their
	// own.
	retry time.Duration
}

// refuse answers a request the gateway turned away, and records it. A refusal
// gets a usage row, since developers ask about it later. The model is not
// charged; what a router or filter spent is on the row.
func (s *Server) refuse(c *call, ref refusal) {
	s.limitHeaders(c.w, c.res, c.tr.start)
	if ref.retry > 0 {
		httpx.SetRetryAfter(c.w, ref.retry)
	}
	c.surf.shape.writeError(c.w, ref.status, ref.typ, ref.code, s.told(ref))

	ev := c.ev
	ev.Status = ref.status
	// The message says which guardrail stopped the request and its setting.
	ev.Error = ref.msg
	// The trace step it stopped in is closed and named by the code.
	s.closeEarly(c, ev, ref.code, metricModel(s.src, ev.OrgID, ev.Alias))
}

// closeEarly records a request that ended before a model answered. The row
// carries what the router and filters already charged to the budgets.
func (s *Server) closeEarly(c *call, ev store.Event, note, metricAlias string) {
	ev.Latency = time.Since(c.tr.start)
	ev.CostMicros = c.hookMicros()
	ev.Bills = c.hookBills()
	ev.Scopes = c.scopes
	ev.Spans = c.tr.steps(note)
	s.record(ev)
	s.metrics.Observe(metricAlias, ev.OrgID, ev.Status, ev.Latency.Seconds(), 0)
}

// statusClientClosed is the status of a request the client gave up on before
// an answer, as nginx names it.
const statusClientClosed = 499

// hungUp records a request whose client left before it was answered.
func (s *Server) hungUp(c *call, while, note string) {
	ev := c.ev
	ev.Status = statusClientClosed
	ev.Canceled = true
	ev.Error = appendNote(appendNote(ev.Error, "the client hung up "+while), note)
	s.closeEarly(c, ev, "client_closed", ev.Alias)
}

// unknownModel is the model label for a name that is none of the
// organisation's models or routers.
const unknownModel = "unknown"

// metricModel limits the model label to the organisation's models and
// routers. Metric series are never cleaned up, so free client text as a label
// would grow the registry until memory runs out.
func metricModel(src policy.Source, orgID, alias string) string {
	if alias == "" {
		return ""
	}
	if _, found := src.Model(orgID, alias); found {
		return alias
	}
	if _, found := src.Router(orgID, alias); found {
		return alias
	}
	return unknownModel
}

// advise appends where to look. In an editor, this message is the only way a
// developer learns of the panel.
func (s *Server) advise(msg string) string {
	if s.opts.PanelURL == "" {
		return msg
	}
	return msg + ". Your keys and their limits are at " + s.opts.PanelURL + "/access"
}

// told is what the client is told of a refusal.
func (s *Server) told(ref refusal) string {
	if ref.advise {
		return s.advise(ref.msg)
	}
	return ref.msg
}

// checkRates checks the rate limits of every scope, so org-wide and per-key
// limits both hold. All are decided at once: charging outer scopes and then
// refusing at an inner one would spend allowance on a request never sent.
// Outer scopes come first, and requests before tokens, so the reported limit
// is the one an administrator expects. It only decides; refuse answers.
func (s *Server) checkRates(scopes []policy.Scope, now time.Time) (refusal, bool) {
	type check struct {
		scope policy.Scope
		limit int
		unit  string
		code  string
	}
	reqs := make([]ratelimit.Requirement, 0, len(scopes)*2)
	checks := make([]check, 0, len(scopes)*2)
	for _, sc := range scopes {
		reqs = append(reqs,
			ratelimit.Requirement{Key: bucketKey(sc, "rpm"), PerMinute: sc.RPM, Take: true},
			tpmBucket(sc))
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

// checkQuotas checks the daily and monthly rate limits of guardrails and
// routers, counted in the budget windows. Like checkRates, it only decides.
// It answers 429 with a Retry-After of when the window resets.
func (s *Server) checkQuotas(scopes []policy.Scope, now time.Time) (refusal, bool) {
	for _, sc := range scopes {
		if sc.Quota <= 0 {
			continue
		}
		if s.budgets.Spent(sc.Type, sc.ID, sc.Period, now) < sc.Quota {
			continue
		}
		resets := sc.Period.Next(now)
		unit := string(sc.Meter)
		if sc.Quota == 1 {
			unit = strings.TrimSuffix(unit, "s")
		}
		return refusal{
			status: http.StatusTooManyRequests,
			typ:    "rate_limit_error", code: string(sc.Meter) + "_quota_exceeded",
			msg: fmt.Sprintf("%s is over its limit of %d %s per %s. It resets on %s (UTC)",
				sc.Type.Possessive(), sc.Quota, unit, sc.Period,
				resets.UTC().Format("2 January 2006 at 15:04")),
			advise: true,
			retry:  resets.Sub(now),
		}, false
	}
	return refusal{}, true
}

// tpmBucket is a scope's token bucket, checked before sending and charged
// once the tokens are counted.
func tpmBucket(sc policy.Scope) ratelimit.Requirement {
	return ratelimit.Requirement{Key: bucketKey(sc, "tpm"), PerMinute: sc.TPM}
}

// rateMessage names the limit that was hit and its value.
func rateMessage(sc policy.Scope, limit int, unit string) string {
	return fmt.Sprintf("%s is over its rate limit of %d %s per minute",
		sc.Type.Possessive(), limit, unit)
}

// limitHeaders states what is left of this key's allowance, on every answer,
// so a client can see a limit coming. Only the tightest scope is reported.
func (s *Server) limitHeaders(w http.ResponseWriter, res *policy.Resolved, now time.Time) {
	s.budgetHeaders(w.Header(), res, now)
	s.rateHeaders(w.Header(), res, now)
}

func (s *Server) budgetHeaders(h http.Header, res *policy.Resolved, now time.Time) {
	sc, left, ok := tightest(res.Scopes, func(sc policy.Scope) (int64, bool) {
		if sc.BudgetMicros <= 0 {
			return 0, false
		}
		return max(sc.BudgetMicros-s.budgets.Spent(sc.Type, sc.ID, sc.Period, now), 0), true
	})
	if !ok {
		return
	}
	h.Set("X-Keera-Budget-Scope", string(sc.Type))
	h.Set("X-Keera-Budget-Limit", policy.FormatMicros(sc.BudgetMicros))
	h.Set("X-Keera-Budget-Remaining", policy.FormatMicros(left))
	h.Set("X-Keera-Budget-Reset", sc.Period.Next(now).Format(time.RFC3339))
}

// rateHeaders uses the OpenAI header names, which clients already read.
func (s *Server) rateHeaders(h http.Header, res *policy.Resolved, now time.Time) {
	sc, left, ok := tightest(res.Scopes, func(sc policy.Scope) (int, bool) {
		if sc.RPM <= 0 {
			return 0, false
		}
		return s.limiter.Remaining(bucketKey(sc, "rpm"), sc.RPM, now), true
	})
	if !ok {
		return
	}
	h.Set("X-RateLimit-Limit-Requests", strconv.Itoa(sc.RPM))
	h.Set("X-RateLimit-Remaining-Requests", strconv.Itoa(left))
	h.Set("X-Keera-RateLimit-Scope", string(sc.Type))
}

// tightest is the scope with the least left of one limit, among the scopes
// that set it. left says how much a scope has left, or false if it sets none.
func tightest[N int | int64](scopes []policy.Scope,
	left func(policy.Scope) (N, bool),
) (best policy.Scope, least N, found bool) {
	for _, sc := range scopes {
		if n, ok := left(sc); ok && (!found || n < least) {
			best, least, found = sc, n, true
		}
	}
	return best, least, found
}

// bucketKey names one scope's bucket for one kind of rate limit.
func bucketKey(sc policy.Scope, limit string) string {
	return string(sc.Type) + ":" + sc.ID + "|" + limit
}
