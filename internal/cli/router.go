package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// routerRun is one 'keera router' invocation.
type routerRun struct {
	c     *client
	fs    *flag.FlagSet
	sub   string
	orgID string

	org          string
	mode         string
	model        string
	destinations string
	fallback     string
	noFallback   bool
	prompt       string
	description  string
	since        time.Duration
	yes          bool
	asJSON       bool
}

func routerCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("router "+sub, flag.ExitOnError)
	r := &routerRun{c: newClient(), fs: fs, sub: sub}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.StringVar(&r.mode, "mode", "",
		"what chooses: 'instruction' reads the request with a model; 'size' places it by how "+
			"much text is in it; 'fallback', 'latency' and 'least-busy' try the destinations "+
			"until one answers, in the order written, fastest first, or emptiest first "+
			"(default: instruction)")
	fs.StringVar(&r.model, "model", "", "the model that makes the decision - a fast local one")
	fs.StringVar(&r.destinations, "destinations", "",
		"comma-separated models this router may choose between; on a fallback router, in the "+
			"order to try them, on a latency or least-busy router, the order to break a "+
			"tie in, and on a size router, each with its ceiling - 'keera-speed:4k,big'")
	// Two flags rather than one string: an absent string cannot be told from an
	// empty one, and on 'set' that is the difference between keeping the
	// fallback and refusing what the router cannot place.
	fs.StringVar(&r.fallback, "fallback", "",
		"where a request goes when the decision cannot be made; must be one of the destinations")
	fs.BoolVar(&r.noFallback, "no-fallback", false,
		"refuse a request the router cannot place, rather than sending it to a fallback")
	fs.StringVar(&r.prompt, "prompt", "", "the instruction the deciding model is given; @path reads a file")
	fs.StringVar(&r.description, "description", "", "what this router is for, for whoever reads the list")
	fs.DurationVar(&r.since, "since", 7*24*time.Hour, "how far back 'report' looks")
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "router", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "router", want)
	}
	if err := parse(fs, rest); err != nil {
		return err
	}
	if r.fallback != "" && r.noFallback {
		return errors.New("--fallback and --no-fallback are opposites; pass one of them")
	}
	if !policy.ValidRouterMode(policy.RouterMode(r.mode)) {
		return fmt.Errorf("--mode is %q; it is 'instruction' (a model reads each request and "+
			"names the destination), 'size' (the request goes to the smallest destination "+
			"its size was meant for), or one of the three that read nothing and try the "+
			"destinations until one answers: 'fallback' in the order they are written, "+
			"'latency' fastest first by what each has lately taken to begin answering, "+
			"'least-busy' emptiest first by what the gateway has in flight against each",
			r.mode)
	}
	orgID, err := resolveOrg(ctx, r.c, r.org)
	if err != nil {
		return err
	}
	r.orgID = orgID

	switch sub {
	case "list", "ls", "":
		return r.list(ctx)
	case "add", "create", "new", "set", "edit", "update":
		return r.put(ctx)
	case "check", "probe", "test":
		return r.check(ctx)
	case "report", "stats":
		return r.report(ctx)
	case "delete", "rm", "remove":
		return r.delete(ctx)
	default:
		return unknownSub("router", sub)
	}
}

func (r *routerRun) list(ctx context.Context) error {
	routers, err := list[policy.Router](ctx, r.c, inOrg("/v1/routers", r.orgID))
	if err != nil {
		return err
	}
	return out(r.asJSON, routers, func(w *table) { printRouters(w, routers) })
}

func (r *routerRun) delete(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera router delete <alias> [--yes]")
	}
	alias := r.fs.Arg(0)
	if !r.yes {
		if err := confirmRouterDelete(alias); err != nil {
			return err
		}
	}
	return deleteAlias(ctx, r.c, r.path(alias, ""), alias, r.asJSON)
}

// path addresses one router, or with a suffix a route under it.
func (r *routerRun) path(alias, suffix string) string {
	return inOrg("/v1/routers/"+url.PathEscape(alias)+suffix, r.orgID)
}

