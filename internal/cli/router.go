package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// routerRun reads a router's steps from a JSON file, since flags cannot spell
// a tree. A route step also has flags, so the simplest router needs no file.
// See docs/routers.md.
type routerRun struct {
	*aliasRun

	since       time.Duration
	file        string
	displayName string
	description string

	// The route step's flags. step names the step they change or check.
	step         string
	mode         string
	model        string
	destinations string
	fallback     string
	noFallback   bool
	prompt       string
	header       string
	values       string
}

func routerCmd(ctx context.Context, args []string) error {
	r := &routerRun{aliasRun: newAliasRun("router", "routers", "router", args)}
	fs := r.fs
	durationVar(fs, &r.since, "since", reportWindow, sinceUsage)
	fs.StringVar(&r.file, "file", "",
		"a JSON file with the steps: the first step, or a whole router with 'start'; - reads stdin")
	fs.StringVar(&r.displayName, "display-name", "",
		"the router's name for people; clients still name the alias")
	fs.StringVar(&r.description, "description", "", "what this router is for, for whoever reads the list")
	fs.StringVar(&r.step, "step", "",
		"the id of the route step to change; needed only when there is more than one")
	fs.StringVar(&r.mode, "mode", "",
		"what the route step chooses by: 'instruction' reads the request with a model; 'size' "+
			"places it by how much text is in it; 'header' by a header the client sends; "+
			"'fallback', 'latency' and 'least-busy' try the destinations until one answers, "+
			"in the order written, fastest first, or emptiest first (default: instruction)")
	fs.StringVar(&r.model, "model", "", "the model that makes the decision - a fast local one, or a decision model")
	fs.StringVar(&r.destinations, "destinations", "",
		"comma-separated models the route step may choose between; on a fallback step, in the "+
			"order to try them, on a latency or least-busy step, the order to break a tie in, "+
			"and on a size step, each with its ceiling - 'keera-speed:4k,big'")
	// Two flags: on 'set', one string could not tell "keep the fallback" from
	// "no fallback".
	fs.StringVar(&r.fallback, "fallback", "",
		"where a request goes when the decision cannot be made, or the header names no "+
			"destination; must be one of the destinations")
	fs.BoolVar(&r.noFallback, "no-fallback", false,
		"refuse a request the route step cannot place, rather than sending it to a fallback")
	fs.StringVar(&r.prompt, "prompt", "", "the instruction the deciding model is given; @path reads a file")
	fs.StringVar(&r.header, "header", "", "the request header a header step chooses by, such as X-Keera-Route")
	fs.StringVar(&r.values, "values", "",
		"which value of the header sends a request where - 'cheap=keera-speed,hard=keera-frontier'; "+
			"without --destinations, these models and the fallback are the destinations")
	r.reword("set", "file", "a JSON file with the new steps; without it the steps are kept")
	r.reword("set", "display-name", "the router's name for people; an empty one clears it")
	r.reword("set", "description", "what this router is for; an empty one clears it")
	r.reword("check", "step", "check only the route step with this id")

	if done, err := r.parse(); done {
		return err
	}
	if r.fallback != "" && r.noFallback {
		return opposites("fallback", "no-fallback")
	}
	if !policy.RouteMode(r.mode).Valid() {
		return fmt.Errorf("--mode is %q; use instruction, size, header, fallback, latency or "+
			"least-busy (see: keera help router add)", r.mode)
	}
	if r.verb == "show" {
		if err := r.resolveOrg(ctx); err != nil {
			return err
		}
		return r.show(ctx)
	}
	return r.run(ctx, r)
}

func routerAlias(p policy.Router) string { return p.Alias }

func (r *routerRun) find(ctx context.Context, alias string) (policy.Router, error) {
	return findAlias(ctx, r.aliasRun, alias, routerAlias)
}

func (r *routerRun) list(ctx context.Context) error {
	return listAliases(ctx, r.aliasRun, printRouters)
}

