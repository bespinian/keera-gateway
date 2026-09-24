package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// RouterProbe is what one router check found.
type RouterProbe struct {
	Alias string `json:"alias"`
	// Mode says which of the two checks this is: where the router would send
	// things, or which of its destinations are up.
	Mode  policy.RouterMode `json:"mode"`
	Model string            `json:"model,omitempty"`
	// Destinations is what the router was offering when checked, which may be
	// fewer than it was written with. A router quietly choosing between two of
	// its four destinations is easily mistaken for one that works.
	Destinations []string `json:"destinations"`
	// Dropped names the destinations that were not offered, and why.
	Dropped []string `json:"dropped,omitempty"`
	// Decisions is what an instruction router did with each sample prompt.
	Decisions []RouterDecision `json:"decisions,omitempty"`
	// Chain is a trying router's half: each destination in the order it would
	// be tried, and whether it would answer.
	Chain   []RouterHop `json:"chain,omitempty"`
	TotalMS int64       `json:"total_ms,omitempty"`
	// CostMicros is what these runs cost, which is also what the router adds
	// to every request that names it.
	CostMicros int64  `json:"cost_micros,omitempty"`
	Error      string `json:"error,omitempty"`
	// Warnings are problems that are not failures, chiefly a router that
	// answered the same way for every sample.
	Warnings []string `json:"warnings,omitempty"`
	OK       bool     `json:"ok"`
}

// RouterDecision is one sample prompt and where the router sent it.
type RouterDecision struct {
	Prompt string `json:"prompt"`
	// Asks says in a few words what the sample tests, so the destination next
	// to it can be judged.
	Asks   string `json:"asks"`
	Chosen string `json:"chosen,omitempty"`
	// Confidence is the share of probability the chosen destination took,
	// from 0 to 1. A router at 0.3 on every sample cannot tell its
	// destinations apart and may flip with a small change. Zero means the
	// backend served no logprobs, so it is unknown. See choice.go.
	Confidence float64 `json:"confidence,omitempty"`
	// Error is a decision the router could not make. On a real request this is
	// the fallback, or the refusal.
	Error string `json:"error,omitempty"`
}

// RouterHop is one destination of a trying router, asked whether it is there.
//
// The order is the point: a chain whose second destination is down is fine,
// while one whose first is down quietly runs on its second choice and pays for
// it.
type RouterHop struct {
	Alias string `json:"alias"`
	// Answers is a lower bar than a model check: it answered, and not with its
	// own failure. A model that is up but bad at tool calls would still be used.
	Answers bool  `json:"answers"`
	Status  int   `json:"status,omitempty"`
	MS      int64 `json:"ms,omitempty"`
	// Note is what a model check found beyond reachability (a failed tool
	// call, a different served model), or why it did not answer.
	Note string `json:"note,omitempty"`
	// Reading says why this destination is where it is, on a router that
	// ranks by measurement or by size. It is empty on a fallback router, whose
	// order was written by hand. It is prose because it is one replica's view
	// of the last few minutes. See loads.describe.
	Reading string `json:"reading,omitempty"`
}

