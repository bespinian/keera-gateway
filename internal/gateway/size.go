package gateway

import (
	"cmp"
	"math"
	"slices"
	"strconv"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A size step places a request by its length, since how much model a request
// needs usually follows it. It costs nothing, always answers the same, and a
// prompt cannot talk it into the large model. It cannot tell hard from easy
// at the same length and does not read the text, so keeping data in the
// cluster is a job for an instruction step or a filter.

// sizeTier is how well one destination suits a request, best first.
type sizeTier int

const (
	// sizeFits is a destination meant for this size that can hold it.
	sizeFits sizeTier = iota
	// sizeOverCeiling can hold the request but is meant for smaller ones. It
	// is used when the right size is down.
	sizeOverCeiling
	// sizeTooLarge cannot fit the request in its context, so it is last.
	sizeTooLarge
)

// sized places a request by how much text is in it. The chain is every
// serveable destination in preference order, so a larger model can answer
// when the preferred one is down.
func (s *Server) sized(rt policy.Route, b *body, kind policy.Kind) (routeDecision, *refusal) {
	d := routeDecision{outcome: store.RouterChose}

	doc, err := extractText(b, kind)
	if err != nil {
		return d, unreadableBody(stepSubject(rt)+" chooses the model by how much text is "+
			"in the request", err)
	}
	tokens := estimateTokens(doc.texts())

	type ranked struct {
		alias   string
		tier    sizeTier
		ceiling int
	}
	var order []ranked
	for _, alias := range rt.Destinations {
		m, ok := s.serveable(rt.OrgID, alias)
		if !ok {
			continue
		}
		ceiling, bounded := rt.Ceiling(alias)
		r := ranked{alias: alias, ceiling: ceiling, tier: sizeFits}
		switch {
		case m.MaxContext > 0 && tokens > m.MaxContext:
			r.tier = sizeTooLarge
		case bounded && tokens > ceiling:
			r.tier = sizeOverCeiling
		}
		order = append(order, r)
	}
	if len(order) == 0 {
		d.outcome = store.RouterError
		return d, s.nowhere(rt, stepSubject(rt)+" chooses between its destinations by how "+
			"much text is in the request, and none of them can answer one - every one is "+
			"missing, disabled, of another kind, or has no backend")
	}

	// Tier first, then by ceiling, then the written order.
	slices.SortStableFunc(order, func(a, c ranked) int {
		return cmp.Or(cmp.Compare(a.tier, c.tier), byCeiling(a.ceiling, c.ceiling))
	})

	for _, r := range order {
		d.chain = append(d.chain, r.alias)
	}
	d.alias = d.chain[0]
	return d, nil
}

// byCeiling puts the smallest ceiling (the cheaper model) first, and no
// ceiling last. A stable sort keeps ties in the written order.
func byCeiling(a, b int) int {
	if a == 0 {
		a = math.MaxInt
	}
	if b == 0 {
		b = math.MaxInt
	}
	return cmp.Compare(a, b)
}

// sizeBand is one destination's share of the size range, for the check. The
// bands describe a size step completely.
type sizeBand struct {
	alias string
	// from and to are the request sizes, in estimated tokens, this destination
	// is first choice for. A to of zero is unbounded.
	from, to int
	// dead says this destination is never first choice: its ceiling is no
	// larger than the one before.
	dead bool
}

// sizeBands works out which sizes each destination is first choice for, in
// sized's order when all are up: smallest ceiling first, unbounded last. Each
// takes everything above the previous one's ceiling.
func sizeBands(rt policy.Route, aliases []string) []sizeBand {
	type entry struct {
		alias   string
		ceiling int
	}
	order := make([]entry, 0, len(aliases))
	for _, alias := range aliases {
		ceiling, _ := rt.Ceiling(alias)
		order = append(order, entry{alias: alias, ceiling: ceiling})
	}
	slices.SortStableFunc(order, func(a, b entry) int { return byCeiling(a.ceiling, b.ceiling) })

	out := make([]sizeBand, 0, len(order))
	floor := 0
	for _, e := range order {
		band := sizeBand{alias: e.alias, from: floor, to: e.ceiling}
		// Never first choice, but kept: it takes traffic when earlier ones
		// are down.
		if e.ceiling > 0 && e.ceiling <= floor {
			band.dead = true
		} else if e.ceiling > 0 {
			floor = e.ceiling
		}
		out = append(out, band)
	}
	return out
}

// describeBand says in words what sizes a destination is first choice for.
func describeBand(b sizeBand) string {
	switch {
	case b.dead:
		return "nothing - its ceiling is no higher than the destination before it, so no " +
			"request is ever placed here first"
	case b.to == 0 && b.from == 0:
		return "every request - it has no ceiling and nothing is ranked ahead of it"
	case b.to == 0:
		return "from " + strconv.Itoa(b.from+1) + " tokens up, with no ceiling"
	case b.from == 0:
		return "up to " + strconv.Itoa(b.to) + " tokens"
	default:
		return strconv.Itoa(b.from+1) + " to " + strconv.Itoa(b.to) + " tokens"
	}
}

// sizeWarnings lists problems in a size step's configuration that are not
// worth refusing it for, such as sending everything to one destination.
func sizeWarnings(rt policy.Route, bands []sizeBand, models map[string]policy.Model) []string {
	var warnings []string
	live := 0
	for _, b := range bands {
		if b.dead {
			warnings = append(warnings, "'"+b.alias+"' is never the first choice for any "+
				"request. Its ceiling is no higher than the ceiling of the destination "+
				"before it, so the only traffic it can take is what reaches it when that "+
				"one is down.")
		} else {
			live++
		}
		// A ceiling above the model's context has no effect.
		m, ok := models[b.alias]
		if ceiling, bounded := rt.Ceiling(b.alias); ok && bounded &&
			m.MaxContext > 0 && ceiling > m.MaxContext {
			warnings = append(warnings, "the ceiling on '"+b.alias+"' is "+
				strconv.Itoa(ceiling)+" tokens and its context holds "+
				strconv.Itoa(m.MaxContext)+". A request between the two is ranked behind "+
				"every destination that can hold it, whatever this ceiling says.")
		}
	}
	if live <= 1 {
		warnings = append(warnings, "every request this step places goes to the same "+
			"destination whatever its size. That is an alias with a step in front of it: it "+
			"moves no traffic and produces nothing but successful requests. The ceilings are "+
			"the thing to change.")
	}
	return warnings
}