func (r *routerRun) check(ctx context.Context) error {
	alias := r.fs.Arg(0)
	path := r.path(alias, "/check")
	if r.step != "" {
		path += "&step=" + url.QueryEscape(r.step)
	}
	return checkProbe(ctx, r.c, path, "the router "+alias, r.asJSON, printRouterCheck,
		func(c control.RouterCheck) bool {
			return !slices.ContainsFunc(c.Routes, func(p gateway.RouteProbe) bool { return !p.OK })
		})
}

func (r *routerRun) report(ctx context.Context) error {
	return reportAlias(ctx, r.aliasRun, r.since, printRouterReport)
}

func (r *routerRun) show(ctx context.Context) error {
	p, err := r.find(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return out(r.asJSON, p, func(w *table) { printRouter(w, p) })
}

func (r *routerRun) delete(ctx context.Context) error {
	p, err := r.find(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return r.deleteEntry(ctx, p.Alias, []string{
		"  it leaves the organisation's allow-list",
		"  any client still naming it is told the model does not exist",
		"  its limits' counts are gone; a new router of this alias starts from zero",
	})
}

func (r *routerRun) routeFlags() bool {
	return slices.ContainsFunc(routeStepFlags, func(name string) bool { return given(r.fs, name) })
}

func (r *routerRun) put(ctx context.Context) error {
	alias := r.fs.Arg(0)
	if r.verb == "add" && r.file == "" && !r.routeFlags() {
		return fmt.Errorf("--file or a route step's flags, such as --destinations, are " +
			"required: a router is a tree of steps, written as JSON, or one route step " +
			"(see: keera help router add)")
	}
	p, err := startPut(ctx, r.aliasRun, alias, policy.Router{Alias: alias}, routerAlias)
	if err != nil {
		return err
	}
	if r.file != "" {
		if err := readRouterFile(r.file, &p); err != nil {
			return err
		}
	}
	if r.routeFlags() {
		step, err := r.routeStep(&p)
		if err != nil {
			return err
		}
		if err := r.applyRoute(step); err != nil {
			return err
		}
	}
	if given(r.fs, "display-name") {
		p.DisplayName = r.displayName
	}
	if given(r.fs, "description") {
		p.Description = r.description
	}
	nameSteps(p)

	var saved control.SavedRouter
	if err := r.c.do(ctx, "PUT", r.path(alias, ""), map[string]any{
		"display_name": p.DisplayName, "description": p.Description, "start": p.Start,
	}, &saved); err != nil {
		return err
	}
	if err := out(r.asJSON, saved, func(w *table) { printRouter(w, saved.Router) }); err != nil {
		return err
	}
	if r.asJSON {
		return nil
	}
	for _, warning := range saved.Warnings {
		fmt.Fprintf(os.Stderr, "\n%s\n", styleErr.warn(wrapAt("warning: "+warning+".", 0, 0, 76)))
	}
	if saved.Warning != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", styleErr.warn(wrapAt(saved.Warning+
			". 'keera guardrail set org <org> --models' sets the whole list, so name "+
			"every model it should allow.", 0, 0, 76)))
	}
	return nil
}

// routeStep finds the step --step names, or the only one. A router with no
// steps yet gets one.
func (r *routerRun) routeStep(p *policy.Router) (*policy.Step, error) {
	if p.Start == nil {
		id := r.step
		if id == "" {
			id = "route"
		}
		p.Start = &policy.Step{ID: id, Kind: policy.StepRoute}
		return p.Start, nil
	}
	var routes []*policy.Step
	for s := range p.Steps() {
		if s.Kind != policy.StepRoute {
			continue
		}
		if r.step != "" && s.ID == r.step {
			return s, nil
		}
		routes = append(routes, s)
	}
	ids := make([]string, 0, len(routes))
	for _, s := range routes {
		ids = append(ids, s.ID)
	}
	switch {
	case r.step != "":
		return nil, fmt.Errorf("the router %s has no route step '%s'; its route steps are: %s",
			p.Alias, r.step, dash(strings.Join(ids, ", ")))
	case len(routes) == 0:
		return nil, fmt.Errorf("the router %s has no route step to change; add one with --file",
			p.Alias)
	case len(routes) > 1:
		return nil, fmt.Errorf("the router %s has more than one route step (%s); say which "+
			"with --step", p.Alias, strings.Join(ids, ", "))
	}
	return routes[0], nil
}

