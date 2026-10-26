package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// RouteProbe is what checking one route step found.
type RouteProbe struct {
	Step string `json:"step"`
	// Mode says which check this is: where the step would send requests, or
	// which of its destinations are up.
	Mode  policy.RouteMode `json:"mode"`
	Model string           `json:"model,omitempty"`
	// Destinations is what the step offered when checked, which may be fewer
	// than configured.
	Destinations []string `json:"destinations"`
	// Dropped names the destinations not offered, and why.
	Dropped []string `json:"dropped,omitempty"`
	// Decisions is what an instruction step did with each sample prompt.
	Decisions []SampleDecision `json:"decisions,omitempty"`
	// Chain is each destination of a trying step, in try order, and whether
	// it would answer.
	Chain   []RouteHop `json:"chain,omitempty"`
	TotalMS int64      `json:"total_ms,omitempty"`
	// CostMicros is what these runs cost, which is also what the step adds
	// to every request.
	CostMicros int64  `json:"cost_micros,omitempty"`
	Error      string `json:"error,omitempty"`
	// Warnings are problems that are not failures, mainly a step that
	// answered the same for every sample.
	Warnings []string `json:"warnings,omitempty"`
	OK       bool     `json:"ok"`
}

// SampleDecision is one sample prompt and where the step sent it.
type SampleDecision struct {
	Prompt string `json:"prompt"`
	// Asks says in a few words what the sample tests.
	Asks   string `json:"asks"`
	Chosen string `json:"chosen,omitempty"`
	// Confidence is the chosen destination's share of probability, from 0 to
	// 1. A step near 0.3 on every sample cannot tell its destinations apart.
	// Zero means unknown: the backend served no logprobs. See choice.go.
	Confidence float64 `json:"confidence,omitempty"`
	// Error is a decision the step could not make. On a real request this
	// means the fallback, or a refusal.
	Error string `json:"error,omitempty"`
}

// RouteHop is one destination of a trying step, checked for whether it
// answers. Order matters: a chain whose first destination is down quietly
// runs on its second, at that one's price.
type RouteHop struct {
	Alias string `json:"alias"`
	// Answers is a lower bar than a model check: it answered without failing.
	// A model that is up but bad at tool calls would still be used.
	Answers bool  `json:"answers"`
	Status  int   `json:"status,omitempty"`
	MS      int64 `json:"ms,omitempty"`
	// Note is what a model check found beyond reachability, or why it did
	// not answer.
	Note string `json:"note,omitempty"`
	// Reading says why this destination is ranked here, on a step that ranks
	// by measurement or size. It is one replica's view of the last few
	// minutes. See loads.describe.
	Reading string `json:"reading,omitempty"`
}

// routeCheckSamples is what a check asks a route step to place: a trivial
// request, one that needs reasoning, and one with data that must not leave.
var routeCheckSamples = []struct{ prompt, asks string }{
	{
		"rename the variable `tmp2` in this function to something readable",
		"a trivial edit - the cheapest model that can do it",
	},
	{
		"We need to move a 400GB Postgres table to a new schema with a changed " +
			"primary key, online, with no more than a minute of write downtime. " +
			"Walk me through the migration and what you would do when the backfill " +
			"falls behind the write rate.",
		"a request that wants a model that can reason",
	},
	{
		"Here is the reconciliation export for Helvetia Versicherung - account " +
			"CH93 0076 2011 6238 5295 7, contract 44192-B. Which of these rows " +
			"failed to match and why?",
		"a request carrying a named client and an account number",
	},
}

