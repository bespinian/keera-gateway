package gateway

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A header step routes by a request header whose values the administrator
// mapped to destinations, so the client's code chooses. It costs nothing and
// reads no text. Since the client chooses, it is not a boundary: it only picks
// between destinations the router allows anyway.

// byHeader routes by the step's header. The chosen destination is the only
// one tried.
func (s *Server) byHeader(rt policy.Route, h http.Header) (routeDecision, *refusal) {
	d := routeDecision{outcome: store.RouterChose}
	sent := h.Values(rt.Header)
	var alias string
	switch {
	case len(sent) == 0:
		d.why = "the request did not send the header " + rt.Header
	case len(sent) > 1:
		d.why = "the request sent the header " + rt.Header + " more than once"
	default:
		var known bool
		alias, known = rt.Values[strings.ToLower(strings.TrimSpace(sent[0]))]
		if !known {
			d.why = "the header " + rt.Header + " says " + quoteValue(sent[0]) +
				", which is not one of the step's values"
		}
	}
	if d.why != "" {
		if rt.Refuses() {
			d.outcome = store.RouterError
			return d, headerUndecided(rt, d.why)
		}
		return s.routeFallback(rt, d)
	}
	m, found := s.src.Model(rt.OrgID, alias)
	if why := chatProblem(m, found); why != "" {
		d.why = "the header " + rt.Header + " says " + quoteValue(sent[0]) + ", which " +
			"sends it to '" + alias + "', and that model " + why
		return s.routeFallback(rt, d)
	}
	d.alias, d.chain = alias, []string{alias}
	return d, nil
}

// headerUndecided refuses a request whose header names no destination, with a
// 400 that lists the values the client may send.
func headerUndecided(rt policy.Route, why string) *refusal {
	return &refusal{
		status: http.StatusBadRequest,
		typ:    "invalid_request_error", code: "router_undecided",
		msg: fmt.Sprintf("%s chooses the model by a request header, and %s. Send %s with "+
			"one of: %s. Nothing was sent to a model", stepSubject(rt), why, rt.Header,
			strings.Join(slices.Sorted(maps.Keys(rt.Values)), ", ")),
	}
}

// quoteValue quotes and shortens a client's value for an error and the log.
func quoteValue(v string) string {
	const limit = 64
	if len(v) > limit {
		v = strings.ToValidUTF8(v[:limit], "") + "…"
	}
	return strconv.Quote(v)
}

// checkHeader checks that each destination of a header step answers.
func (s *Server) checkHeader(ctx context.Context, rt policy.Route) RouteProbe {
	p := RouteProbe{Step: rt.Step, Mode: rt.Mode}
	p.Destinations, p.Dropped = s.sortDestinations(rt)
	if len(p.Destinations) == 0 {
		p.Error = "none of this step's destinations can be reached, so every request " +
			"through it is refused"
		return p
	}
	start := time.Now()
	for _, alias := range p.Destinations {
		m, _ := s.src.Model(rt.OrgID, alias)
		hop := chainHop(alias, headerReading(rt, alias), s.CheckModel(ctx, m))
		p.Chain = append(p.Chain, hop)
		switch {
		case hop.Answers:
		case alias == rt.Fallback:
			p.Warnings = append(p.Warnings, "the fallback '"+alias+"' is not answering, so "+
				"a request without a known value gets its error")
		default:
			p.Warnings = append(p.Warnings, "'"+alias+"' is not answering, so a request "+
				"that sends "+headerReading(rt, alias)+" gets its error")
		}
	}
	p.TotalMS = time.Since(start).Milliseconds()
	p.OK = true
	return p
}

func headerReading(rt policy.Route, alias string) string {
	values := policy.ValuesTo(rt.Values, alias)
	if alias == rt.Fallback {
		values = append(values, "anything else")
	}
	return strings.Join(values, ", ")
}