func (r *routerRun) applyRoute(s *policy.Step) error {
	if r.mode != "" {
		s.Mode = policy.RouteMode(r.mode)
	}
	if s.Mode == "" {
		s.Mode = policy.RouteModeInstruction
	}
	if r.model != "" {
		if !s.Mode.Decides() {
			return needsDecider("model", s.Mode)
		}
		s.Model = r.model
	}
	if r.destinations != "" {
		aliases, ceilings, err := parseDestinations(r.destinations)
		if err != nil {
			return err
		}
		if len(ceilings) > 0 && !s.Mode.Sizes() {
			return fmt.Errorf("a ceiling was set on a destination, and this is a %s "+
				"step: nothing there reads the size of a request. Use --mode size "+
				"for a step that does", s.Mode)
		}
		s.Destinations, s.Ceilings = aliases, ceilings
	}
	// A step that changes mode keeps nothing the new mode does not use, or
	// the control plane refuses it.
	if !s.Mode.Decides() {
		s.Model, s.Prompt = "", ""
	}
	if !s.Mode.Chooses() {
		s.Fallback = ""
	}
	switch {
	case r.noFallback:
		s.Fallback = ""
	case r.fallback != "":
		if !s.Mode.Chooses() {
			return needsDecider("fallback", s.Mode)
		}
		s.Fallback = r.fallback
	}
	if err := r.applyHeader(s); err != nil {
		return err
	}
	if !s.Mode.Sizes() {
		s.Ceilings = nil
	}
	if r.prompt != "" {
		if !s.Mode.Decides() {
			return needsDecider("prompt", s.Mode)
		}
		text, err := textOrFile(r.prompt)
		if err != nil {
			return err
		}
		s.Prompt = text
	}
	return nil
}

// applyHeader sets what a header step reads. Without --destinations, its
// destinations are its values' targets and the fallback.
func (r *routerRun) applyHeader(s *policy.Step) error {
	if s.Mode != policy.RouteModeHeader {
		for _, flag := range []string{"header", "values"} {
			if given(r.fs, flag) {
				return fmt.Errorf("--%s is for a route step that chooses by a header; "+
					"use --mode header", flag)
			}
		}
		s.Header, s.Values = "", nil
		return nil
	}
	if r.header != "" {
		s.Header = r.header
	}
	var order []string
	if r.values != "" {
		values, dests, err := parseValues(r.values)
		if err != nil {
			return err
		}
		s.Values, order = values, dests
	} else {
		for _, v := range slices.Sorted(maps.Keys(s.Values)) {
			order = append(order, s.Values[v])
		}
	}
	if r.destinations != "" || (r.values == "" && r.fallback == "" && !r.noFallback) {
		return nil
	}
	s.Destinations = nil
	for _, d := range append(order, s.Fallback) {
		if d != "" && !slices.Contains(s.Destinations, d) {
			s.Destinations = append(s.Destinations, d)
		}
	}
	return nil
}

func needsDecider(flag string, mode policy.RouteMode) error {
	return fmt.Errorf("--%s is for a route step that decides with a model; a %s step "+
		"decides nothing, and what orders its destinations is %s", flag, mode, cliOrdersBy(mode))
}

// readRouterFile reads stdin for "-". The file holds either the first step or
// a whole router with 'start', as 'show --json' prints it.
func readRouterFile(path string, p *policy.Router) error {
	text, err := textOrFile("@" + path)
	if err != nil {
		return err
	}
	raw := []byte(text)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	if _, whole := probe["start"]; whole {
		var in policy.Router
		if err := decodeStrict(raw, &in); err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		p.Start, p.DisplayName, p.Description = in.Start, in.DisplayName, in.Description
		return nil
	}
	p.Start = &policy.Step{}
	if err := decodeStrict(raw, p.Start); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}