// CheckRoute runs one route step over sample prompts and reports where each
// went. A wrong router never fails a request; it silently picks the wrong
// model. Like other checks, it has no rate limit, budget or usage row, but a
// model on the deployment's key needs credit and is billed.
func (s *Server) CheckRoute(ctx context.Context, rt policy.Route) RouteProbe {
	p := RouteProbe{Step: rt.Step, Mode: rt.Mode}
	if rt.Mode == policy.RouteModeHeader {
		return s.checkHeader(ctx, rt)
	}
	if !rt.Decides() {
		return s.checkChain(ctx, rt)
	}
	p.Model = rt.Model

	p.Destinations, p.Dropped = s.sortDestinations(rt)
	for _, alias := range p.Destinations {
		if m, _ := s.src.Model(rt.OrgID, alias); m.Description == "" {
			p.Warnings = append(p.Warnings, "the destination '"+alias+"' has no description, "+
				"so the only thing the step is told about it is its name. A model's "+
				"description is what a routing decision is made on: set one with "+
				"`keera model set "+alias+" --description ...`")
		}
	}

	m, found := s.src.Model(rt.OrgID, rt.Model)
	if p.Error = routeCheckProblem(rt, p.Destinations, m, found); p.Error != "" {
		return p
	}
	if p.Error = s.checkCredit(rt.OrgID, m); p.Error != "" {
		return p
	}

	start := time.Now()
	for _, sample := range routeCheckSamples {
		chose, err := s.decide(ctx, rt, m, p.Destinations, []string{sample.prompt})
		p.CostMicros += chose.micros
		s.billCheck(rt.OrgID, chose.bills)
		d := SampleDecision{
			Prompt: sample.prompt, Asks: sample.asks, Chosen: chose.alias,
			Confidence: chose.confidence,
		}
		if err != nil {
			d.Error = err.Error()
		}
		p.Decisions = append(p.Decisions, d)
	}
	p.TotalMS = time.Since(start).Milliseconds()
	p.OK = true
	p.Warnings = append(p.Warnings, sampleWarnings(p.Decisions, rt)...)
	return p
}

// sortDestinations splits a route step's destinations into the ones that can
// be served and, for the rest, the reason they cannot.
func (s *Server) sortDestinations(rt policy.Route) (kept, dropped []string) {
	for _, alias := range rt.Destinations {
		m, ok := s.src.Model(rt.OrgID, alias)
		if why := chatProblem(m, ok); why != "" {
			dropped = append(dropped, alias+" "+why)
		} else {
			kept = append(kept, alias)
		}
	}
	return kept, dropped
}

// routeCheckProblem says why an instruction step cannot be checked, or "".
func routeCheckProblem(rt policy.Route, destinations []string, m policy.Model, found bool) string {
	switch {
	case len(destinations) == 0:
		return "none of this step's destinations can be reached, so it would send every " +
			"request to its fallback or refuse it"
	case len(destinations) == 1:
		return "only one of this step's destinations can be reached ('" +
			destinations[0] + "'), so there is no decision left for it to make"
	}
	if why := readerProblem(m, found, true); why != "" {
		return "the model '" + rt.Model + "' " + why + ", so this step could not decide anything"
	}
	return ""
}

// checkChain checks each of a trying step's destinations, in try order. On a
// latency or least-busy step the order is a measurement, so two checks a
// minute apart may differ.
func (s *Server) checkChain(ctx context.Context, rt policy.Route) RouteProbe {
	p := RouteProbe{Step: rt.Step, Mode: rt.Mode}
	if rt.Mode.Decides() {
		// An instruction step with one destination tries it like a fallback
		// step.
		p.Mode = policy.RouteModeFallback
	}
	p.Destinations, p.Dropped = s.sortDestinations(rt)
	// Ranked once, so the whole check describes one ranking.
	s.orderByLoad(rt, p.Destinations)
	if len(p.Destinations) == 0 {
		p.Error = "none of this step's destinations can be reached, so every request " +
			"through it is refused"
		return p
	}
	// A size step's order depends on request size, so all bands are reported.
	var bands []sizeBand
	if rt.Sizes() {
		bands = sizeBands(rt, p.Destinations)
		p.Destinations = p.Destinations[:0]
		for _, b := range bands {
			p.Destinations = append(p.Destinations, b.alias)
		}
	}

	start := time.Now()
	models := make(map[string]policy.Model, len(p.Destinations))
	for i, alias := range p.Destinations {
		m, found := s.src.Model(rt.OrgID, alias)
		if found {
			models[alias] = m
		}
		probe := s.CheckModel(ctx, m)
		reading := s.load.describe(rt.Mode, m.Key())
		if bands != nil {
			reading = describeBand(bands[i])
		}
		p.Chain = append(p.Chain, chainHop(alias, reading, probe))
	}
	p.TotalMS = time.Since(start).Milliseconds()
	p.OK = true
	if bands != nil {
		p.Warnings = append(p.Warnings, sizeWarnings(rt, bands, models)...)
	}
	p.Warnings = append(p.Warnings, chainWarnings(p, rt)...)
	return p
}

