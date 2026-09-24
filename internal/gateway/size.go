package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A size router places a request by how long it is. How much model a request
// needs usually follows its length: a variable rename is a paragraph, a
// migration plan is a conversation plus files.
//
// It costs nothing, answers the same way every time (so its check shows what
// production does), and a prompt cannot talk it into the large model.
//
// It cannot tell a hard request from an easy one of the same length, and it
// does not read the text. Keeping client data inside the cluster is a job for
// an instruction router or a filter.

// sizeTier is how well one destination suits a request, best first.
type sizeTier int

const (
	// sizeFits is a destination meant for this size that can hold it.
	sizeFits sizeTier = iota
	// sizeOverCeiling can hold the request but was meant for smaller ones. It
	// is used when everything meant for this size is down.
	sizeOverCeiling
	// sizeTooLarge cannot hold the request in its context, so it is ranked
	// last whatever the ceilings say.
	sizeTooLarge
)

// sized places a request by how much text is in it.
//
// The chain is every serveable destination in preference order, not just the
// winner: if the preferred one is down, the larger model behind it is better
// than a refusal. Nothing is generated or spent.
func (s *Server) sized(rt policy.Router, b *body, kind policy.Kind) (routeDecision, *refusal) {
	d := routeDecision{outcome: store.RouterChose}

	// The same extraction the filters and instruction routers use.
	doc, err := extractText(b, kind)
	if err != nil {
		return d, &refusal{
			status: http.StatusBadRequest,
			typ:    "invalid_request_error", code: "invalid_body",
			msg: "'" + rt.Alias + "' is a router: it chooses the model by how much text is " +
				"in the request, and this request could not be read: " + err.Error(),
			advise: true,
		}
	}
	tokens := estimateTokens(doc.texts())

	type ranked struct {
		alias   string
		tier    sizeTier
		ceiling int
		at      int
	}
	var order []ranked
	for i, alias := range rt.Destinations {
		m, ok := s.serveable(alias)
		if !ok {
			continue
		}
		ceiling, bounded := rt.Ceiling(alias)
		r := ranked{alias: alias, ceiling: ceiling, at: i, tier: sizeFits}
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
		return d, &refusal{
			status: http.StatusServiceUnavailable,
			typ:    "server_error", code: "router_destination_unavailable",
			msg: fmt.Sprintf("'%s' is a router: it chooses between its destinations by how "+
				"much text is in the request, and none of them can answer one - every one is "+
				"missing, disabled, of another kind, or has no backend. Nothing was sent to "+
				"a model", rt.Alias),
			advise: true,
		}
	}

	// Tier first, then the smallest ceiling (the cheaper model), then the
	// written order. No ceiling sorts after every bounded one: it takes what
	// nothing else would.
	sort.SliceStable(order, func(i, j int) bool {
		a, c := order[i], order[j]
		if a.tier != c.tier {
			return a.tier < c.tier
		}
		if (a.ceiling == 0) != (c.ceiling == 0) {
			return c.ceiling == 0
		}
		if a.ceiling != c.ceiling {
			return a.ceiling < c.ceiling
		}
		return a.at < c.at
	})

	for _, r := range order {
		d.chain = append(d.chain, r.alias)
	}
	d.alias = d.chain[0]
	return d, nil
}

// estimateTokens estimates a request's size without a tokeniser, the same
// way the filters do. It over-estimates, which sends a borderline request to
// the larger model.
//
// It counts only text, so images are not counted. That understates the
// context they use, which is why the MaxContext tier is a guard, not a
// guarantee.
func estimateTokens(texts []string) int {
	n := 0
	for _, t := range texts {
		n += len(t)
	}
	return n / bytesPerToken
}

/* ------------------------------------------------------------ checking one */

// sizeBand is one destination's share of the size range, for the check. The
// router depends on one number, so the bands describe it completely.
type sizeBand struct {
	alias string
	// from and to are the request sizes, in estimated tokens, this destination
	// is first choice for. A to of zero is unbounded.
	from, to int
	// dead says this destination is never first choice, because its ceiling
	// is no larger than the one before it.
	dead bool
}

// sizeBands works out which sizes each destination is first choice for.
//
// The order is the one sized uses when every destination is up and can hold the
// request: smallest ceiling first, unbounded last, the written order breaking a
// tie. Each then takes everything above the previous one's ceiling.
func sizeBands(rt policy.Router, aliases []string) []sizeBand {
	type entry struct {
		alias   string
		ceiling int
		at      int
	}
	order := make([]entry, 0, len(aliases))
	for i, alias := range aliases {
		ceiling, _ := rt.Ceiling(alias)
		order = append(order, entry{alias: alias, ceiling: ceiling, at: i})
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if (a.ceiling == 0) != (b.ceiling == 0) {
			return b.ceiling == 0
		}
		if a.ceiling != b.ceiling {
			return a.ceiling < b.ceiling
		}
		return a.at < b.at
	})

	out := make([]sizeBand, 0, len(order))
	floor := 0
	for _, e := range order {
		band := sizeBand{alias: e.alias, from: floor, to: e.ceiling}
		// A ceiling at or below the floor is never first choice, but it still
		// takes traffic when the ones before are down, so it is kept.
		if e.ceiling > 0 && e.ceiling <= floor {
			band.dead = true
		} else if e.ceiling > 0 {
			floor = e.ceiling
		}
		out = append(out, band)
	}
	return out
}

// describeBand says what sizes a destination is first choice for, in the words
// somebody reads on a check.
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

// sizeWarnings says what is wrong with a size router's configuration but not
// wrong enough to refuse when it is written. The most important: a router that
// sends everything to one destination is just an alias.
func sizeWarnings(rt policy.Router, bands []sizeBand, models map[string]policy.Model) []string {
	var warnings []string
	live := 0
	for _, b := range bands {
		if !b.dead {
			live++
		}
		if b.dead {
			warnings = append(warnings, "'"+b.alias+"' is never the first choice for any "+
				"request. Its ceiling is no higher than the ceiling of the destination "+
				"before it, so the only traffic it can take is what reaches it when that "+
				"one is down.")
		}
		// A ceiling above the model's context. Nothing breaks, but the number
		// set is not the one in effect.
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
		warnings = append(warnings, "every request this router places goes to the same "+
			"destination whatever its size. That is an alias with a step in front of it: it "+
			"moves no traffic and produces nothing but successful requests. The ceilings are "+
			"the thing to change.")
	}
	return warnings
}