// decodeStrict refuses unknown fields, as the control plane does. Otherwise a
// misspelt "exceded" would save a limit that refuses instead of branching.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// nameSteps names steps without an id after their kind, such as "filter-2". A
// limit counts under its id, so one that should keep its counts across edits
// needs an id in the file.
func nameSteps(p policy.Router) {
	taken := map[string]bool{}
	for s := range p.Steps() {
		taken[s.ID] = true
	}
	n := map[policy.StepKind]int{}
	for s := range p.Steps() {
		if s.ID != "" {
			continue
		}
		base := strings.ReplaceAll(string(s.Kind), "_", "-")
		if base == "" {
			base = "step"
		}
		for {
			n[s.Kind]++
			id := base + "-" + strconv.Itoa(n[s.Kind])
			if !taken[id] {
				s.ID, taken[id] = id, true
				break
			}
		}
	}
}

func printRouters(w *table, routers []policy.Router) {
	w.header("ALIAS\tNAME\tSTEPS\tSENDS TO\tDESCRIPTION")
	for _, p := range routers {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			p.Alias, dash(p.DisplayName), len(slices.Collect(p.Steps())), dash(strings.Join(p.Reaches(), ", ")),
			dash(p.Description))
	}
}

func printRouter(w *table, p policy.Router) {
	show(w, "alias", p.Alias)
	showIfSet(w, "display name", p.DisplayName)
	showIfSet(w, "description", p.Description)
	_, _ = fmt.Fprintln(w, "steps\t")
	for _, line := range stepTree(p.Start) {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
	// A route step's instruction is printed whole, which a tree line cannot do.
	for s := range p.Steps() {
		if s.Kind == policy.StepRoute && s.Mode.Decides() && s.Prompt != "" {
			printBlock(w, "instruction ("+s.ID+")", strings.Split(s.Prompt, "\n"))
		}
	}
}

// stepTree draws one line per step. A single next step stays at the same
// depth; where a request can go two ways, each way is a branch under it.
func stepTree(s *policy.Step) []string {
	var lines []string
	for s != nil {
		lines = append(lines, stepLine(s))
		switch {
		case s.Kind == policy.StepRoute:
			return append(lines, branchLines(routeBranches(s))...)
		case s.Kind.Limits() && s.Exceeded != nil:
			return append(lines, branchLines([]branch{
				{"within", stepTree(s.Next)},
				{"exceeded", stepTree(s.Exceeded)},
			})...)
		}
		s = s.Next
	}
	return lines
}

type branch struct {
	label string
	lines []string
}

// routeBranches is one branch per destination, in the step's order. A branch
// for a destination it no longer has is still shown, so it can be removed.
func routeBranches(s *policy.Step) []branch {
	dests := slices.Clone(s.Destinations)
	for _, d := range slices.Sorted(maps.Keys(s.Branches)) {
		if !slices.Contains(dests, d) {
			dests = append(dests, d)
		}
	}
	out := make([]branch, 0, len(dests))
	for _, d := range dests {
		label := "to " + d
		if n, ok := s.Ceilings[d]; ok && n > 0 {
			label += " (up to " + formatTokenCount(n) + " tokens)"
		}
		if values := policy.ValuesTo(s.Values, d); len(values) > 0 {
			label += " (for " + strings.Join(values, ", ") + ")"
		}
		if next := s.Branches[d]; next != nil {
			out = append(out, branch{label, stepTree(next)})
			continue
		}
		out = append(out, branch{label + ", sent straight there", nil})
	}
	return out
}

func branchLines(branches []branch) []string {
	var lines []string
	for i, b := range branches {
		head, indent := "├─ ", "│  "
		if i == len(branches)-1 {
			head, indent = "└─ ", "   "
		}
		lines = append(lines, head+style.muted(b.label))
		for _, line := range b.lines {
			lines = append(lines, indent+line)
		}
	}
	return lines
}

// stepLine shows the id of a limit, whose counts are kept under it, and of a
// route step, which --step names by it.
func stepLine(s *policy.Step) string {
	switch s.Kind {
	case policy.StepFilter:
		return "filter " + style.cmd(s.Ref)
	case policy.StepRoute:
		return "route " + routeLine(s) + " " + style.muted("("+s.ID+")")
	case policy.StepModel:
		return "model " + style.cmd(s.Ref)
	case policy.StepRateLimit:
		return "rate limit " + rateText(s.RateLimit()) + limitTail(s)
	case policy.StepSystemPrompt:
		return "system prompt " + style.muted(strconv.Quote(promptPreview(s.Prompt)))
	case policy.StepBudget:
		return "budget " + budgetText(s.Budget()) + limitTail(s)
	}
	return string(s.Kind) + " " + s.Ref
}

func routeLine(s *policy.Step) string {
	switch {
	case s.Mode.Decides():
		return "decided by " + style.cmd(s.Model) + ", " + fallbackLabel(s.Fallback) +
			" when it cannot"
	case s.Mode.Sizes():
		return "by size"
	case s.Mode == policy.RouteModeHeader:
		return "by the header " + style.cmd(s.Header) + ", " + fallbackLabel(s.Fallback) +
			" without a known value"
	case s.Mode.Measures():
		return "by " + cliOrdersBy(s.Mode)
	default:
		return "tries in order"
	}
}

// promptPreview is enough to tell two prompts apart in a tree.
func promptPreview(p string) string {
	p = strings.Join(strings.Fields(p), " ")
	if r := []rune(p); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return p
}

func limitTail(s *policy.Step) string {
	tail := " " + style.muted("("+s.ID+")")
	if s.Exceeded == nil {
		tail += ", refuses what is over it"
	}
	return tail
}

func cliOrdersBy(mode policy.RouteMode) string {
	switch mode {
	case policy.RouteModeLatency:
		return "what each has lately taken to begin answering, fastest first"
	case policy.RouteModeLeastBusy:
		return "how many requests the gateway has in flight against each, emptiest first"
	case policy.RouteModeSize:
		return "how much text is in the request, against the ceiling on each"
	case policy.RouteModeHeader:
		return "the value of a header the client sends"
	default:
		return "the order they are given to --destinations"
	}
}

func fallbackLabel(fallback string) string {
	if fallback == "" {
		return "refuses the request"
	}
	return "falls back to " + fallback
}

func printRouterCheck(w *table, c control.RouterCheck) {
	show(w, "router", c.Router)
	for _, p := range c.Routes {
		_, _ = fmt.Fprintln(w)
		printRouteProbe(w, p)
	}
}

func printRouteProbe(w *table, p gateway.RouteProbe) {
	show(w, "route step", p.Step+" ("+string(p.Mode)+")")
	showIfSet(w, "decides with", p.Model)
	if p.Mode.Sizes() || p.Mode == policy.RouteModeHeader {
		show(w, "places between", strings.Join(p.Destinations, ", "))
	} else {
		show(w, "offered", strings.Join(p.Destinations, ", "))
	}
	for _, d := range p.Dropped {
		show(w, "not offered", d)
	}
	show(w, "result", yesNo(p.OK))
	if p.TotalMS > 0 {
		show(w, "took", fmt.Sprintf("%dms", p.TotalMS))
	}
	if p.CostMicros > 0 {
		show(w, "cost of these runs", policy.FormatMoney(p.CostMicros))
	}
	showIfSet(w, "error", p.Error)
	for _, warning := range p.Warnings {
		show(w, "warning", warning)
	}
	// Each sample beside where it went, since what is right depends on it.
	for i, d := range p.Decisions {
		printDecision(w, i, d)
	}
	printRouteChain(w, p)
}

func printDecision(w *table, i int, d gateway.SampleDecision) {
	_, _ = fmt.Fprintf(w, "\nsample %d\t%s\n", i+1, d.Asks)
	_, _ = fmt.Fprintf(w, "  sent\t%s\n", firstLine(d.Prompt))
	switch {
	case d.Error != "":
		_, _ = fmt.Fprintf(w, "  could not decide\t%s\n", d.Error)
	case d.Confidence > 0:
		// A step right but barely sure is what this check is for.
		_, _ = fmt.Fprintf(w, "  chose\t%s (%.0f%% sure)\n", d.Chosen, d.Confidence*100)
	default:
		_, _ = fmt.Fprintf(w, "  chose\t%s\n", d.Chosen)
	}
}

// printRouteChain lists destinations in the order they would be tried. The
// first that answers serves every request right now.
func printRouteChain(w *table, p gateway.RouteProbe) {
	if len(p.Chain) == 0 {
		return
	}
	// A size step's bands are configuration, not a reading, so they get
	// their own column.
	if p.Mode.Sizes() {
		_, _ = fmt.Fprintln(w)
		w.header("PLACED ON\tTAKES\tANSWERS\tTOOK\tNOTE")
		for _, hop := range p.Chain {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				hop.Alias, hop.Reading, yesNo(hop.Answers), hopTook(hop), hop.Note)
		}
		return
	}
	if p.Mode == policy.RouteModeHeader {
		_, _ = fmt.Fprintln(w)
		w.header("PLACED ON\tFOR\tANSWERS\tTOOK\tNOTE")
		for _, hop := range p.Chain {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				hop.Alias, hop.Reading, yesNo(hop.Answers), hopTook(hop), hop.Note)
		}
		return
	}
	_, _ = fmt.Fprintln(w)
	w.header("TRIES\tANSWERS\tTOOK\tNOTE")
	for i, hop := range p.Chain {
		// The reading goes with the note: only some modes have one.
		_, _ = fmt.Fprintf(w, "%d. %s\t%s\t%s\t%s\n",
			i+1, hop.Alias, yesNo(hop.Answers), hopTook(hop),
			appendReading(hop.Note, hop.Reading))
	}
}