func (r *routerRun) put(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf(
			"usage: keera router %s <alias> --model <alias> --destinations a,b --prompt <text>",
			r.sub)
	}
	alias := r.fs.Arg(0)
	// 'set' starts from the stored router, so what is not given is kept. 'add'
	// starts from nothing, and the control plane refuses what is missing.
	rt := policy.Router{Alias: alias}
	if r.sub == "set" {
		existing, err := requireRouter(ctx, r.c, r.orgID, alias)
		if err != nil {
			return err
		}
		rt = existing
	}
	if err := r.apply(&rt); err != nil {
		return err
	}
	return putRouter(ctx, r.c, r.orgID, rt, r.asJSON)
}

// apply writes the given flags into rt.
func (r *routerRun) apply(rt *policy.Router) error {
	if r.mode != "" {
		rt.Mode = policy.RouterMode(r.mode)
	}
	if rt.Mode == "" {
		rt.Mode = policy.RouterModeInstruction
	}
	if r.model != "" {
		rt.Model = r.model
	}
	if r.destinations != "" {
		aliases, ceilings, err := parseDestinations(r.destinations)
		if err != nil {
			return err
		}
		if len(ceilings) > 0 && !rt.Sizes() {
			return fmt.Errorf("a ceiling was set on a destination, and this is a %s "+
				"router: nothing there reads the size of a request. Use --mode size "+
				"for a router that does", rt.Mode)
		}
		rt.Destinations, rt.Ceilings = aliases, ceilings
	}
	switch {
	case r.noFallback:
		rt.Fallback = ""
	case r.fallback != "":
		rt.Fallback = r.fallback
	}
	// A router that changes mode keeps nothing the new mode does not use: the
	// endpoint refuses, say, a fallback router that carries a deciding model.
	if !rt.Decides() {
		rt.Model, rt.Prompt, rt.Fallback = "", "", ""
	}
	if !rt.Sizes() {
		rt.Ceilings = nil
	}
	if r.prompt != "" {
		if !rt.Decides() {
			return fmt.Errorf("--prompt is for a router that decides with a model; a %s "+
				"router is given none, and what orders its destinations is %s",
				rt.Mode, cliOrdersBy(rt.Mode))
		}
		text, err := textOrFile(r.prompt)
		if err != nil {
			return err
		}
		rt.Prompt = text
	}
	if r.description != "" {
		rt.Description = r.description
	}
	return nil
}

func (r *routerRun) check(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera router check <alias>")
	}
	var probe gateway.RouterProbe
	if err := r.c.do(ctx, "POST", r.path(r.fs.Arg(0), "/check"), nil, &probe); err != nil {
		return err
	}
	if err := out(r.asJSON, probe, func(w *table) { printRouterProbe(w, probe) }); err != nil {
		return err
	}
	if !probe.OK {
		return errors.New("the router did not pass its check")
	}
	return nil
}

func (r *routerRun) report(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera router report <alias> [--since 168h]")
	}
	var res routerReportResponse
	if err := r.c.do(ctx, "GET",
		r.path(r.fs.Arg(0), "/report")+"&from="+url.QueryEscape(sinceParam(r.since)),
		nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		printRouterReport(w, r.fs.Arg(0), r.since, res)
	})
}

// requireRouter reads one router from the list, as requireFilter does.
func requireRouter(ctx context.Context, c *client, orgID, alias string) (policy.Router, error) {
	routers, err := list[policy.Router](ctx, c, inOrg("/v1/routers", orgID))
	if err != nil {
		return policy.Router{}, err
	}
	for _, rt := range routers {
		if rt.Alias == alias {
			return rt, nil
		}
	}
	return policy.Router{}, fmt.Errorf("no router %s (see: keera router list)", alias)
}

