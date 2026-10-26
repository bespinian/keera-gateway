package gateway

import (
	"slices"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// onKey reports whether a request may call any model on the deployment's own
// provider key, which may need prepaid credit. It runs before anything is
// spent, so a model a route step might pick counts even if it picks another.
func (s *Server) onKey(c *call) bool {
	return len(s.keyDeciders(c)) > 0 || len(s.keyAnswerers(c)) > 0 ||
		s.filtersOnKey(c.res.Key.OrgID, s.callFilters(c))
}

// keyAnswerers is the models on the deployment's key that may answer a
// request: the one it names, or every one its router may reach.
func (s *Server) keyAnswerers(c *call) []policy.Model {
	if !c.viaRouter() {
		if c.model.PlatformKey {
			return []policy.Model{c.model}
		}
		return nil
	}
	var out []policy.Model
	for _, alias := range c.router.Reaches() {
		if m, ok := s.keyModel(c.res.Key.OrgID, alias); ok {
			out = append(out, m)
		}
	}
	return out
}

// keyDeciders is the deciding models of a request's route steps that are on
// the deployment's key.
func (s *Server) keyDeciders(c *call) []policy.Model {
	var out []policy.Model
	for _, rt := range c.router.Routes() {
		if !rt.Decides() {
			continue
		}
		if m, ok := s.keyModel(c.res.Key.OrgID, rt.Model); ok {
			out = append(out, m)
		}
	}
	return out
}

// callFilters is every filter a request may pass: its router's, then the key's.
func (s *Server) callFilters(c *call) []string {
	var out []string
	for step := range c.router.Steps() {
		if step.Kind == policy.StepFilter {
			out = append(out, step.Ref)
		}
	}
	return append(out, c.res.Filters...)
}

func (s *Server) filtersOnKey(org string, filters []string) bool {
	return len(s.keyFilterModels(org, filters)) > 0
}

func (s *Server) keyFilterModels(org string, filters []string) []policy.Model {
	var out []policy.Model
	for _, alias := range filters {
		f, ok := s.src.Filter(org, alias)
		if !ok || !f.UsesModel() {
			continue
		}
		if m, ok := s.keyModel(org, f.Model); ok {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) keyModel(org, alias string) (policy.Model, bool) {
	if alias == "" {
		return policy.Model{}, false
	}
	m, ok := s.src.Model(org, alias)
	return m, ok && m.PlatformKey
}

// mostTokens is all a call may use: its whole body as prompt and its output
// ceiling as answer. A call on the deployment's key with no usage record is
// billed this, so losing the record never means paying less.
func mostTokens(bodyBytes, outputCeiling int) policy.Tokens {
	return policy.Tokens{Input: bodyBytes / bytesPerToken, Output: outputCeiling}
}

// billUnreported bills a request on the deployment's key that came back
// without a usage record. See mostTokens.
func (s *Server) billUnreported(c *call, b *body) {
	if !c.model.PlatformKey || c.ev.InputTokens+c.ev.OutputTokens > 0 {
		return
	}
	t := mostTokens(c.received, c.outputTokens(c.sent(b)))
	c.ev.InputTokens, c.ev.OutputTokens, c.ev.Estimated = t.Input, t.Output, true
	s.log.Warn("a request on the deployment's key came back without a usage record; "+
		"it is billed the most it may have used", "model", c.alias,
		httpx.LogIDs(c.r.Context()))
}

// stripPremium removes and returns the fields that make a provider charge
// more than list price: Anthropic's fast mode and data residency, and dearer
// service tiers. The usage record would not show the extra cost.
func stripPremium(b *body) []string {
	var removed []string
	for _, field := range []string{"speed", "inference_geo"} {
		if b.remove(field) {
			removed = append(removed, field)
		}
	}
	if _, present := b.value("service_tier"); present {
		tier, _ := b.str("service_tier")
		if !slices.Contains(listPriceTiers, tier) {
			b.remove("service_tier")
			removed = append(removed, "service_tier")
		}
	}
	return removed
}

var listPriceTiers = []string{"auto", "default", "flex", "standard_only"}

// unstatedOutputTokens is held for a request with no output ceiling. It is not
// a bound, so the hold can fall short.
const unstatedOutputTokens = 64_000

// mostOutputTokens caps a ceiling so a nonsense one cannot overflow the hold.
const mostOutputTokens = 10_000_000

// creditHold is the most a request may take from the credit: mostTokens at the
// dearest model it may reach, plus each route step's decision twice, plus a
// full rewrite by each filter on the key. It errs high and is released once
// the request is charged.
func (s *Server) creditHold(c *call, b *body) int64 {
	t := mostTokens(c.received, c.outputTokens(b))
	in := t.Input
	var most int64
	for _, m := range s.keyAnswerers(c) {
		if line, ok := m.Bill(t); ok {
			most = max(most, line.CreditMicros)
		}
	}
	// An unreadable decision by letter is asked again by name.
	for _, m := range s.keyDeciders(c) {
		if line, ok := m.Bill(policy.Tokens{Input: in, Output: routeOutputTokens}); ok {
			most += 2 * line.CreditMicros
		}
	}
	return most + s.filtersHold(c.res.Key.OrgID, s.callFilters(c), in)
}

// filtersHold is the most the filters on the deployment's key may cost to read
// in tokens. Each may rewrite all of them.
func (s *Server) filtersHold(org string, filters []string, in int) int64 {
	var most int64
	for _, m := range s.keyFilterModels(org, filters) {
		if line, ok := m.Bill(policy.Tokens{Input: in, Output: in}); ok {
			most += line.CreditMicros
		}
	}
	return most
}

// outputTokens is the most a request may write; an embedding writes none.
func (c *call) outputTokens(b *body) int {
	if c.surf.kind == policy.KindEmbedding {
		return 0
	}
	return outputCeiling(b)
}

func outputCeiling(b *body) int {
	asked := 0
	if b != nil {
		for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			if v, ok := b.ceiling(field); ok && v > 0 {
				asked = max(asked, int(min(v, mostOutputTokens)))
			}
		}
	}
	if asked == 0 {
		asked = unstatedOutputTokens
	}
	return asked
}

// checkCredit says why a check may not call these models, or "" if it may. A
// model on the deployment's key needs credit, as for a request.
func (s *Server) checkCredit(org string, models ...policy.Model) string {
	if !slices.ContainsFunc(models, func(m policy.Model) bool { return m.PlatformKey }) {
		return ""
	}
	// Holding 0 only asks whether the organisation has credit.
	if _, err := s.budgets.HoldCredit(org, 0); err != nil {
		return err.Error()
	}
	return ""
}

// billCheck bills what a check spent on the deployment's key. It writes no
// usage row, since a check is not a request.
func (s *Server) billCheck(org string, bills []policy.BillLine) {
	if len(bills) == 0 {
		return
	}
	s.record(store.Event{TS: time.Now(), OrgID: org, Bills: bills, BillsOnly: true})
}

// record charges the local view of the organisation's credit and hands the
// event to the usage writer, which charges it for good.
func (s *Server) record(ev store.Event) {
	var credit int64
	for _, b := range ev.Bills {
		credit += b.CreditMicros
	}
	s.budgets.ChargeCredit(ev.OrgID, credit)
	s.sink.Record(ev)
}
