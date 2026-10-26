package policy

import (
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"
)

// Router chains the steps a request passes on its way to a model: filters,
// route steps, rate limits, budgets and system prompts. A client names it
// where it names a model.
//
// The steps form a tree. A filter hands the request to one next step. A route
// step splits it into a branch per destination, and a limit into within and
// exceeded. Every branch ends at a model or a route step. Branches never join,
// so a step always knows which way the request came.
//
// The key's guardrails wrap the router: their limits are checked before it,
// and their filters and system prompt apply after it. So a router cannot
// loosen what an administrator set.
type Router struct {
	OrgID       string    `json:"org_id"`
	Alias       string    `json:"alias"`
	DisplayName string    `json:"display_name,omitempty"`
	Description string    `json:"description,omitempty"`
	Start       *Step     `json:"start"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// StepKind is what one step of a router does.
type StepKind string

const (
	StepFilter    StepKind = "filter"
	StepRoute     StepKind = "route"
	StepRateLimit StepKind = "rate_limit"
	StepBudget    StepKind = "budget"
	// StepModel sends the request to one model. It ends a branch.
	StepModel        StepKind = "model"
	StepSystemPrompt StepKind = "system_prompt"
)

var stepKinds = []StepKind{
	StepFilter, StepRoute, StepRateLimit, StepBudget, StepModel,
	StepSystemPrompt,
}

func (k StepKind) Limits() bool { return k == StepRateLimit || k == StepBudget }

// Names reports whether the step names a filter or model in Ref.
func (k StepKind) Names() bool { return k == StepFilter || k == StepModel }

// Step is one node of a router.
type Step struct {
	// ID names the step in its router. A limit's counters are kept under it,
	// so editing around a limit keeps what it counted.
	ID   string   `json:"id"`
	Kind StepKind `json:"kind"`
	Ref  string   `json:"ref,omitempty"`
	// Requests and Tokens are a rate limit's, per Period. Zero is no limit.
	Requests int64 `json:"requests,omitempty"`
	Tokens   int64 `json:"tokens,omitempty"`
	// BudgetMicros is a budget's, per Period.
	BudgetMicros int64 `json:"budget_micros,omitempty"`
	// Prompt is a system prompt step's text. It is added after every filter,
	// with the key's own, so no filter can edit it; the step's place only
	// decides which requests get it. On an instruction route step it is the
	// instruction.
	Prompt string `json:"prompt,omitempty"`
	// Mode, Model, Destinations, Ceilings and Fallback are a route step's.
	// See Route.
	Mode         RouteMode      `json:"mode,omitempty"`
	Model        string         `json:"model,omitempty"`
	Destinations []string       `json:"destinations,omitempty"`
	Ceilings     map[string]int `json:"ceilings,omitempty"`
	Fallback     string         `json:"fallback,omitempty"`
	// Header and Values are a header step's.
	Header string            `json:"header,omitempty"`
	Values map[string]string `json:"values,omitempty"`
	// Period is a limit's. Empty is a minute for a rate limit, a month for a
	// budget.
	Period Period `json:"period,omitempty"`
	// Next follows a filter, or a limit while within it.
	Next *Step `json:"next,omitempty"`
	// Exceeded is where a limit sends a request over it. Nil refuses it.
	Exceeded *Step `json:"exceeded,omitempty"`
	// Branches is a route step's, keyed by destination. A destination without
	// a branch gets the request straight away.
	Branches map[string]*Step `json:"branches,omitempty"`
}

// MaxPerMinute bounds a per-minute rate limit, which a token bucket counts in
// an int.
const MaxPerMinute = 1 << 30

// MaxStepPromptBytes bounds a system prompt step, which is sent with every
// request that passes it.
const MaxStepPromptBytes = 16 << 10

// MaxRoutePromptBytes bounds a route step's instruction, sent with every
// request that reaches the step.
const MaxRoutePromptBytes = 4 << 10

// MaxRouterSteps bounds one router. Every step may cost a generation or a
// counter.
const MaxRouterSteps = 64

// ScopeRouter is the scope of a router's limit step.
const ScopeRouter ScopeType = "router"

// Steps walks every step, each before what follows it, in a stable order.
func (p Router) Steps() iter.Seq[*Step] {
	return func(yield func(*Step) bool) {
		walkSteps(p.Start, yield)
	}
}

func walkSteps(s *Step, yield func(*Step) bool) bool {
	if s == nil {
		return true
	}
	if !yield(s) || !walkSteps(s.Next, yield) || !walkSteps(s.Exceeded, yield) {
		return false
	}
	for _, dest := range slices.Sorted(maps.Keys(s.Branches)) {
		if !walkSteps(s.Branches[dest], yield) {
			return false
		}
	}
	return true
}

// Uses reports whether a step names alias as kind. A route step uses its
// deciding model and its destinations.
func (p Router) Uses(kind StepKind, alias string) bool {
	for s := range p.Steps() {
		switch {
		case s.Kind == kind && s.Ref == alias:
			return true
		case kind == StepModel && s.Kind == StepRoute &&
			(s.Model == alias || slices.Contains(s.Destinations, alias)):
			return true
		}
	}
	return false
}

func (p Router) Route(s *Step) Route {
	return Route{
		OrgID: p.OrgID, Router: p.Alias, Step: s.ID,
		Mode: s.Mode, Model: s.Model, Prompt: s.Prompt,
		Destinations: s.Destinations, Ceilings: s.Ceilings, Fallback: s.Fallback,
		Header: s.Header, Values: s.Values,
	}
}

// Routes reads every route step, in the order Steps walks them.
func (p Router) Routes() []Route {
	var out []Route
	for s := range p.Steps() {
		if s.Kind == StepRoute {
			out = append(out, p.Route(s))
		}
	}
	return out
}

// Scopes is what a limit step counts under. The ids hold the organisation and
// router, so two routers never share one.
//
// A per-minute rate limit uses token buckets. One per day or month counts like
// a budget instead, since an in-memory count would reset on every restart.
func (p Router) Scopes(s *Step) []Scope {
	id := p.OrgID + "/" + p.Alias + "/" + s.ID
	if s.Kind == StepBudget {
		b := s.Budget()
		return []Scope{{Type: ScopeRouter, ID: id, BudgetMicros: b.BudgetMicros, Period: b.Per()}}
	}
	month := Scope{Type: ScopeRouter, ID: id, Period: PeriodMonth}
	if q := s.RateLimit().count(&month); q != nil {
		return q
	}
	return []Scope{month}
}

// quotas is a rate limit per day or month: a scope for requests and one for
// tokens, each left out when zero. The ids end in the meter, so they never
// share a window with the same scope's budget.
func quotas(t ScopeType, id string, p Period, requests, tokens int64) []Scope {
	var out []Scope
	for _, m := range []struct {
		meter Meter
		quota int64
	}{{MeterRequests, requests}, {MeterTokens, tokens}} {
		if m.quota > 0 {
			out = append(out, Scope{
				Type: t, ID: id + "|" + string(m.meter),
				Period: p, Meter: m.meter, Quota: m.quota,
			})
		}
	}
	return out
}

// RateLimit and Budget read a limit step as a guardrail limit, so both are
// checked and counted alike.
func (s *Step) RateLimit() RateLimit {
	return RateLimit{Requests: s.Requests, Tokens: s.Tokens, Period: s.Period}
}

func (s *Step) Budget() Budget { return Budget{BudgetMicros: s.BudgetMicros, Period: s.Period} }

// RouterRefs is what a router's steps may name, by alias.
type RouterRefs struct {
	Models  map[string]Model
	Filters map[string]Filter
}

// Reaches lists every model the router may send to. Allowing the router
// allows all of them.
func (p Router) Reaches() []string {
	var out []string
	add := func(alias string) {
		if !slices.Contains(out, alias) {
			out = append(out, alias)
		}
	}
	for s := range p.Steps() {
		switch s.Kind {
		case StepModel:
			add(s.Ref)
		case StepRoute:
			for _, d := range s.Destinations {
				if s.Branches[d] == nil {
					add(d)
				}
			}
		}
	}
	return out
}

// Problem says what is wrong with the router, or "".
func (p Router) Problem(refs RouterRefs) string {
	if !ValidAlias(p.Alias) {
		return "'" + p.Alias + "' is not a usable alias; use lowercase letters, digits and inner hyphens"
	}
	if p.Start == nil {
		return "'start' is required: a router needs at least one step, ending at a model"
	}
	seen := map[string]bool{}
	n := 0
	for s := range p.Steps() {
		n++
		if n > MaxRouterSteps {
			return fmt.Sprintf("a router may have at most %d steps", MaxRouterSteps)
		}
		if !validName(s.ID, 32) {
			return fmt.Sprintf("the step id '%s' is not usable; use up to 32 lowercase letters, "+
				"digits and inner hyphens", s.ID)
		}
		if seen[s.ID] {
			return "two steps have the id '" + s.ID + "'; each step needs its own"
		}
		seen[s.ID] = true
		if msg := s.problem(refs); msg != "" {
			return stepName(s) + " " + msg
		}
	}
	return ""
}

// problem says what is wrong with one step, in words that follow its name.
func (s *Step) problem(refs RouterRefs) string {
	if !slices.Contains(stepKinds, s.Kind) {
		return "has the kind '" + string(s.Kind) + "'; use filter, route, rate_limit, budget, " +
			"system_prompt or model"
	}
	switch {
	case !s.Kind.Names() && s.Ref != "":
		return "names '" + s.Ref + "', but a " + strings.ReplaceAll(string(s.Kind), "_", " ") +
			" step uses nothing of the organisation's"
	case s.Kind.Names() && s.Ref == "":
		return "names nothing; say which " + string(s.Kind) + " it uses in 'ref'"
	case s.Kind != StepSystemPrompt && s.Kind != StepRoute && s.Prompt != "":
		return "sets a 'prompt', which only a system prompt or route step has"
	case s.Kind != StepRoute && (s.Mode != "" || s.Model != "" || len(s.Destinations) > 0 ||
		len(s.Ceilings) > 0 || s.Fallback != "" || s.Header != "" || len(s.Values) > 0):
		return "sets 'mode', 'model', 'destinations', 'ceilings', 'header', 'values' or " +
			"'fallback', which only a route step has"
	case s.Kind != StepRateLimit && (s.Requests != 0 || s.Tokens != 0):
		return "sets 'requests' or 'tokens', which only a rate limit has"
	case s.Kind != StepBudget && s.BudgetMicros != 0:
		return "sets a budget, which only a budget step has"
	case !s.Kind.Limits() && s.Period != "":
		return "sets a 'period', which only a limit has"
	case !s.Kind.Limits() && s.Exceeded != nil:
		return "has an 'exceeded' branch, which only a limit has"
	case s.Kind != StepRoute && len(s.Branches) > 0:
		return "has 'branches', which only a route step has"
	case s.Kind == StepRoute && s.Next != nil:
		return "has a 'next' step; after a route step, the steps go in 'branches', one per " +
			"destination"
	case s.Kind == StepModel && s.Next != nil:
		return "has a 'next' step, but the request is sent to the model there, so nothing can follow"
	case s.Kind != StepRoute && s.Kind != StepModel && s.Next == nil:
		return "has no 'next' step; every branch has to end at a model or a route step"
	}

	switch s.Kind {
	case StepFilter:
		if _, ok := refs.Filters[s.Ref]; !ok {
			return "names '" + s.Ref + "', which is not one of this organisation's filters"
		}
	case StepRoute:
		return s.routeProblem(refs)
	case StepModel:
		if why := answererProblem(s.Ref, refs); why != "" {
			return "names '" + s.Ref + "', which " + why
		}
	case StepRateLimit:
		return s.RateLimit().Problem()
	case StepSystemPrompt:
		switch {
		case strings.TrimSpace(s.Prompt) == "":
			return "needs a 'prompt'"
		case len(s.Prompt) > MaxStepPromptBytes:
			return fmt.Sprintf("has a prompt of %d bytes; the limit is %d, since it is sent "+
				"with every request that passes it", len(s.Prompt), MaxStepPromptBytes)
		}
	case StepBudget:
		return s.Budget().Problem()
	}
	return ""
}

// Warnings are setups that work but are likely not meant. Today there is one:
// a model rewrites the request before a route step reads it, so the step
// judges the redacted text and may send out what it would have kept inside.
//
// Pattern filters are left out: what they remove is not what a route step
// judges by.
func (p Router) Warnings(refs RouterRefs) []string {
	var out []string
	var walk func(s *Step, rewrites []string)
	walk = func(s *Step, rewrites []string) {
		if s == nil {
			return
		}
		switch s.Kind {
		case StepFilter:
			if f, ok := refs.Filters[s.Ref]; ok && f.Mode.Rewrites() && f.UsesModel() && !f.Shadow {
				rewrites = append(slices.Clip(rewrites), s.Ref)
			}
		case StepRoute:
			if len(rewrites) > 0 && s.Mode.Decides() {
				msg := "the route step '" + s.ID + "' reads the request after the filter '" +
					strings.Join(rewrites, "', '") + "' rewrote it, so it decides on the " +
					"rewritten text, not what the client sent"
				if !slices.Contains(out, msg) {
					out = append(out, msg)
				}
			}
		}
		walk(s.Next, rewrites)
		walk(s.Exceeded, rewrites)
		for _, dest := range slices.Sorted(maps.Keys(s.Branches)) {
			walk(s.Branches[dest], rewrites)
		}
	}
	walk(p.Start, nil)
	return out
}

// stepName names a step in a message.
func stepName(s *Step) string {
	switch s.Kind {
	case StepRateLimit:
		return "the rate limit '" + s.ID + "'"
	case StepBudget:
		return "the budget '" + s.ID + "'"
	case StepRoute:
		return "the route step '" + s.ID + "'"
	case "":
		return "the step '" + s.ID + "'"
	}
	if s.Ref == "" {
		return "the " + string(s.Kind) + " step '" + s.ID + "'"
	}
	return "the " + string(s.Kind) + " step '" + s.Ref + "'"
}

func (s *Step) routeProblem(refs RouterRefs) string {
	if !s.Mode.Valid() {
		return "has the mode '" + string(s.Mode) + "'; use instruction, size, header, " +
			"fallback, latency or least-busy"
	}
	if msg := s.leftoverProblem(); msg != "" {
		return msg
	}
	n := len(s.Destinations)
	switch {
	case len(s.Ceilings) > 0 && !s.Mode.Sizes():
		return "sets 'ceilings', which only a step with the mode 'size' has"
	case (s.Header != "" || len(s.Values) > 0) && s.Mode != RouteModeHeader:
		return "sets 'header' or 'values', which only a step with the mode 'header' has"
	case s.Mode.Decides() && s.Model == "":
		return "needs a 'model': the model that decides, best a fast one served locally"
	case s.Mode.Decides() && strings.TrimSpace(s.Prompt) == "":
		return "needs a 'prompt': it tells the deciding model how to choose a destination"
	case len(s.Prompt) > MaxRoutePromptBytes:
		return fmt.Sprintf("has a prompt of %d bytes; the limit is %d, since it is sent with "+
			"every request", len(s.Prompt), MaxRoutePromptBytes)
	case n == 0:
		return "needs 'destinations': at least one model to send requests to"
	case n > MaxDestinations && s.Mode.Decides():
		// A small model picking from a long list tends to name the last item.
		return fmt.Sprintf("names %d destinations; the limit is %d, since a longer list is "+
			"too much for the deciding model to choose from", n, MaxDestinations)
	case n > MaxDestinations:
		return fmt.Sprintf("names %d destinations; the limit is %d, since each one tried "+
			"adds to the client's wait", n, MaxDestinations)
	case s.Fallback != "" && !slices.Contains(s.Destinations, s.Fallback):
		return "falls back to '" + s.Fallback + "', which is not one of its destinations. " +
			"The fallback has to be somewhere the step may send a request in the first place"
	}
	seen := map[string]bool{}
	for _, dest := range s.Destinations {
		if seen[dest] {
			return "names the destination '" + dest + "' twice"
		}
		seen[dest] = true
		if why := answererProblem(dest, refs); why != "" {
			return "sends to '" + dest + "', which " + why
		}
	}
	if s.Mode.Sizes() {
		if msg := s.ceilingProblem(); msg != "" {
			return msg
		}
	}
	if s.Mode == RouteModeHeader {
		if msg := s.headerProblem(); msg != "" {
			return msg
		}
	}
	for dest, next := range s.Branches {
		if !seen[dest] {
			return "has a branch for '" + dest + "', which is not one of its destinations"
		}
		if next == nil {
			return "has an empty branch for '" + dest + "'; leave it out to send straight to the model"
		}
	}
	return ""
}

// leftoverProblem refuses a model, prompt or fallback the step does not use. It
// would do nothing, but a reader would think it decides where traffic goes.
func (s *Step) leftoverProblem() string {
	mode := "a " + string(s.Mode) + " step"
	switch {
	case s.Mode.Decides():
		return ""
	case s.Model != "":
		return "sets a 'model', but " + mode + " has no deciding model"
	case s.Prompt != "":
		return "sets a 'prompt', but " + mode + " has no deciding model to give it to"
	case s.Fallback != "" && !s.Mode.Chooses():
		return "sets a 'fallback', but " + mode + " already tries its destinations in turn, " +
			"so put '" + s.Fallback + "' in 'destinations'"
	}
	return ""
}

// ceilingProblem refuses a size step that could not place every request.
// Exactly one destination must have no ceiling: with none, a large request has
// nowhere to go, and with more, only the first is ever chosen.
func (s *Step) ceilingProblem() string {
	for alias := range s.Ceilings {
		if !slices.Contains(s.Destinations, alias) {
			return "has a ceiling for '" + alias + "', which is not one of its destinations"
		}
	}
	switch unbounded := (Route{Destinations: s.Destinations, Ceilings: s.Ceilings}).Unbounded(); len(unbounded) {
	case 1:
		return ""
	case 0:
		return "needs one destination without a ceiling, usually the largest model, so the " +
			"largest requests have somewhere to go"
	default:
		return "may leave the ceiling off only one destination, but '" +
			strings.Join(unbounded, "' and '") + "' have none"
	}
}

// headerProblem refuses a header step that reads a credential, or whose
// values miss a destination. Values are compared in lowercase. A destination
// no value or fallback reaches would still be allowed with the router.
func (s *Step) headerProblem() string {
	switch {
	case s.Header == "":
		return "needs a 'header': the request header whose value chooses the destination"
	case !validHeaderName(s.Header):
		return "reads the header '" + s.Header + "', which is not a usable header name; " +
			"use up to 64 letters, digits and inner hyphens"
	case credentialHeaders[strings.ToLower(s.Header)]:
		return "reads the header '" + s.Header + "', which carries the client's " +
			"credential. Use a header of its own, such as X-Keera-Route"
	case len(s.Values) == 0:
		return "needs 'values': which value of the header sends a request where"
	case len(s.Values) > MaxHeaderValues:
		return fmt.Sprintf("has %d values; the limit is %d", len(s.Values), MaxHeaderValues)
	}
	reached := map[string]bool{s.Fallback: true}
	for _, value := range slices.Sorted(maps.Keys(s.Values)) {
		dest := s.Values[value]
		if !validName(value, 32) {
			return "has the value '" + value + "', which is not usable; use up to 32 " +
				"lowercase letters, digits and inner hyphens"
		}
		if !slices.Contains(s.Destinations, dest) {
			return "sends the value '" + value + "' to '" + dest + "', which is not one of " +
				"its destinations"
		}
		reached[dest] = true
	}
	for _, dest := range s.Destinations {
		if !reached[dest] {
			return "sends no value to '" + dest + "', and it is not the fallback, so no " +
				"request can reach it"
		}
	}
	return ""
}

// ValuesTo returns the values that send a request to dest, sorted.
func ValuesTo(values map[string]string, dest string) []string {
	var out []string
	for _, v := range slices.Sorted(maps.Keys(values)) {
		if values[v] == dest {
			out = append(out, v)
		}
	}
	return out
}

// credentialHeaders are the sign-in headers, in lowercase. A header step must
// not route on a key.
var credentialHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "x-keera-key": true,
}

// validHeaderName allows the part of the HTTP token grammar people use.
func validHeaderName(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// answererProblem says why a model cannot answer through a router, in words
// that follow "which", or "".
func answererProblem(alias string, refs RouterRefs) string {
	m, ok := refs.Models[alias]
	switch {
	case !ok:
		return "is not one of this organisation's models"
	case m.Kind != KindChat:
		return "is a " + string(m.Kind) + " model; a router is reached on a chat request, " +
			"so it can only send to a chat model"
	case m.Subscription:
		return "is paid by each person's own Claude subscription; a router cannot send a " +
			"request there"
	}
	return ""
}
