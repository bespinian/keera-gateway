package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

type filterRun struct {
	*aliasRun

	since       time.Duration
	model       string
	mode        string
	shadow      bool
	noShadow    bool
	prompt      string
	rules       string
	description string
}

func filterCmd(ctx context.Context, args []string) error {
	r := &filterRun{aliasRun: newAliasRun("filter", "filters", "filter", args)}
	fs := r.fs
	durationVar(fs, &r.since, "since", reportWindow, sinceUsage)
	fs.StringVar(&r.model, "model", "", "the model the filter runs on; a gate may run on a decision model")
	fs.StringVar(&r.mode, "mode", "",
		"rewrite (a model edits the request and lets it go), gate (a model judges only "+
			"whether it may go), or pattern (a list of rules is applied, with no model)")
	// Two flags: on 'set', one bool could not tell "leave it" from "off".
	fs.BoolVar(&r.shadow, "shadow", false,
		"run the filter and enforce nothing; what it would have done is recorded")
	fs.BoolVar(&r.noShadow, "no-shadow", false, "act on what the filter answers")
	r.reword("add", "shadow", "start in shadow: run the filter and enforce nothing until "+
		"'set --no-shadow'; what it would have done is recorded")
	fs.StringVar(&r.prompt, "prompt", "", "the instruction its model is given; @path reads a file")
	fs.StringVar(&r.rules, "rules", "",
		"a pattern filter's rules, one per line: an expression, '=>', then what each match "+
			"becomes - or 'REFUSE: why' to drop the request. @path reads a file")
	fs.StringVar(&r.description, "description", "", "what this filter is for, for whoever reads the list")
	r.reword("set", "description", "what this filter is for, for whoever reads the list; an empty one clears it")

	if done, err := r.parse(); done {
		return err
	}
	if r.shadow && r.noShadow {
		return opposites("shadow", "no-shadow")
	}
	if !policy.FilterMode(r.mode).Valid() {
		return fmt.Errorf("--mode is %q; it is 'rewrite' (a model edits the request and lets "+
			"it go), 'gate' (a model judges only whether it may go) or 'pattern' (a list of "+
			"rules is applied, with no model)", r.mode)
	}
	return r.run(ctx, r)
}

func filterAlias(f policy.Filter) string { return f.Alias }

func (r *filterRun) find(ctx context.Context, alias string) (policy.Filter, error) {
	return findAlias(ctx, r.aliasRun, alias, filterAlias)
}

func (r *filterRun) list(ctx context.Context) error {
	return listAliases(ctx, r.aliasRun, printFilters)
}

func (r *filterRun) check(ctx context.Context) error {
	alias := r.fs.Arg(0)
	return checkProbe(ctx, r.c, r.path(alias, "/check"), "the filter "+alias, r.asJSON,
		printFilterProbe, func(p gateway.FilterProbe) bool { return p.OK })
}

func (r *filterRun) report(ctx context.Context) error {
	return reportAlias(ctx, r.aliasRun, r.since, printFilterReport)
}

func (r *filterRun) delete(ctx context.Context) error {
	// Read first, so a typo is said before anybody types the alias back.
	f, err := r.find(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	// The control plane refuses to delete a filter in use, so this warns that
	// the wording is kept nowhere else.
	what := "its instruction is"
	if !f.UsesModel() {
		what = "its rules are"
	}
	return r.deleteEntry(ctx, f.Alias, []string{"  " + what + " not kept anywhere else"})
}

func (r *filterRun) put(ctx context.Context) error {
	alias := r.fs.Arg(0)
	// A new filter enforces, as in the panel and the control API.
	f, err := startPut(ctx, r.aliasRun, alias, policy.Filter{Alias: alias}, filterAlias)
	if err != nil {
		return err
	}
	if err := r.apply(&f); err != nil {
		return err
	}
	return putAlias(ctx, r.aliasRun, f.Alias, map[string]any{
		"model": f.Model, "mode": f.Mode, "shadow": f.Shadow, "prompt": f.Prompt,
		"rules": f.Rules, "description": f.Description,
	}, printFilter)
}

func (r *filterRun) apply(f *policy.Filter) error {
	if r.model != "" {
		f.Model = r.model
	}
	if r.mode != "" {
		f.Mode = policy.FilterMode(r.mode)
	}
	if r.shadow || r.noShadow {
		f.Shadow = r.shadow
	}
	// A filter that changes kind keeps nothing of the other kind, or the
	// endpoint refuses it.
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
	if given(r.fs, "description") {
		f.Description = r.description
	}
	return nil
}

func printFilters(w *table, filters []policy.Filter) {
	w.header("ALIAS\tMODE\tMODEL\tDESCRIPTION\tINSTRUCTION")
	for _, f := range filters {
		model := f.Model
		if !f.UsesModel() {
			model = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			f.Alias, filterModeLabel(f), model, dash(f.Description), filterWhat(f))
	}
}

// filterWhat is the instruction, or for a pattern filter its rule count.
func filterWhat(f policy.Filter) string {
	if f.UsesModel() {
		return firstLine(f.Prompt)
	}
	return plural(len(f.Rules), "rule")
}

// filterModeLabel says whether the filter enforces: a "gate" in shadow does
// not.
func filterModeLabel(f policy.Filter) string {
	if f.Shadow {
		return string(f.Mode) + " (shadow)"
	}
	return string(f.Mode)
}

// filterMode defaults to rewrite.
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
	showIfSet(w, "description", f.Description)
	// Printed whole, the rules in the form --rules reads.
	if !f.UsesModel() {
		printBlock(w, "rules", formatRules(f.Rules))
		return
	}
	printBlock(w, "instruction", strings.Split(f.Prompt, "\n"))
}