func putRouter(ctx context.Context, c *client, orgID string, rt policy.Router, asJSON bool) error {
	var saved policy.Router
	if err := c.do(ctx, "PUT",
		"/v1/routers/"+url.PathEscape(rt.Alias)+"?org_id="+url.QueryEscape(orgID),
		map[string]any{
			"mode": rt.Mode, "model": rt.Model, "prompt": rt.Prompt,
			"destinations": rt.Destinations, "ceilings": rt.Ceilings,
			// Always sent, so that clearing the fallback works.
			"fallback": rt.Fallback, "description": rt.Description,
		}, &saved); err != nil {
		return err
	}
	return out(asJSON, saved, func(w *table) { printRouter(w, saved) })
}

// confirmRouterDelete asks before removing a router. The control plane guards
// the scopes that use one, but not the clients: they name routers in their
// own configuration, and would get "model not found".
func confirmRouterDelete(alias string) error {
	fmt.Printf("Deleting the router %s. Any client still naming it will be told the model "+
		"does not exist.\n", alias)
	return confirmTyping("alias", alias, "nothing was deleted")
}

func printRouters(w *table, routers []policy.Router) {
	w.header("ALIAS\tMODE\tDECIDES WITH\tDESTINATIONS\tCANNOT PLACE\tDESCRIPTION")
	for _, rt := range routers {
		decidesWith := rt.Model
		if !rt.Decides() {
			decidesWith = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			rt.Alias, modeLabel(rt), decidesWith, destinationsLabel(rt), fallbackLabel(rt),
			rt.Description)
	}
}

// modeLabel is the router's mode, including one stored before there were modes.
func modeLabel(rt policy.Router) string {
	if rt.Mode == "" {
		return string(policy.RouterModeInstruction)
	}
	return string(rt.Mode)
}

// cliOrdersBy says, as a clause, where a router that reads nothing gets its
// order from.
func cliOrdersBy(mode policy.RouterMode) string {
	switch mode {
	case policy.RouterModeLatency:
		return "what each has lately taken to begin answering, fastest first"
	case policy.RouterModeLeastBusy:
		return "how many requests the gateway has in flight against each, emptiest first"
	case policy.RouterModeSize:
		return "how much text is in the request, against the ceiling on each"
	default:
		return "the order they are given to --destinations"
	}
}

// destinationsLabel lists the destinations. Arrows only where the written
// order is the order tried: a measured router's order changes with traffic.
func destinationsLabel(rt policy.Router) string {
	switch {
	case rt.Sizes():
		// The ceilings are the point of a size router, so they go in the cell.
		return formatDestinations(rt)
	case rt.Decides(), rt.Measures():
		return strings.Join(rt.Destinations, ", ")
	default:
		return strings.Join(rt.Destinations, " -> ")
	}
}

// fallbackLabel says what happens to a request the router cannot place. A
// router that reads nothing has tried every destination by then.
func fallbackLabel(rt policy.Router) string {
	switch {
	case rt.Sizes():
		return "the next destination up, then the last one's answer"
	case !rt.Decides():
		return "the last one's answer"
	case rt.Refuses():
		return "refuse the request"
	default:
		return "-> " + rt.Fallback
	}
}

// routerState is one line saying what steers a router, for the head of its
// report.
func routerState(rt policy.Router) string {
	switch {
	case rt.Decides():
		return "decides with " + rt.Model + ", " + fallbackLabel(rt) + " when it cannot"
	case rt.Sizes():
		return "places by size between " + destinationsLabel(rt)
	case rt.Measures():
		return "tries " + destinationsLabel(rt) + ", in the order of " + cliOrdersBy(rt.Mode)
	default:
		return "tries " + destinationsLabel(rt)
	}
}

