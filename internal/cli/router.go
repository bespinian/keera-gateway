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

// routerRun is one 'keera router' invocation.
//
// A router is a tree, which flags cannot spell, so its steps come from a JSON
// file. The flags of a route step are the exception: the simplest router is
// one route step, and changing one step should not need the whole file. See
// docs/routers.md.
type routerRun struct {
	*aliasRun

	since       time.Duration
	file        string
	displayName string
	description string

	// The route step's own flags. step names the step they change, or which
	// one to check.
	step         string
	mode         string
	model        string
	destinations string
	fallback     string
	noFallback   bool
	prompt       string
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
			"places it by how much text is in it; 'fallback', 'latency' and 'least-busy' try "+
			"the destinations until one answers, in the order written, fastest first, or "+
			"emptiest first (default: instruction)")
	fs.StringVar(&r.model, "model", "", "the model that makes the decision - a fast local one, or a decision model")
	fs.StringVar(&r.destinations, "destinations", "",
		"comma-separated models the route step may choose between; on a fallback step, in the "+
			"order to try them, on a latency or least-busy step, the order to break a tie in, "+
			"and on a size step, each with its ceiling - 'keera-speed:4k,big'")
	// Two flags rather than one string: an absent string cannot be told from an
	// empty one, and on 'set' that is the difference between keeping the
	// fallback and refusing what the step cannot place.
	fs.StringVar(&r.fallback, "fallback", "",
		"where a request goes when the decision cannot be made; must be one of the destinations")
	fs.BoolVar(&r.noFallback, "no-fallback", false,
		"refuse a request the route step cannot place, rather than sending it to a fallback")
	fs.StringVar(&r.prompt, "prompt", "", "the instruction the deciding model is given; @path reads a file")
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
		return fmt.Errorf("--mode is %q; use instruction, size, fallback, latency or "+
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

// routeFlags reports whether a flag of a route step was given.
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

// routeStep finds the route step the flags change: the one --step names, or
// the router's only one. A router with no steps yet gets one, which makes the
// simplest router.
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
	// A step that changes mode keeps nothing the new mode does not use: the
	// control plane refuses, say, a fallback step that carries a deciding
	// model.
	if !s.Mode.Decides() {
		s.Model, s.Prompt, s.Fallback = "", "", ""
	}
	switch {
	case r.noFallback:
		s.Fallback = ""
	case r.fallback != "":
		if !s.Mode.Decides() {
			return needsDecider("fallback", s.Mode)
		}
		s.Fallback = r.fallback
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

// needsDecider refuses a flag that only an instruction step reads.
func needsDecider(flag string, mode policy.RouteMode) error {
	return fmt.Errorf("--%s is for a route step that decides with a model; a %s step "+
		"decides nothing, and what orders its destinations is %s", flag, mode, cliOrdersBy(mode))
}

// readRouterFile reads the steps from a file, or stdin for "-". The file
// holds either the first step, or a whole router with 'start', as 'show
// --json' prints it. Names in a whole router are kept unless a flag says
// otherwise.
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

// decodeStrict refuses a field it does not know, as the control plane does.
// Dropped quietly, a misspelt "exceded" would save a limit that refuses
// instead of branching.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// nameSteps gives every step without an id one made from its kind, such as
// "filter-2", so a file need not number its steps. A limit counts under its
// id, so a limit that should keep its counts across edits is given one in the
// file.
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
	// A route step's instruction is what it decides by, so it is printed
	// whole, which a line of the tree cannot do.
	for s := range p.Steps() {
		if s.Kind == policy.StepRoute && s.Mode.Decides() && s.Prompt != "" {
			printBlock(w, "instruction ("+s.ID+")", strings.Split(s.Prompt, "\n"))
		}
	}
}

// stepTree draws the steps from s on, one line each. A step that hands the
// request on to one next step is followed by it at the same depth; where the
// request can go two ways, each way is a branch drawn under it.
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

// branch is one way a request can go from a step, and the steps along it.
type branch struct {
	label string
	lines []string
}

// routeBranches is one branch per destination: its steps, or a note that it
// is sent straight there. The step's own order comes first; a branch for a
// destination it no longer has is still shown, so it can be taken out.
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
		if next := s.Branches[d]; next != nil {
			out = append(out, branch{label, stepTree(next)})
			continue
		}
		out = append(out, branch{label + ", sent straight there", nil})
	}
	return out
}

