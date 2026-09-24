package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// filterRun is one 'keera filter' invocation.
type filterRun struct {
	c     *client
	fs    *flag.FlagSet
	sub   string
	orgID string

	org         string
	model       string
	mode        string
	shadow      bool
	enforce     bool
	prompt      string
	rules       string
	description string
	since       time.Duration
	yes         bool
	asJSON      bool
}

func filterCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("filter "+sub, flag.ExitOnError)
	r := &filterRun{c: newClient(), fs: fs, sub: sub}
	fs.StringVar(&r.org, "org", "", orgUsage)
	fs.StringVar(&r.model, "model", "", "the model the filter runs on")
	fs.StringVar(&r.mode, "mode", "",
		"rewrite (a model edits the request and lets it go), gate (a model judges only "+
			"whether it may go), or pattern (a list of rules is applied, with no model)")
	// Two flags rather than one bool: an absent bool cannot be told from false,
	// and on 'set' that is the difference between leaving shadow alone and
	// switching it off.
	fs.BoolVar(&r.shadow, "shadow", false,
		"run the filter and enforce nothing; what it would have done is recorded")
	fs.BoolVar(&r.enforce, "enforce", false, "act on what the filter answers (the default)")
	fs.StringVar(&r.prompt, "prompt", "", "the instruction its model is given; @path reads a file")
	fs.StringVar(&r.rules, "rules", "",
		"a pattern filter's rules, one per line: an expression, '=>', then what each match "+
			"becomes - or 'REFUSE: why' to drop the request. @path reads a file")
	fs.StringVar(&r.description, "description", "", "what this filter is for, for whoever reads the list")
	fs.DurationVar(&r.since, "since", 7*24*time.Hour, "how far back 'report' looks")
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "filter", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "filter", want)
	}
	if err := parse(fs, rest); err != nil {
		return err
	}
	if r.shadow && r.enforce {
		return errors.New("--shadow and --enforce are opposites; pass one of them")
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
		return unknownSub("filter", sub)
	}
}

func (r *filterRun) list(ctx context.Context) error {
	filters, err := list[policy.Filter](ctx, r.c, inOrg("/v1/filters", r.orgID))
	if err != nil {
		return err
	}
	return out(r.asJSON, filters, func(w *table) { printFilters(w, filters) })
}

func (r *filterRun) delete(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera filter delete <alias> [--yes]")
	}
	alias := r.fs.Arg(0)
	if !r.yes {
		if err := confirmFilterDelete(alias); err != nil {
			return err
		}
	}
	return deleteAlias(ctx, r.c, r.path(alias, ""), alias, r.asJSON)
}

// path addresses one filter, or with a suffix a route under it.
func (r *filterRun) path(alias, suffix string) string {
	return inOrg("/v1/filters/"+url.PathEscape(alias)+suffix, r.orgID)
}

func (r *filterRun) put(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera filter %s <alias> --model <model> --prompt <text>", r.sub)
	}
	alias := r.fs.Arg(0)
	// 'set' starts from the stored filter, so what is not given is kept. 'add'
	// starts from nothing, and the control plane refuses what is missing.
	f := policy.Filter{Alias: alias}
	if r.sub == "set" {
		existing, err := requireFilter(ctx, r.c, r.orgID, alias)
		if err != nil {
			return err
		}
		f = existing
	}
	if err := r.apply(&f); err != nil {
		return err
	}
	return putFilter(ctx, r.c, r.orgID, f, r.asJSON)
}

// apply writes the given flags into f.
func (r *filterRun) apply(f *policy.Filter) error {
	if r.model != "" {
		f.Model = r.model
	}
	if r.mode != "" {
		f.Mode = policy.FilterMode(r.mode)
	}
	if r.shadow || r.enforce {
		f.Shadow = r.shadow
	}
	// A filter that changes kind keeps nothing of the other kind: the endpoint
	// refuses a pattern filter that carries a model, and the other way round.
	if f.UsesModel() {
		f.Rules = nil
	} else {
		f.Model, f.Prompt = "", ""
	}
	if r.prompt != "" {
		if !f.UsesModel() {
			return errors.New("--prompt is for a filter that reads the request with a " +
				"model; a pattern filter applies --rules and reads no prose at all")
		}
		text, err := textOrFile(r.prompt)
		if err != nil {
			return err
		}
		f.Prompt = text
	}
	if r.rules != "" {
		if f.UsesModel() {
			return fmt.Errorf("--rules is for a pattern filter; a %s filter reads the "+
				"request with a model and is given --prompt", filterMode(*f))
		}
		text, err := textOrFile(r.rules)
		if err != nil {
			return err
		}
		parsed, err := parseRules(text)
		if err != nil {
			return err
		}
		f.Rules = parsed
	}
	if r.description != "" {
		f.Description = r.description
	}
	return nil
}