func printRouter(w *table, rt policy.Router) {
	show(w, "alias", rt.Alias)
	show(w, "mode", string(rt.Mode))
	if !rt.Decides() {
		printRouterOrder(w, rt)
		show(w, "cannot place", fallbackLabel(rt))
		if rt.Description != "" {
			show(w, "description", rt.Description)
		}
		return
	}
	show(w, "decides with", rt.Model)
	show(w, "destinations", strings.Join(rt.Destinations, ", "))
	show(w, "cannot decide", fallbackLabel(rt))
	if rt.Description != "" {
		show(w, "description", rt.Description)
	}
	// The instruction is the router, so it is printed whole.
	printBlock(w, "instruction", strings.Split(rt.Prompt, "\n"))
}

// printRouterOrder says how a router that reads nothing orders its
// destinations.
func printRouterOrder(w *table, rt policy.Router) {
	switch {
	case rt.Sizes():
		show(w, "places between", destinationsLabel(rt))
		show(w, "by", cliOrdersBy(rt.Mode))
		// The destination with no ceiling is easy to misread as unfinished.
		if unbounded := rt.Unbounded(); len(unbounded) == 1 {
			show(w, "above every ceiling", unbounded[0])
		}
	case rt.Measures():
		show(w, "tries", destinationsLabel(rt))
		show(w, "in the order of", cliOrdersBy(rt.Mode))
	default:
		show(w, "tries in order", destinationsLabel(rt))
	}
}

func printRouterProbe(w *table, p gateway.RouterProbe) {
	show(w, "router", p.Alias)
	if p.Model != "" {
		show(w, "decides with", p.Model)
	}
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
		show(w, "cost of these runs", policy.FormatMicros(p.CostMicros))
	}
	if p.Error != "" {
		show(w, "error", p.Error)
	}
	for _, warning := range p.Warnings {
		show(w, "warning", warning)
	}
	// Each sample beside where it went: whether a destination is right depends
	// on what the sample asked for.
	for i, d := range p.Decisions {
		printDecision(w, i, d)
	}
	printRouterChain(w, p)
}

func printDecision(w *table, i int, d gateway.RouterDecision) {
	_, _ = fmt.Fprintf(w, "\nsample %d\t%s\n", i+1, d.Asks)
	_, _ = fmt.Fprintf(w, "  sent\t%s\n", firstLine(d.Prompt))
	switch {
	case d.Error != "":
		_, _ = fmt.Fprintf(w, "  could not decide\t%s\n", d.Error)
	case d.Confidence > 0:
		// Beside the choice it qualifies: a router right but barely sure is
		// what this check is for.
		_, _ = fmt.Fprintf(w, "  chose\t%s (%.0f%% sure)\n", d.Chosen, d.Confidence*100)
	default:
		_, _ = fmt.Fprintf(w, "  chose\t%s\n", d.Chosen)
	}
}

// printRouterChain lists the destinations in the order they would be tried.
// The first that answers is serving every request right now.
func printRouterChain(w *table, p gateway.RouterProbe) {
	if len(p.Chain) == 0 {
		return
	}
	// A size router's bands are configuration, not a reading, so they get a
	// column of their own.
	if p.Mode.Sizes() {
		_, _ = fmt.Fprintln(w, "\nPLACED ON\tTAKES\tANSWERS\tTOOK\tNOTE")
		for _, hop := range p.Chain {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				hop.Alias, hop.Reading, yesNo(hop.Answers), hopTook(hop), hop.Note)
		}
		return
	}
	_, _ = fmt.Fprintln(w, "\nTRIES\tANSWERS\tTOOK\tNOTE")
	for i, hop := range p.Chain {
		// The reading goes with the note: only some modes have one, and an
		// empty column would look like a measurement of nothing.
		_, _ = fmt.Fprintf(w, "%d. %s\t%s\t%s\t%s\n",
			i+1, hop.Alias, yesNo(hop.Answers), hopTook(hop),
			appendReading(hop.Note, hop.Reading))
	}
}

// hopTook is how long one destination took to answer, or a dash.
func hopTook(hop gateway.RouterHop) string {
	if hop.MS > 0 {
		return fmt.Sprintf("%dms", hop.MS)
	}
	return "-"
}