func hopTook(hop gateway.RouteHop) string {
	if hop.MS > 0 {
		return fmt.Sprintf("%dms", hop.MS)
	}
	return "-"
}

func appendReading(note, reading string) string {
	switch {
	case reading == "":
		return note
	case note == "":
		return reading
	default:
		return reading + "; " + note
	}
}

type routerReportResponse struct {
	Router *policy.Router        `json:"router"`
	Report store.RouterReport    `json:"report"`
	Limits []control.RouterLimit `json:"limits"`
}

func printRouterReport(w *table, name string, since time.Duration, res routerReportResponse) {
	printRouterTraffic(w, name, since, res)
	printRouterLimits(w, res)
}

// printRouterTraffic shows the split between destinations, which says whether
// routing works: a step that sends everything to one place looks healthy in
// every other number.
func printRouterTraffic(w *table, name string, since time.Duration, res routerReportResponse) {
	rep := res.Report
	show(w, "router", name)
	if res.Router == nil {
		show(w, "state", "deleted - this is the traffic it placed while it existed")
	}
	show(w, "window", "the last "+shortDuration(since))
	if rep.Total.Requests == 0 {
		why := "A router is reached by a client naming it where it would name a model, " +
			"so either no client has been configured with it, or nothing called it in this window."
		if res.Router == nil {
			why = "No router of this alias exists in this organisation."
		}
		_, _ = fmt.Fprintf(w, "\nnothing named this router in this window. %s\n", why)
		return
	}
	show(w, "requests", fmt.Sprintf("%d", rep.Total.Requests))
	if rep.Total.Refused > 0 {
		show(w, "refused", fmt.Sprintf("%d (%s) - by a step of it, or the key's guardrails",
			rep.Total.Refused, share(rep.Total.Refused, rep.Total.Requests)))
	}
	if rep.Total.Failed > 0 {
		show(w, "failed", fmt.Sprintf("%d (%s)", rep.Total.Failed,
			share(rep.Total.Failed, rep.Total.Requests)))
	}
	if placed := rep.Chose + rep.FellBack + rep.Errored; placed > 0 {
		show(w, "decided", fmt.Sprintf("%d (%s)", rep.Chose, share(rep.Chose, placed)))
		if rep.FellBack > 0 {
			// A request that fell back was answered, so it shows only here.
			show(w, "fell back", fmt.Sprintf("%d (%s) - answered by a destination other "+
				"than the route step's first choice", rep.FellBack, share(rep.FellBack, placed)))
		}
		if rep.Errored > 0 {
			show(w, "could not place", fmt.Sprintf("%d (%s) - refused for want of a fallback, "+
				"or every destination failed", rep.Errored, share(rep.Errored, placed)))
		}
		show(w, "deciding cost", fmt.Sprintf("%dms median, %dms p95",
			rep.DecisionMedianMS, rep.DecisionP95MS))
	}
	show(w, "spend", policy.FormatMoney(rep.Total.CostMicros)+
		" (the models' bill, with its filters and decisions in it)")
	if len(rep.Destinations) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	w.header("MODEL\tREQUESTS\tSHARE\tTOKENS\tSPEND (" + policy.Currency + ")\tMEDIAN TTFT")
	for _, d := range rep.Destinations {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%dms\n",
			d.Alias, d.Requests, share(d.Requests, rep.Total.Requests),
			d.InputTokens+d.OutputTokens, policy.FormatAmount(d.CostMicros), d.TTFTMedianMS)
	}
}