func (r *filterRun) check(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera filter check <alias>")
	}
	var probe gateway.FilterProbe
	if err := r.c.do(ctx, "POST", r.path(r.fs.Arg(0), "/check"), nil, &probe); err != nil {
		return err
	}
	if err := out(r.asJSON, probe, func(w *table) { printFilterProbe(w, probe) }); err != nil {
		return err
	}
	if !probe.OK {
		return errors.New("the filter did not pass its check")
	}
	return nil
}

func (r *filterRun) report(ctx context.Context) error {
	if r.fs.NArg() != 1 {
		return fmt.Errorf("usage: keera filter report <alias> [--since 168h]")
	}
	var res filterReportResponse
	if err := r.c.do(ctx, "GET",
		r.path(r.fs.Arg(0), "/report")+"&from="+url.QueryEscape(sinceParam(r.since)),
		nil, &res); err != nil {
		return err
	}
	return out(r.asJSON, res, func(w *table) {
		printFilterReport(w, r.fs.Arg(0), r.since, res)
	})
}

// deleteAlias deletes the filter or router at path and says so.
func deleteAlias(ctx context.Context, c *client, path, alias string, asJSON bool) error {
	var res map[string]any
	if err := c.do(ctx, "DELETE", path, nil, &res); err != nil {
		return err
	}
	return out(asJSON, res, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted\t%s\n", alias)
	})
}

// requireFilter reads one filter from the list. There is no endpoint for a
// single filter; the list is small.
func requireFilter(ctx context.Context, c *client, orgID, alias string) (policy.Filter, error) {
	filters, err := list[policy.Filter](ctx, c, inOrg("/v1/filters", orgID))
	if err != nil {
		return policy.Filter{}, err
	}
	for _, f := range filters {
		if f.Alias == alias {
			return f, nil
		}
	}
	return policy.Filter{}, fmt.Errorf("no filter %s (see: keera filter list)", alias)
}

func putFilter(ctx context.Context, c *client, orgID string, f policy.Filter, asJSON bool) error {
	var saved policy.Filter
	if err := c.do(ctx, "PUT",
		"/v1/filters/"+url.PathEscape(f.Alias)+"?org_id="+url.QueryEscape(orgID),
		map[string]any{
			"model": f.Model, "mode": f.Mode, "shadow": f.Shadow, "prompt": f.Prompt,
			"rules": f.Rules, "description": f.Description,
		}, &saved); err != nil {
		return err
	}
	return out(asJSON, saved, func(w *table) { printFilter(w, saved) })
}

// confirmFilterDelete asks before removing a filter. The control plane already
// refuses to delete one a guardrail names, so this is about a filter nothing
// uses today, whose wording is kept nowhere else.
func confirmFilterDelete(alias string) error {
	fmt.Printf("Deleting the filter %s. Its instruction is not kept anywhere else.\n", alias)
	return confirmTyping("alias", alias, "nothing was deleted")
}

func printFilters(w *table, filters []policy.Filter) {
	w.header("ALIAS\tMODE\tMODEL\tDESCRIPTION\tINSTRUCTION")
	for _, f := range filters {
		model := f.Model
		if !f.UsesModel() {
			model = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			f.Alias, filterModeLabel(f), model, f.Description, filterWhat(f))
	}
}

// filterWhat is one line saying what a filter does: its instruction, or for a
// pattern filter how many rules it holds.
func filterWhat(f policy.Filter) string {
	if f.UsesModel() {
		return firstLine(f.Prompt)
	}
	return plural(len(f.Rules), "rule")
}

// filterModeLabel is the mode, and whether the filter is enforced at all: a
// "gate" in shadow enforces nothing.
func filterModeLabel(f policy.Filter) string {
	if f.Shadow {
		return string(filterMode(f)) + " (shadow)"
	}
	return string(filterMode(f))
}

// filterMode is a filter's mode, including one stored before there were modes.
func filterMode(f policy.Filter) policy.FilterMode {
	if f.Mode == "" {
		return policy.FilterModeRewrite
	}
	return f.Mode
}

