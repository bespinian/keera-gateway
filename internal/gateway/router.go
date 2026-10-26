package gateway

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// A router is a tree of steps: filters, route steps, rate limits, budgets and
// system prompts, ending at a model. See policy.Router. It runs after the
// key's limits and before the key's filters, so it works inside the
// guardrails, never around them. Each step reuses the code that does the same
// job outside a router.

// useRouter walks the router the client named until a model is chosen. It
// returns the body, which a step may have translated or rewritten.
func (s *Server) useRouter(c *call, b *body) (*body, bool) {
	p := c.router
	c.ev.Router = p.Alias
	// Replaced with where it sent the request, once a route step chose.
	c.w.Header().Set("X-Keera-Router", p.Alias)
	for step := p.Start; step != nil; {
		ok := true
		switch step.Kind {
		case policy.StepFilter:
			// Filters in a row run as one chain, so the body is read and
			// written once.
			var aliases []string
			for ; step != nil && step.Kind == policy.StepFilter; step = step.Next {
				aliases = append(aliases, step.Ref)
			}
			b, ok = s.routerFilters(c, b, aliases)
		case policy.StepRateLimit, policy.StepBudget:
			step, ok = s.routerLimit(c, step)
		case policy.StepRoute:
			b, step, ok = s.routerRoute(c, b, step)
		case policy.StepSystemPrompt:
			// prepare adds it after the key's own prompt and after every
			// filter.
			c.prompts = append(c.prompts, step.Prompt)
			step = step.Next
		case policy.StepModel:
			return b, s.routerModel(c, step)
		default:
			// The control plane refuses such a step, so a newer version
			// wrote this router.
			s.refuse(c, routerBroken(p, fmt.Sprintf("it has a step of the kind '%s', "+
				"which this gateway does not know", step.Kind)))
			return nil, false
		}
		if !ok {
			return nil, false
		}
	}
	// The last step was a route step with no branch for its choice.
	return b, true
}

func (s *Server) routerFilters(c *call, b *body, aliases []string) (*body, bool) {
	rewrote, ok := s.runFilters(c, b, aliases, "the router '"+c.router.Alias+"'")
	if !ok {
		return nil, false
	}
	// An earlier route step translated the body. Translate again from what
	// the filter left, or the model would get the original.
	if rewrote && c.native != nil && b != nil && !s.retranslate(c, b) {
		return nil, false
	}
	return b, true
}

// routerLimit checks one rate limit or budget step and returns the next step:
// onward, its exceeded branch, or a refusal without one. A request within the
// limit is charged to it in full; one on the exceeded branch not at all.
func (s *Server) routerLimit(c *call, step *policy.Step) (*policy.Step, bool) {
	scopes := c.router.Scopes(step)
	var (
		ref    refusal
		within bool
	)
	switch {
	case step.Kind == policy.StepBudget:
		ref, within = s.checkBudgets(c, scopes)
	case step.RateLimit().PerMinute():
		ref, within = s.checkRates(scopes, c.now)
	default:
		ref, within = s.checkQuotas(scopes, c.now)
	}
	if within {
		c.scopes = append(c.scopes, scopes...)
		// The usage row charges the step the whole cost, so this does too,
		// starting with what earlier steps spent.
		s.budgets.Charge(scopes, c.hookMicros(), c.now)
		// Tokens are counted at the end.
		s.budgets.StartRequest(scopes, c.now)
		return step.Next, true
	}
	if step.Exceeded == nil {
		ref.msg = "the router '" + c.router.Alias + "': " + ref.msg
		s.refuse(c, ref)
		return nil, false
	}
	// A note, so a request answered by the cheaper model says why.
	c.ev.Note = appendNote(c.ev.Note, "the router '"+c.router.Alias+"' took the "+
		"exceeded branch of '"+step.ID+"': "+ref.msg)
	return step.Exceeded, true
}

// routerRoute lets one route step choose the model, and returns the branch
// for that choice, or nil to go straight to the chosen model.
func (s *Server) routerRoute(c *call, b *body, step *policy.Step) (*body, *policy.Step, bool) {
	rt := c.router.Route(step)
	// Only the last decision is recorded on its own. Earlier ones are still
	// charged and kept on the request.
	c.earlier.micros += c.decision.micros
	c.earlier.bills = append(c.earlier.bills, c.decision.bills...)
	c.earlier.took += c.decision.took
	c.route, c.decision = rt, routeDecision{}
	if rt.Decides() || rt.Sizes() {
		var ok bool
		if b, ok = s.translate(c, b); !ok {
			return nil, nil, false
		}
	}
	if !s.useRoute(c, b) {
		return nil, nil, false
	}
	if next := step.Branches[c.alias]; next != nil {
		return b, next, true
	}
	// A destination with a branch is reached only through it. Failing over
	// to it would skip the branch's steps, such as a redaction filter.
	c.decision.chain = slices.DeleteFunc(c.decision.chain, func(alias string) bool {
		return step.Branches[alias] != nil
	})
	return b, nil, true
}

// routerModel ends the router at one model.
func (s *Server) routerModel(c *call, step *policy.Step) bool {
	m, found := s.src.Model(c.res.Key.OrgID, step.Ref)
	if why := chatProblem(m, found); why != "" {
		s.refuse(c, routerBroken(c.router, "it ends at the model '"+step.Ref+"', which "+why))
		return false
	}
	c.model, c.alias = m, m.Alias
	c.ev.Alias = m.Alias
	// An earlier route step may have ranked several models; this one wins.
	c.decision.chain = nil
	return true
}

// routerBroken refuses for a router that names something it can no longer
// use. An administrator can fix that, so it is 503.
func routerBroken(p policy.Router, why string) refusal {
	return refusal{
		status: http.StatusServiceUnavailable,
		typ:    "server_error", code: "router_unavailable",
		msg:    "the router '" + p.Alias + "' cannot place this request: " + why + ". Nothing was sent to a model",
		advise: true,
	}
}