// routerCheckSample is what a check asks a router to place: a trivial
// request, one that needs reasoning, and one carrying data that may not
// leave. They test three separate things, since an instruction can get one
// right and another wrong.
var routerCheckSample = []struct{ prompt, asks string }{
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

// CheckRouter runs one router over a sample of prompts and reports where each
// one went.
//
// A wrong router never fails a request: it answers every one from the wrong
// model. Too eager, and renaming variables costs the large model's price; too
// shy, and account numbers go to a hosted endpoint.
//
// Like a model or filter check, this is not a tenant's request: no rate limit,
// budget, billing or usage row. Its cost is reported, since every request
// naming the router pays the same.
func (s *Server) CheckRouter(ctx context.Context, rt policy.Router) RouterProbe {
	p := RouterProbe{Alias: rt.Alias, Mode: rt.Mode}
	if !rt.Decides() {
		return s.checkChain(ctx, rt)
	}
	p.Model = rt.Model

	p.Destinations, p.Dropped = s.sortDestinations(rt)
	for _, alias := range p.Destinations {
		if m, _ := s.src.Model(alias); m.Description == "" {
			p.Warnings = append(p.Warnings, "the destination '"+alias+"' has no description, "+
				"so the only thing the router is told about it is its name. A model's "+
				"description is what a routing decision is made on: set one with "+
				"`keera model set "+alias+" --description ...`")
		}
	}

	m, found := s.src.Model(rt.Model)
	if p.Error = routerCheckProblem(rt, p.Destinations, m, found); p.Error != "" {
		return p
	}

	// Only reachable destinations are offered, exactly as on a real request.
	offered := rt
	offered.Destinations = p.Destinations

	start := time.Now()
	for _, sample := range routerCheckSample {
		chose, err := s.decide(ctx, offered, m, []string{sample.prompt})
		p.CostMicros += chose.micros
		d := RouterDecision{Prompt: sample.prompt, Asks: sample.asks}
		if err != nil {
			d.Error = err.Error()
		}
		d.Chosen = chose.alias
		d.Confidence = chose.confidence
		p.Decisions = append(p.Decisions, d)
	}
	p.TotalMS = time.Since(start).Milliseconds()
	p.OK = true
	p.Warnings = append(p.Warnings, routerWarnings(p.Decisions, rt)...)
	return p
}

// sortDestinations splits a router's destinations into the ones that can be
// served and, for the rest, the reason they cannot.
func (s *Server) sortDestinations(rt policy.Router) (kept, dropped []string) {
	for _, alias := range rt.Destinations {
		m, ok := s.src.Model(alias)
		switch {
		case !ok:
			dropped = append(dropped, alias+" is not in the catalogue")
		case !m.Enabled:
			dropped = append(dropped, alias+" is disabled")
		case m.Kind != policy.KindChat:
			dropped = append(dropped, alias+" is a "+string(m.Kind)+" model")
		case len(m.Backends) == 0:
			dropped = append(dropped, alias+" has no backend")
		default:
			kept = append(kept, alias)
		}
	}
	return kept, dropped
}

// routerCheckProblem says why an instruction router cannot be checked, or
// returns empty when it can.
func routerCheckProblem(rt policy.Router, destinations []string, m policy.Model, found bool) string {
	switch {
	case len(destinations) == 0:
		return "none of this router's destinations can be reached, so it would send every " +
			"request to its fallback or refuse it"
	case len(destinations) == 1:
		return "only one of this router's destinations can be reached ('" +
			destinations[0] + "'), so there is no decision left for it to make"
	case !found:
		return "the model '" + rt.Model + "' is not in the catalogue any more, so this " +
			"router could not decide anything"
	case !m.Enabled:
		return "the model '" + rt.Model + "' is disabled, so this router could not decide " +
			"anything"
	case m.Kind != policy.KindChat:
		return "the model '" + rt.Model + "' is a " + string(m.Kind) + " model; a router " +
			"reads text and answers with a name, which only a chat model does"
	case len(m.Backends) == 0:
		return "the model '" + rt.Model + "' has no backend configured"
	}
	return ""
}

// checkChain asks each of a trying router's destinations whether it is there,
// in the order they would be tried.
//
// It is a reading of the inference plane at one moment. What it shows is the
// state nothing else does: a chain quietly running on its second destination
// at that destination's price. On a latency or least-busy router the order is
// itself a measurement, so two checks a minute apart may differ.
func (s *Server) checkChain(ctx context.Context, rt policy.Router) RouterProbe {
	p := RouterProbe{Alias: rt.Alias, Mode: rt.Mode}
	p.Destinations, p.Dropped = s.sortDestinations(rt)
	// Ranked once, so the whole check describes one ranking.
	s.load.order(rt.Mode, p.Destinations)
	if len(p.Destinations) == 0 {
		p.Error = "none of this router's destinations can be reached, so every request " +
			"naming it is refused"
		return p
	}
	// A size router's order depends on the request's size, so all its bands
	// are reported, in the order a request meets them.
	var bands []sizeBand
	if rt.Sizes() {
		bands = sizeBands(rt, p.Destinations)
		p.Destinations = p.Destinations[:0]
		for _, b := range bands {
			p.Destinations = append(p.Destinations, b.alias)
		}
	}

	start := time.Now()
	for i, alias := range p.Destinations {
		m, _ := s.src.Model(alias)
		probe := s.CheckModel(ctx, m)
		reading := s.load.describe(rt.Mode, alias)
		if bands != nil {
			reading = describeBand(bands[i])
		}
		p.Chain = append(p.Chain, routerHop(alias, reading, probe))
	}
	p.TotalMS = time.Since(start).Milliseconds()
	p.OK = true
	if bands != nil {
		models := make(map[string]policy.Model, len(p.Destinations))
		for _, alias := range p.Destinations {
			if m, ok := s.src.Model(alias); ok {
				models[alias] = m
			}
		}
		p.Warnings = append(p.Warnings, sizeWarnings(rt, bands, models)...)
	}
	p.Warnings = append(p.Warnings, chainWarnings(p, rt)...)
	return p
}

// routerHop turns a model check into one hop of a chain.
func routerHop(alias, reading string, probe Probe) RouterHop {
	hop := RouterHop{
		Alias:   alias,
		Reading: reading,
		// A chain only needs an answer that is not the destination's own
		// failure. Failing over on less would move traffic off a working model.
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

// chainWarnings says what is worth knowing about a chain before its hops are
// read.
func chainWarnings(p RouterProbe, rt policy.Router) []string {
	var (
		warnings []string
		up       []string
	)
	for _, hop := range p.Chain {
		if hop.Answers {
			up = append(up, hop.Alias)
		}
	}
	switch {
	case len(up) == 0:
		warnings = append(warnings, "not one destination in this chain answered. Every "+
			"request naming this router is being served whatever the last of them says.")
	case rt.Sizes():
		// Traffic away from the first destination is the router working. With
		// only one destination up, though, it sorts nothing.
		if len(up) == 1 {
			warnings = append(warnings, "only '"+up[0]+"' is answering, so every request is "+
				"going there whatever its size. Until another destination comes back, this "+
				"router is a name for that model.")
		}
	case rt.Measures():
		// The order is a measurement, so a different first choice is the
		// router doing its job. Only "nothing left behind it" is worth saying.
		if len(up) == 1 {
			warnings = append(warnings, "only '"+up[0]+"' is answering, so this router is "+
				"placing every request on it however it ranks them. There is nothing behind "+
				"it: until another destination comes back, this router is a name for that "+
				"model.")
		}
	case up[0] != p.Chain[0].Alias:
		warnings = append(warnings, "the first destination, '"+p.Chain[0].Alias+"', is not "+
			"answering, so this router is placing every request on '"+up[0]+"'. Nothing about "+
			"those requests says so - they succeed, at whatever the second choice costs.")
	case len(up) == 1:
		warnings = append(warnings, "only '"+up[0]+"' is answering, and it is the one this "+
			"chain is placing requests on. There is nothing behind it: this router is "+
			"currently a name for that model.")
	}
	return warnings
}

// routerWarnings says what a router that answered the samples still got
// wrong. The key one: a router sending a variable rename and a database
// migration to the same model is an expensive way to hard-code a destination.
func routerWarnings(decisions []RouterDecision, rt policy.Router) []string {
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
		warnings = append(warnings, "the router could not place any of the samples. On a real "+
			"request each of these is "+undecidedBecomes(rt)+".")
	case failed > 0:
		warnings = append(warnings, fmt.Sprintf("the router could not place %d of the %d "+
			"samples. On a real request each of those is %s.",
			failed, len(decisions), undecidedBecomes(rt)))
	}
	if len(distinct) == 1 && failed == 0 {
		for alias := range distinct {
			warnings = append(warnings, "every sample was sent to '"+alias+"'. The samples ask "+
				"for very different things, so a router that answers them all the same way is "+
				"not deciding anything - which costs a generation per request and moves no "+
				"traffic. Its instruction is the thing to change.")
		}
	}
	return warnings
}

// undecidedBecomes says what happens to a request this router cannot place.
func undecidedBecomes(rt policy.Router) string {
	if rt.Refuses() {
		return "refused, because this router has no fallback destination"
	}
	return "sent to '" + rt.Fallback + "', this router's fallback"
}