// branchLines draws branches with box-drawing lines, so each step can be
// followed back to the branch it is on.
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

// stepLine says what one step does. A limit and a route step name their id
// too: a limit's counts are kept under it, and --step names a route step by
// it.
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

// routeLine says how a route step chooses, and what it does when it cannot.
func routeLine(s *policy.Step) string {
	switch {
	case s.Mode.Decides():
		return "decided by " + style.cmd(s.Model) + ", " + fallbackLabel(s.Fallback) +
			" when it cannot"
	case s.Mode.Sizes():
		return "by size"
	case s.Mode.Measures():
		return "by " + cliOrdersBy(s.Mode)
	default:
		return "tries in order"
	}
}

// promptPreview is the start of a system prompt, on one line: enough to tell
// two apart in a tree. `show --json` prints the whole text.
func promptPreview(p string) string {
	p = strings.Join(strings.Fields(p), " ")
	if r := []rune(p); len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return p
}

// limitTail is a limit's id, and what happens over it when it has no exceeded
// branch to draw.
func limitTail(s *policy.Step) string {
	tail := " " + style.muted("("+s.ID+")")
	if s.Exceeded == nil {
		tail += ", refuses what is over it"
	}
	return tail
}

// cliOrdersBy says, as a clause, where a route step that reads nothing gets
// its order from.
func cliOrdersBy(mode policy.RouteMode) string {
	switch mode {
	case policy.RouteModeLatency:
		return "what each has lately taken to begin answering, fastest first"
	case policy.RouteModeLeastBusy:
		return "how many requests the gateway has in flight against each, emptiest first"
	case policy.RouteModeSize:
		return "how much text is in the request, against the ceiling on each"
	default:
		return "the order they are given to --destinations"
	}
}

// fallbackLabel says what happens to a request an instruction step cannot
// place.
func fallbackLabel(fallback string) string {
	if fallback == "" {
		return "refuses the request"
	}
	return "falls back to " + fallback
}

// printRouterCheck prints what checking each route step found.
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
	if p.Mode.Sizes() {
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
	// Each sample beside where it went: whether a destination is right depends
	// on what the sample asked for.
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
		// Beside the choice it qualifies: a step right but barely sure is
		// what this check is for.
		_, _ = fmt.Fprintf(w, "  chose\t%s (%.0f%% sure)\n", d.Chosen, d.Confidence*100)
	default:
		_, _ = fmt.Fprintf(w, "  chose\t%s\n", d.Chosen)
	}
}

// printRouteChain lists the destinations in the order they would be tried.
// The first that answers is serving every request right now.
func printRouteChain(w *table, p gateway.RouteProbe) {
	if len(p.Chain) == 0 {
		return
	}
	// A size step's bands are configuration, not a reading, so they get a
	// column of their own.
	if p.Mode.Sizes() {
		_, _ = fmt.Fprintln(w)
		w.header("PLACED ON\tTAKES\tANSWERS\tTOOK\tNOTE")
		for _, hop := range p.Chain {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				hop.Alias, hop.Reading, yesNo(hop.Answers), hopTook(hop), hop.Note)
		}
		return
	}
	_, _ = fmt.Fprintln(w)
	w.header("TRIES\tANSWERS\tTOOK\tNOTE")
	for i, hop := range p.Chain {
		// The reading goes with the note: only some modes have one, and an
		// empty column would look like a measurement of nothing.
		_, _ = fmt.Fprintf(w, "%d. %s\t%s\t%s\t%s\n",
			i+1, hop.Alias, yesNo(hop.Answers), hopTook(hop),
			appendReading(hop.Note, hop.Reading))
	}
}

// hopTook is how long one destination took to answer, or a dash.
func hopTook(hop gateway.RouteHop) string {
	if hop.MS > 0 {
		return fmt.Sprintf("%dms", hop.MS)
	}
	return "-"
}

// appendReading joins a hop's note to what the step measured of it.
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

// routerReportResponse is what /v1/routers/{alias}/report answers with.
type routerReportResponse struct {
	Router *policy.Router     `json:"router"`
	Report store.RouterReport `json:"report"`
}

// printRouterReport is how much went through a router, which models answered
// it, and how its route steps decided. The split between destinations is what
// says whether routing works: a step that sends everything to one place
// looks healthy in every other number.
func printRouterReport(w *table, name string, since time.Duration, res routerReportResponse) {
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
			// Every request that fell back was answered, so this line is the
			// only place it shows.
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