// chainHop turns a model check into one hop of a chain.
func chainHop(alias, reading string, probe Probe) RouteHop {
	hop := RouteHop{
		Alias:   alias,
		Reading: reading,
		// Failing over on anything less than a 5xx would move traffic off a
		// working model.
		Answers: probe.Reachable && probe.Status < 500,
		Status:  probe.Status,
		MS:      probe.TotalMS,
	}
	switch {
	case !hop.Answers:
		hop.Note = probe.Error
		if hop.Note == "" {
			hop.Note = "it answered " + strconv.Itoa(probe.Status)
		}
	case !probe.OK && probe.Error != "":
		// It would take traffic, and something else is wrong with it.
		hop.Note = probe.Error
	case len(probe.Warnings) > 0:
		hop.Note = strings.Join(probe.Warnings, " ")
	}
	return hop
}

func chainWarnings(p RouteProbe, rt policy.Route) []string {
	var up []string
	for _, hop := range p.Chain {
		if hop.Answers {
			up = append(up, hop.Alias)
		}
	}
	// A size, latency or least-busy step moving traffic off its first
	// destination is working as meant. Only in a fixed order does it mean
	// the first is down.
	written := !rt.Sizes() && !rt.Measures()
	switch {
	case len(up) == 0:
		return []string{"not one destination in this chain answered. Every request " +
			"through this step is being served whatever the last of them says."}
	case written && up[0] != p.Chain[0].Alias:
		return []string{"the first destination, '" + p.Chain[0].Alias + "', is not " +
			"answering, so this step is placing every request on '" + up[0] + "'. Nothing " +
			"about those requests says so - they succeed, at whatever the second choice costs."}
	case len(up) == 1:
		return []string{"only '" + up[0] + "' is answering, so every request through this " +
			"step goes there. There is nothing behind it: until another destination comes " +
			"back, this step is a name for that model."}
	}
	return nil
}

// sampleWarnings says what a route step that answered the samples still got
// wrong, mainly sending every sample to the same model.
func sampleWarnings(decisions []SampleDecision, rt policy.Route) []string {
	var (
		warnings []string
		distinct = map[string]int{}
		failed   int
	)
	for _, d := range decisions {
		if d.Error != "" {
			failed++
			continue
		}
		distinct[d.Chosen]++
	}
	switch {
	case failed == len(decisions):
		warnings = append(warnings, "the step could not place any of the samples. On a real "+
			"request each of these is "+undecidedBecomes(rt)+".")
	case failed > 0:
		warnings = append(warnings, fmt.Sprintf("the step could not place %d of the %d "+
			"samples. On a real request each of those is %s.",
			failed, len(decisions), undecidedBecomes(rt)))
	}
	if len(distinct) == 1 && failed == 0 {
		for alias := range distinct {
			warnings = append(warnings, "every sample was sent to '"+alias+"'. The samples ask "+
				"for very different things, so a route step that answers them all the same way is "+
				"not deciding anything - which costs a generation per request and moves no "+
				"traffic. Its instruction is the thing to change.")
		}
	}
	return warnings
}

// undecidedBecomes says what happens to a request this step cannot place.
func undecidedBecomes(rt policy.Route) string {
	if rt.Refuses() {
		return "refused, because this step has no fallback destination"
	}
	return "sent to '" + rt.Fallback + "', this step's fallback"
}