func printBlock(w *table, name string, lines []string) {
	_, _ = fmt.Fprintln(w, name+"\t")
	for _, line := range lines {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}

func printFilterProbe(w *table, p gateway.FilterProbe) {
	show(w, "filter", p.Alias)
	show(w, "mode", p.Mode)
	showIfSet(w, "model", p.Model)
	if p.Shadow {
		show(w, "enforcing", "no - this filter is in shadow, so nothing below is acted on")
	}
	show(w, "result", yesNo(p.OK))
	if p.TotalMS > 0 {
		show(w, "took", fmt.Sprintf("%dms", p.TotalMS))
	}
	switch {
	case p.Free:
		// So a zero cost does not read as one that was not measured.
		show(w, "cost of one run", "nothing - no model runs, so there is nothing to charge "+
			"and nothing added to the wait")
	case p.CostMicros > 0:
		show(w, "cost of one run", policy.FormatMoney(p.CostMicros))
	}
	// Which rules fired, so a wrong one can be found.
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
	showIfSet(w, "error", p.Error)
	// A refusal is an answer, not a fault.
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
	// A gate answers once per sample: one meant to be stopped and one not.
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

type filterReportResponse struct {
	Filter       *policy.Filter     `json:"filter"`
	Report       store.FilterReport `json:"report"`
	ProjectNames map[string]string  `json:"project_names"`
}

// printFilterReport answers in the order people ask: is it firing, what does
// it do, what does it cost in time and money, and who gets its refusals.
func printFilterReport(w *table, name string, since time.Duration,
	res filterReportResponse,
) {
	rep := res.Report
	show(w, "filter", name)
	show(w, "state", filterState(res.Filter))
	show(w, "window", "the last "+shortDuration(since))

	if rep.Runs == 0 {
		_, _ = fmt.Fprintf(w, "\nnothing ran in this window. %s\n", noRunsReason(res))
		return
	}

	show(w, "runs", fmt.Sprintf("%d of the organisation's %d requests (%s)",
		rep.Runs, rep.Requests, share(rep.Runs, rep.Requests)))
	// Rates are what an instruction is tuned against.
	show(w, "passed", fmt.Sprintf("%d (%s)", rep.Passed, share(rep.Passed, rep.Runs)))
	if rep.Rewrote > 0 || (res.Filter != nil && res.Filter.Mode.Rewrites()) {
		show(w, "rewrote", fmt.Sprintf("%d (%s), touching %d of %d segments",
			rep.Rewrote, share(rep.Rewrote, rep.Runs), rep.Changed, rep.Segments))
	}
	show(w, "refused", fmt.Sprintf("%d (%s)", rep.Refused, share(rep.Refused, rep.Runs)))
	if rep.Errored > 0 {
		show(w, "could not run", fmt.Sprintf("%d (%s) - %s", rep.Errored,
			share(rep.Errored, rep.Runs), errorConsequence(res)))
	}
	if rep.Shadowed > 0 && rep.Shadowed < rep.Runs {
		show(w, "of those, unenforced", fmt.Sprintf("%d - the filter changed state inside this window",
			rep.Shadowed))
	}
	show(w, "added latency", fmt.Sprintf("%dms median, %dms p95",
		rep.LatencyMedianMS, rep.LatencyP95MS))
	show(w, "spend", fmt.Sprintf("%s of the organisation's %s (%s)",
		policy.FormatMoney(rep.CostMicros), policy.FormatMoney(rep.OrgCostMicros),
		share(rep.CostMicros, rep.OrgCostMicros)))

	if len(rep.Projects) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	w.header("PROJECT\tRUNS\tREFUSED\tRATE\tREWROTE\tFAILED\tSPEND (" + policy.Currency + ")")
	for _, t := range rep.Projects {
		label := t.ProjectID
		if name, ok := res.ProjectNames[t.ProjectID]; ok && name != "" {
			label = name
		}
		if label == "" {
			label = "(no project)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\t%d\t%s\n",
			label, t.Runs, t.Refused, share(t.Refused, t.Runs), t.Rewrote, t.Errored,
			policy.FormatAmount(t.CostMicros))
	}
}

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

// noRunsReason tells unattached apart from merely quiet.
func noRunsReason(res filterReportResponse) string {
	if res.Filter == nil {
		return "No filter of this alias exists in this organisation."
	}
	return "Either no guardrail applies it, or nothing it covers was called. " +
		"'keera guardrail show' on a scope says which filters it applies."
}

// errorConsequence says that an enforcing filter that could not run refused
// the request, and a shadow one measured nothing.
func errorConsequence(res filterReportResponse) string {
	if res.Filter != nil && res.Filter.Shadow {
		return "in shadow, each of those requests was forwarded and measured nothing"
	}
	return "each of those requests was refused rather than sent unfiltered"
}