// appendReading joins a hop's note to what the router measured of it.
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

// routerReportResponse is what /v1/routers/{name}/report answers with.
type routerReportResponse struct {
	Router   *policy.Router     `json:"router"`
	Report   store.RouterReport `json:"report"`
	Currency string             `json:"currency"`
}

// printRouterReport is where a router has been sending traffic. The split
// between destinations is what says whether it works: a router that sends
// everything to one place looks healthy in every other number.
func printRouterReport(w *table, name string, since time.Duration,
	res routerReportResponse,
) {
	rep := res.Report
	show(w, "router", name)
	if res.Router == nil {
		show(w, "state", "deleted - this is the traffic it placed while it existed")
	} else {
		show(w, "state", routerState(*res.Router))
	}
	show(w, "window", "the last "+since.String())

	if rep.Total.Requests == 0 {
		_, _ = fmt.Fprintf(w, "\nnothing named this router in this window. %s\n",
			noRoutedReason(res))
		return
	}

	show(w, "requests placed", fmt.Sprintf("%d", rep.Total.Requests))
	show(w, "decided", fmt.Sprintf("%d (%s)", rep.Chose, share(rep.Chose, rep.Total.Requests)))
	if rep.FellBack > 0 {
		// Every request that fell back was answered, so this line is the only
		// place it shows.
		show(w, "fell back", fmt.Sprintf("%d (%s) - %s",
			rep.FellBack, share(rep.FellBack, rep.Total.Requests), fellBackMeans(res)))
	}
	if rep.Errored > 0 {
		show(w, "could not place", fmt.Sprintf("%d (%s) - %s",
			rep.Errored, share(rep.Errored, rep.Total.Requests), couldNotPlaceMeans(res)))
	}
	show(w, "deciding cost", fmt.Sprintf("%dms median, %dms p95",
		rep.DecisionMedianMS, rep.DecisionP95MS))
	show(w, "spend", policy.FormatMicros(rep.Total.CostMicros)+
		" (the destinations' bill, with the decisions in it)")

	if len(rep.Destinations) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nDESTINATION\tREQUESTS\tSHARE\tTOKENS\tSPEND\tMEDIAN TTFT")
	for _, d := range rep.Destinations {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%dms\n",
			d.Alias, d.Requests, share(d.Requests, rep.Total.Requests),
			d.InputTokens+d.OutputTokens, policy.FormatMicros(d.CostMicros), d.TTFTMedianMS)
	}
}

// noRoutedReason says why a router saw no traffic. Clients name routers, so
// usually no client has been told to.
func noRoutedReason(res routerReportResponse) string {
	if res.Router == nil {
		return "No router of this name exists in this organisation."
	}
	return "A router is reached by a client naming it where it would name a model, " +
		"so either no client has been configured with it, or nothing called this week."
}

// fellBackMeans says what a fallback means for this kind of router: a
// decision that could not be made, a destination that was down, or a ranking
// that was wrong.
func fellBackMeans(res routerReportResponse) string {
	switch {
	case res.Router == nil:
		return "each of those was placed somewhere other than the router's first choice"
	case res.Router.Sizes():
		return "each of those was answered by a destination other than the one its size " +
			"was meant for, after waiting for that one to fail"
	case res.Router.Measures():
		return "each of those was answered by a destination other than the one this " +
			"router ranked first, after waiting for that one to fail"
	case !res.Router.Decides():
		return "each of those was answered further down the chain, after waiting for the " +
			"destinations before it to fail"
	default:
		return "each of those went to '" + res.Router.Fallback + "' without a decision"
	}
}

// couldNotPlaceMeans says what running out of options was. Only a router that
// decides can refuse before trying anything.
func couldNotPlaceMeans(res routerReportResponse) string {
	if res.Router != nil && !res.Router.Decides() {
		return "each of those ran out of destinations: every one of them failed, and the " +
			"client was given whatever the last of them said"
	}
	return "each of those was refused, because this router has no fallback"
}