// printRouterLimits shows how much of each budget and daily or monthly rate
// limit is used. Each counts in its own window, not the report's.
func printRouterLimits(w *table, res routerReportResponse) {
	if len(res.Limits) == 0 {
		return
	}
	var full []string
	_, _ = fmt.Fprintln(w)
	w.header("LIMIT\tUSED\tOF\tRESETS")
	for _, l := range res.Limits {
		var (
			used, of string
			frac     float64
			resets   time.Time
		)
		switch {
		case l.Budget != nil:
			b := l.Budget
			used, frac = policy.FormatMoney(b.SpendMicros), fraction(b.SpendMicros, b.BudgetMicros)
			of, resets = policy.FormatMoney(b.BudgetMicros)+" per "+string(b.Period), b.ResetsAt
		case l.Rate != nil:
			r := l.Rate
			var usedBits, ofBits []string
			for _, c := range []struct {
				used, limit int64
				unit        string
			}{{r.RequestsUsed, r.Requests, "req"}, {r.TokensUsed, r.Tokens, "tok"}} {
				if c.limit > 0 {
					usedBits = append(usedBits, fmt.Sprintf("%d %s", c.used, c.unit))
					ofBits = append(ofBits, fmt.Sprintf("%d %s", c.limit, c.unit))
					frac = max(frac, fraction(c.used, c.limit))
				}
			}
			used = strings.Join(usedBits, ", ")
			of, resets = strings.Join(ofBits, ", ")+" per "+string(r.Period), r.Period.Next(time.Now())
		default:
			continue
		}
		used += fmt.Sprintf(" (%.0f%%)", frac*100)
		switch {
		case frac >= 1:
			used = style.bad(used)
			full = append(full, l.Step)
		case frac >= 0.8:
			used = style.warn(used)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", l.Step, used, of,
			resets.Local().Format("2006-01-02 15:04"))
	}
	for _, id := range full {
		then := "refuses requests"
		if s := routerStep(res.Router, id); s != nil && s.Exceeded != nil {
			then = "sends requests down its exceeded branch"
		}
		_, _ = fmt.Fprintf(w, "\n%s is used up: it %s until it resets.\n", id, then)
	}
}

func fraction(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(used) / float64(limit)
}

func routerStep(p *policy.Router, id string) *policy.Step {
	if p == nil {
		return nil
	}
	for s := range p.Steps() {
		if s.ID == id {
			return s
		}
	}
	return nil
}