func printFilter(w *table, f policy.Filter) {
	show(w, "alias", f.Alias)
	show(w, "mode", filterModeLabel(f))
	if f.UsesModel() {
		show(w, "model", f.Model)
	} else {
		show(w, "runs on", "nothing - its rules are applied in the gateway")
	}
	if f.Shadow {
		show(w, "enforcing", "no - it runs, and the request goes on as it was sent")
	}
	if f.Description != "" {
		show(w, "description", f.Description)
	}
	// The instruction or the rules are the filter, so they are printed whole,
	// the rules in the form --rules reads.
	if !f.UsesModel() {
		printBlock(w, "rules", formatRules(f.Rules))
		return
	}
	printBlock(w, "instruction", strings.Split(f.Prompt, "\n"))
}

// printBlock writes a heading row and indented lines under it.
func printBlock(w *table, name string, lines []string) {
	_, _ = fmt.Fprintln(w, name+"\t")
	for _, line := range lines {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}

func printFilterProbe(w *table, p gateway.FilterProbe) {
	show(w, "filter", p.Alias)
	show(w, "mode", p.Mode)
	if p.Model != "" {
		show(w, "model", p.Model)
	}
	if p.Shadow {
		show(w, "enforcing", "no - this filter is in shadow, so nothing below is acted on")
	}
	show(w, "result", yesNo(p.OK))
	if p.TotalMS > 0 {
		show(w, "took", fmt.Sprintf("%dms", p.TotalMS))
	}
	switch {
	case p.Free:
		// Said, so a zero cost does not read as one that was not measured.
		show(w, "cost of one run", "nothing - no model runs, so there is nothing to charge "+
			"and nothing added to the wait")
	case p.CostMicros > 0:
		show(w, "cost of one run", policy.FormatMicros(p.CostMicros))
	}
	// Which rules fired, so a pattern filter doing the wrong thing names the line.
	for _, hit := range p.Hits {
		what := "matched once"
		if hit.Matches != 1 {
			what = "matched " + strconv.Itoa(hit.Matches) + " times"
		}
		if hit.Refused {
			what += ", and refused the request"
		}
		_, _ = fmt.Fprintf(w, "rule\t%s (%s)\n", hit.Rule, what)
	}
	if p.Error != "" {
		show(w, "error", p.Error)
	}
	// A refusal is an answer, not a fault, and nothing was rewritten under it.
	if p.Refused {
		reason := p.Refusal
		if reason == "" {
			reason = "no reason given"
		}
		show(w, "refused the sample", reason)
	}
	for _, warning := range p.Warnings {
		show(w, "warning", warning)
	}
	for i, seg := range p.Segments {
		_, _ = fmt.Fprintf(w, "\nsegment %d\t%s\n", i+1, changedLabel(seg.Changed))
		_, _ = fmt.Fprintf(w, "  sent\t%s\n", firstLine(seg.Before))
		_, _ = fmt.Fprintf(w, "  came back\t%s\n", firstLine(seg.After))
	}
	// A gate answers once per sample: one built to be stopped and one not.
	for _, v := range p.Verdicts {
		printVerdict(w, v)
	}
}

func printVerdict(w *table, v gateway.FilterVerdict) {
	_, _ = fmt.Fprintf(w, "\n%s\t%s\n", gateSampleLabel(v), verdictLabel(v))
	for _, sent := range v.Sent {
		_, _ = fmt.Fprintf(w, "  sent\t%s\n", firstLine(sent))
	}
	if v.Reason != "" {
		_, _ = fmt.Fprintf(w, "  reason\t%s\n", v.Reason)
	}
	// A gate right on both samples at 55% cannot really tell them apart.
	if v.Confidence > 0 {
		_, _ = fmt.Fprintf(w, "  how sure\t%.0f%%\n", v.Confidence*100)
	}
}

func gateSampleLabel(v gateway.FilterVerdict) string {
	if v.ExpectRefusal {
		return "a credential and a client"
	}
	return "ordinary source code"
}

func verdictLabel(v gateway.FilterVerdict) string {
	switch {
	case v.Refused == v.ExpectRefusal && v.Refused:
		return "refused, as it should be"
	case v.Refused == v.ExpectRefusal:
		return "allowed, as it should be"
	case v.Refused:
		return "refused - read the warning"
	default:
		return "allowed - read the warning"
	}
}

func changedLabel(changed bool) string {
	if changed {
		return "rewritten"
	}
	return "unchanged"
}

// filterReportResponse is what /v1/filters/{alias}/report answers with.
type filterReportResponse struct {
	Filter    *policy.Filter     `json:"filter"`
	Report    store.FilterReport `json:"report"`
	TeamNames map[string]string  `json:"team_names"`
	Currency  string             `json:"currency"`
}

// printFilterReport is what a filter has been doing, in the order people ask:
// is it firing, what does it do, what does it add to the wait and the bill,
// and which team is living with its refusals.
func printFilterReport(w *table, name string, since time.Duration,
	res filterReportResponse,
) {
	rep := res.Report
	show(w, "filter", name)
	show(w, "state", filterState(res.Filter))
	show(w, "window", "the last "+since.String())

	if rep.Runs == 0 {
		_, _ = fmt.Fprintf(w, "\nnothing ran in this window. %s\n", noRunsReason(res))
		return
	}

	show(w, "runs", fmt.Sprintf("%d of the organisation's %d requests (%s)",
		rep.Runs, rep.Requests, share(rep.Runs, rep.Requests)))
	// Counts say what happened; rates are what an instruction is tuned against.
	show(w, "passed", fmt.Sprintf("%d (%s)", rep.Pass, share(rep.Pass, rep.Runs)))
	if rep.Rewrote > 0 || (res.Filter != nil && res.Filter.Mode.Rewrites()) {
		show(w, "rewrote", fmt.Sprintf("%d (%s), touching %d of %d segments",
			rep.Rewrote, share(rep.Rewrote, rep.Runs), rep.Changed, rep.Segments))
	}
	show(w, "refused", fmt.Sprintf("%d (%s)", rep.Refused, share(rep.Refused, rep.Runs)))
	if rep.Errors > 0 {
		show(w, "could not run", fmt.Sprintf("%d (%s) - %s", rep.Errors,
			share(rep.Errors, rep.Runs), errorConsequence(res)))
	}
	if rep.Shadowed > 0 && rep.Shadowed < rep.Runs {
		show(w, "of those, unenforced", fmt.Sprintf("%d - the filter changed state inside this window",
			rep.Shadowed))
	}
	show(w, "added latency", fmt.Sprintf("%dms median, %dms p95",
		rep.LatencyMedianMS, rep.LatencyP95MS))
	show(w, "spend", fmt.Sprintf("%s of the organisation's %s (%s)",
		policy.FormatMicros(rep.CostMicros), policy.FormatMicros(rep.OrgCostMicros),
		share(rep.CostMicros, rep.OrgCostMicros)))

	if len(rep.Teams) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nTEAM\tRUNS\tREFUSED\tRATE\tREWROTE\tFAILED\tSPEND")
	for _, t := range rep.Teams {
		label := t.TeamID
		if name, ok := res.TeamNames[t.TeamID]; ok && name != "" {
			label = name
		}
		if label == "" {
			label = "(no team)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\t%d\t%s\n",
			label, t.Runs, t.Refused, share(t.Refused, t.Runs), t.Rewrote, t.Errors,
			policy.FormatMicros(t.CostMicros))
	}
}

// filterState is the head line of a filter's report.
func filterState(f *policy.Filter) string {
	switch {
	case f == nil:
		return "deleted - this is the traffic it saw while it existed"
	case f.Shadow:
		return filterModeLabel(*f) +
			" - it runs and enforces nothing, so its refusals below did not happen"
	case !f.UsesModel():
		return filterModeLabel(*f) + ", " + plural(len(f.Rules), "rule") + " applied in the gateway"
	default:
		return filterModeLabel(*f) + " on " + f.Model
	}
}

// noRunsReason says why a filter saw no traffic: nothing attached it, or it
// had a quiet week.
func noRunsReason(res filterReportResponse) string {
	if res.Filter == nil {
		return "No filter of this alias exists in this organisation."
	}
	return "Either no guardrail applies it, or nothing it covers was called. " +
		"'keera guardrail get' on a scope says which filters it applies."
}

// errorConsequence says what a filter that could not run did to the request:
// an enforcing one refused it, a shadow one measured nothing.
func errorConsequence(res filterReportResponse) string {
	if res.Filter != nil && res.Filter.Shadow {
		return "in shadow, each of those requests was forwarded and measured nothing"
	}
	return "each of those requests was refused rather than sent unfiltered"
}
