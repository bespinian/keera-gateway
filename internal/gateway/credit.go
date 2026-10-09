package gateway

import (
	"slices"
	"time"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// onKey reports whether a request may call a model on the deployment's own
// provider key, which an organisation may have to pay for in advance: the
// model it names, any its router decides with or may choose, or a filter's.
// It is asked before anything is spent, so a router that might pick such a
// model counts even if it would pick another.
func (s *Server) onKey(c *call) bool {
	return len(s.keyModels(c)) > 0 || s.filtersOnKey(c.res.Key.OrgID, c.res.Filters)
}

// keyModels is the models on the deployment's key a request may be answered
// by, or decided with.
func (s *Server) keyModels(c *call) []policy.Model {
	if m, ok := s.keyDecider(c); ok {
		return append(s.keyAnswerers(c), m)
	}
	return s.keyAnswerers(c)
}

// keyAnswerers is the models on the deployment's key a request may be
// answered by: the one it names, or every one its router may choose.
func (s *Server) keyAnswerers(c *call) []policy.Model {
	if !c.routed {
		if c.model.PlatformKey {
			return []policy.Model{c.model}
		}
		return nil
	}
	var out []policy.Model
	for _, alias := range append([]string{c.router.Fallback}, c.router.Destinations...) {
		if m, ok := s.keyModel(c.res.Key.OrgID, alias); ok {
			out = append(out, m)
		}
	}
	return out
}

// keyDecider is the model a request's router decides with, when it is on the
// deployment's key.
func (s *Server) keyDecider(c *call) (policy.Model, bool) {
	if !c.routed {
		return policy.Model{}, false
	}
	return s.keyModel(c.res.Key.OrgID, c.router.Model)
}

// filtersOnKey reports whether one of the filters runs on a model on the
// deployment's key.
func (s *Server) filtersOnKey(org string, filters []string) bool {
	return len(s.keyFilterModels(org, filters)) > 0
}

// keyFilterModels is the models on the deployment's key the filters run on.
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

// unreportedTokens is what a call on the deployment's key is billed when its
// usage record never came, though the provider may have charged for it: an
// answer too large to read, or one that took longer than the gateway waits.
// It is billed all it may have used: its whole body as prompt and its whole
// output ceiling as answer. That errs high, so losing the record is never a
// way to pay less.
func unreportedTokens(bodyBytes, outputCeiling int) policy.Tokens {
	return policy.Tokens{Input: bodyBytes / bytesPerToken, Output: outputCeiling}
}

// billUnreported bills a request on the deployment's key that reached the
// provider and came back without a usage record. See unreportedTokens.
func (s *Server) billUnreported(c *call, b *body) {
	if !c.model.PlatformKey || c.ev.InputTokens+c.ev.OutputTokens > 0 {
		return
	}
	out := 0
	if c.surf.kind != policy.KindEmbedding {
		sent := b
		if sent == nil {
			sent = c.native
		}
		out = outputCeiling(sent, c.res.MaxOutputTokens)
	}
	t := unreportedTokens(c.received, out)
	c.ev.InputTokens, c.ev.OutputTokens, c.ev.Estimated = t.Input, t.Output, true
	s.log.Warn("a request on the deployment's key came back without a usage record; "+
		"it is billed the most it may have used", "model", c.alias,
		"request_id", httpx.RequestID(c.r.Context()))
}

// stripPremium takes out the fields that make a provider charge more per
// token than its list price: Anthropic's fast mode and data residency, and a
// service tier such as OpenAI's priority processing. The usage record counts
// the same tokens either way, so the request would be billed less than it
// cost. The model answers as it would without them. It returns what it took
// out.
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

// listPriceTiers are the service tiers that cost no more than the list price.
var listPriceTiers = []string{"auto", "default", "flex", "standard_only"}

// unstatedOutputTokens is what a request that sets no output ceiling is held
// for. It is not a bound: such a request may write up to what the model
// allows, so its hold can fall short.
const unstatedOutputTokens = 64_000

// mostOutputTokens bounds what a ceiling counts for, so a nonsense one cannot
// overflow the hold.
const mostOutputTokens = 10_000_000

// creditHold is the most a request may take from the credit: its whole body
// as prompt, its output ceiling as answer, at the dearest model on the key it
// may reach, plus its router's decision, asked twice, and a rewrite of the
// body by each filter on the key. It errs high, and is given back when the
// request has been charged.
func (s *Server) creditHold(c *call, b *body) int64 {
	in := c.received / bytesPerToken
	out := 0
	if c.surf.kind != policy.KindEmbedding {
		out = outputCeiling(b, c.res.MaxOutputTokens)
	}
	var most int64
	for _, m := range s.keyAnswerers(c) {
		if line, ok := m.Bill(policy.Tokens{Input: in, Output: out}); ok {
			most = max(most, line.CreditMicros)
		}
	}
	// A decision is billed apart from the answer. One asked by letter that
	// comes back unreadable is asked again by name.
	if m, ok := s.keyDecider(c); ok {
		if line, ok := m.Bill(policy.Tokens{Input: in, Output: routerOutputTokens}); ok {
			most += 2 * line.CreditMicros
		}
	}
	return most + s.filtersHold(c.res.Key.OrgID, c.res.Filters, in)
}

// filtersHold is the most the filters on the deployment's key may take from
// the credit reading in tokens: each may rewrite all of it.
func (s *Server) filtersHold(org string, filters []string, in int) int64 {
	var most int64
	for _, m := range s.keyFilterModels(org, filters) {
		if line, ok := m.Bill(policy.Tokens{Input: in, Output: in}); ok {
			most += line.CreditMicros
		}
	}
	return most
}

// outputCeiling is the most output tokens a request asks for, held to the
// guardrail's ceiling.
func outputCeiling(b *body, guardrail int) int {
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
	if guardrail > 0 {
		asked = min(asked, guardrail)
	}
	return asked
}

// checkCredit says why a check may not call these models, or is empty when
// it may: a model on the deployment's key needs credit, as a request does.
func (s *Server) checkCredit(org string, models ...policy.Model) string {
	if !slices.ContainsFunc(models, func(m policy.Model) bool { return m.PlatformKey }) {
		return ""
	}
	if err := s.budgets.AllowCredit(org); err != nil {
		return err.Error()
	}
	return ""
}

// billCheck bills what a check spent on the deployment's key. A check is not
// a request, so it writes no usage row, but its calls are paid for like any.
func (s *Server) billCheck(org string, bills []policy.BillLine) {
	if len(bills) == 0 {
		return
	}
	s.record(store.Event{TS: time.Now(), OrgID: org, Bills: bills, BillsOnly: true})
}

// record takes what a request used of the deployment's keys from the local
// view of its organisation's credit, and hands the event to the usage
// writer, which takes it from the credit for good.
func (s *Server) record(ev store.Event) {
	var credit int64
	for _, b := range ev.Bills {
		credit += b.CreditMicros
	}
	s.budgets.ChargeCredit(ev.OrgID, credit)
	s.sink.Record(ev)
}
