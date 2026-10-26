package cli

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// guardrailFlags numbers use -1 for "not given" and 0 to clear; everything
// else is cleared by its --no- flag.
type guardrailFlags struct {
	models           string
	noModels         bool
	requests         int64
	tokens           int64
	per              string
	budget           float64
	period           string
	systemPrompt     string
	noSystemPrompt   bool
	filters          string
	noFilters        bool
	tools            string
	noTools          bool
	maxSandboxes     int
	maxSandboxTTL    time.Duration
	sandboxClasses   string
	noSandboxClasses bool
	maxSandboxCPU    float64
	maxSandboxMemory int
	repos            string
	noRepos          bool
}

func registerGuardrailFlags(fs *flag.FlagSet) *guardrailFlags {
	f := &guardrailFlags{}
	fs.StringVar(&f.models, "models", "", "comma-separated models this scope may use")
	fs.BoolVar(&f.noModels, "no-models", false, "stop this scope limiting the models; an outer level still does")
	fs.Int64Var(&f.requests, "requests", -1, "requests per --per (0 clears it)")
	fs.Int64Var(&f.tokens, "tokens", -1, "tokens per --per (0 clears it)")
	fs.StringVar(&f.per, "per", "",
		"which rate limit to set, by what it counts over: minute, day or month (default minute)")
	fs.Float64Var(&f.budget, "budget", -1, "budget per --period, in CHF (0 clears it)")
	fs.StringVar(&f.period, "period", "",
		"which budget to set, by when it resets: day or month (default month)")
	fs.StringVar(&f.systemPrompt, "system-prompt", "",
		"standing instruction sent ahead of every chat request; @path reads a file")
	fs.BoolVar(&f.noSystemPrompt, "no-system-prompt", false, "remove this scope's system prompt")
	fs.StringVar(&f.filters, "filters", "",
		"comma-separated filters every request through this scope passes through")
	fs.BoolVar(&f.noFilters, "no-filters", false, "stop this scope applying any filter of its own")
	fs.StringVar(&f.tools, "tools", "",
		"comma-separated MCP tools this scope may call: a server, or server/tool")
	fs.BoolVar(&f.noTools, "no-tools", false, "stop this scope limiting the tools; an outer level still does")
	fs.IntVar(&f.maxSandboxes, "max-sandboxes", -1,
		"how many sandboxes this scope may run at once (0 clears it); the sandbox "+
			"flags are for an organisation or a project, not a key")
	durationVar(fs, &f.maxSandboxTTL, "max-sandbox-ttl", -1,
		"longest lifetime any one of them may be given (0 clears it)")
	fs.StringVar(&f.sandboxClasses, "sandbox-classes", "",
		"comma-separated sandbox classes this scope may use")
	fs.BoolVar(&f.noSandboxClasses, "no-sandbox-classes", false,
		"stop this scope limiting the sandbox classes; an outer level still does")
	fs.Float64Var(&f.maxSandboxCPU, "max-sandbox-cpu", -1,
		"largest sandbox this scope may run, in cores (0 clears it)")
	fs.IntVar(&f.maxSandboxMemory, "max-sandbox-memory", -1,
		"largest sandbox this scope may run, in mebibytes (0 clears it)")
	fs.StringVar(&f.repos, "repos", "",
		"comma-separated repositories sandboxes may check out: owner, owner/name, or '*' "+
			"for all; on an organisation, operators only")
	fs.BoolVar(&f.noRepos, "no-repos", false,
		"clear this scope's repositories: on an organisation that allows none, "+
			"on a project it leaves the organisation's list")
	return f
}

// apply leaves fields alone whose flags were not given.
func (f *guardrailFlags) apply(lim *policy.Limits) error {
	if err := setList(&lim.AllowedModels, "models", f.models, f.noModels); err != nil {
		return err
	}
	if err := f.applyRate(lim); err != nil {
		return err
	}
	if err := f.applyBudget(lim); err != nil {
		return err
	}
	// A --no- flag rather than a reserved word, which nobody could then use as
	// a prompt or an alias.
	switch {
	case f.noSystemPrompt && f.systemPrompt != "":
		return opposites("system-prompt", "no-system-prompt")
	case f.noSystemPrompt:
		lim.SystemPrompt = nil
	case f.systemPrompt != "":
		text, err := textOrFile(f.systemPrompt)
		if err != nil {
			return err
		}
		lim.SystemPrompt = &text
	}
	// Clearing this scope's filters does not lift those an outer level applies.
	if err := setList(&lim.Filters, "filters", f.filters, f.noFilters); err != nil {
		return err
	}
	if err := setList(&lim.AllowedTools, "tools", f.tools, f.noTools); err != nil {
		return err
	}
	setInt(&lim.MaxSandboxes, f.maxSandboxes)
	setInt(&lim.MaxSandboxMemory, f.maxSandboxMemory)
	if f.maxSandboxCPU >= 0 {
		setInt(&lim.MaxSandboxCPU, int(f.maxSandboxCPU*1000))
	}
	if f.maxSandboxTTL >= 0 {
		setInt(&lim.MaxSandboxTTLSeconds, int(f.maxSandboxTTL.Seconds()))
	}
	if err := setList(&lim.SandboxClasses, "sandbox-classes", f.sandboxClasses, f.noSandboxClasses); err != nil {
		return err
	}
	return setList(&lim.AllowedRepos, "repos", f.repos, f.noRepos)
}

// setList treats an empty value as "not given".
func setList(dst *[]string, name, v string, none bool) error {
	switch {
	case none && v != "":
		return opposites(name, "no-"+name)
	case none:
		*dst = nil
	case v != "":
		// Never nil, which would read as "no limit".
		*dst = append([]string{}, splitList(v)...)
	}
	return nil
}

// micros rounds, because 2.01 is 2009999.99... as a float.
func micros(units float64) int64 { return int64(math.Round(units * 1_000_000)) }

// applyRate changes the --per period only. A limit left with neither count
// is removed.
func (f *guardrailFlags) applyRate(lim *policy.Limits) error {
	per := policy.PeriodMinute
	if f.per != "" {
		per = policy.Period(f.per)
		if per != policy.PeriodMinute && !per.Valid() {
			return fmt.Errorf("--per must be minute, day or month")
		}
	}
	if f.requests < 0 && f.tokens < 0 {
		if f.per != "" {
			return fmt.Errorf("--per says which rate limit to set; give --requests, --tokens or both")
		}
		return nil
	}
	i := slices.IndexFunc(lim.RateLimits, func(r policy.RateLimit) bool { return r.Per() == per })
	if i < 0 {
		lim.RateLimits = append(lim.RateLimits, policy.RateLimit{Period: per})
		i = len(lim.RateLimits) - 1
	}
	rl := &lim.RateLimits[i]
	if f.requests >= 0 {
		rl.Requests = f.requests
	}
	if f.tokens >= 0 {
		rl.Tokens = f.tokens
	}
	if rl.Requests == 0 && rl.Tokens == 0 {
		lim.RateLimits = slices.Delete(lim.RateLimits, i, i+1)
	}
	return nil
}

// applyBudget changes the --period budget only. A budget of 0 is removed.
func (f *guardrailFlags) applyBudget(lim *policy.Limits) error {
	period := policy.PeriodMonth
	if f.period != "" {
		period = policy.Period(f.period)
		if !period.Valid() {
			return fmt.Errorf("--period must be day or month")
		}
	}
	if f.budget < 0 {
		if f.period != "" {
			return fmt.Errorf("--period says which budget to set; give --budget too")
		}
		return nil
	}
	lim.Budgets = slices.DeleteFunc(lim.Budgets, func(b policy.Budget) bool { return b.Per() == period })
	if m := micros(f.budget); m > 0 {
		lim.Budgets = append(lim.Budgets, policy.Budget{BudgetMicros: m, Period: period})
	}
	return nil
}

func guardrailCmd(ctx context.Context, args []string) error {
	r := newCmdRun("guardrail", args)
	f := registerGuardrailFlags(r.fs)
	if done, err := r.parse(); done {
		return err
	}
	c := r.c
	scope, scopeID, err := scopeArgs(ctx, c, r.fs, r.org)
	if err != nil {
		return err
	}

	switch r.verb {
	case "effective":
		eff, err := effectiveFor(ctx, c, scope, scopeID)
		if err != nil {
			return err
		}
		return out(r.asJSON, eff, func(w *table) { printEffective(w, eff, nil) })
	case "set":
		if !changesSomething(r.fs) {
			return nothingToChange("guardrail set")
		}
		lim, err := updateLimits(ctx, c, scope, scopeID, f.apply)
		if err != nil {
			return err
		}
		return out(r.asJSON, lim, func(w *table) { printLimits(w, lim, scope) })
	default:
		return doOut(ctx, c, "GET", guardrailPath(scope, scopeID), nil, r.asJSON,
			func(w *table, lim policy.Limits) { printLimits(w, lim, scope) })
	}
}

// scopeArgs accepts names for projects and keys. An organisation's id may be
// left out: it is then org, the caller's own, or an operator's only one.
func scopeArgs(ctx context.Context, c *client, fs *flag.FlagSet, org string) (string, string, error) {
	scope, given := fs.Arg(0), fs.Arg(1)
	switch policy.ScopeType(scope) {
	case policy.ScopeOrg:
		if given == "" {
			given = org
		}
		orgID, err := resolveOrg(ctx, c, given)
		return scope, orgID, err
	case policy.ScopeProject, policy.ScopeKey:
		if given == "" {
			return "", "", fmt.Errorf("name the %s after the scope: %s <%s>", scope, scope, scope)
		}
		find := projectID
		if scope == string(policy.ScopeKey) {
			find = keyID
		}
		scopeID, err := find(ctx, c, org, given)
		return scope, scopeID, err
	default:
		return "", "", fmt.Errorf("the scope is org, project or key, not %q", scope)
	}
}

// updateLimits reads before writing, so a field nobody named is kept.
func updateLimits(ctx context.Context, c *client, scope, scopeID string,
	change func(*policy.Limits) error,
) (policy.Limits, error) {
	var lim policy.Limits
	path := guardrailPath(scope, scopeID)
	if err := c.do(ctx, "GET", path, nil, &lim); err != nil {
		return lim, err
	}
	if err := change(&lim); err != nil {
		return lim, err
	}
	err := c.do(ctx, "PUT", path, lim, &lim)
	return lim, err
}

// A facet is 'guardrail set' with only some flags: 'keera limit' for the
// rates and 'keera budget' for the spend. With no flags it reports, because
// `keera limit project payments` is a question.

var facetFields = map[string][]string{"limit": {"rates"}, "budget": {"spend"}}

func facetCmd(ctx context.Context, name string, args []string) error {
	r := newCmdRun(name, args)
	f := registerGuardrailFlags(r.fs)
	// The other facet's flags are refused before parse, which says it less
	// helpfully.
	if want, ok := wantsHelp(args); ok {
		return printHelp(r.fs, name, want)
	}
	if err := wrongFacet(name, args); err != nil {
		return err
	}
	if done, err := r.parse(); done {
		return err
	}
	c := r.c
	scope, scopeID, err := scopeArgs(ctx, c, r.fs, r.org)
	if err != nil {
		return err
	}

	if changesSomething(r.fs) {
		if _, err := updateLimits(ctx, c, scope, scopeID, f.apply); err != nil {
			return err
		}
	}
	// Only this facet's part, even after a change.
	eff, err := effectiveFor(ctx, c, scope, scopeID)
	if err != nil {
		return err
	}
	return out(r.asJSON, eff, func(w *table) { printEffective(w, eff, facetFields[name]) })
}

func wrongFacet(name string, args []string) error {
	own, _ := find(name)
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flagName, _, _ := strings.Cut(arg, "=")
		flagName = strings.TrimLeft(flagName, "-")
		if slices.Contains(own.flags, flagName) {
			continue
		}
		for _, other := range []string{"limit", "budget"} {
			c, _ := find(other)
			if other != name && slices.Contains(c.flags, flagName) {
				return fmt.Errorf("--%s is not part of 'keera %s'; it is 'keera %s --%s'",
					flagName, name, other, flagName)
			}
		}
	}
	return nil
}

// guardrailPath escapes both parts: a slash or a space would land on another
// route.
func guardrailPath(scope, scopeID string) string {
	return "/v1/guardrails/" + url.PathEscape(scope) + "/" + url.PathEscape(scopeID)
}

func effectiveFor(ctx context.Context, c *client, scope, scopeID string) (control.Effective, error) {
	var eff control.Effective
	err := c.do(ctx, "GET", guardrailPath(scope, scopeID)+"/effective", nil, &eff)
	return eff, err
}

var effectiveParts = []struct {
	name  string
	print func(*table, control.Effective)
}{
	{"models", printEffectiveModels},
	{"rates", printEffectiveRates},
	{"spend", printEffectiveSpend},
	{"prompt", printEffectivePrompt},
	{"filters", printEffectiveFilters},
	{"tools", printEffectiveTools},
	{"sandboxes", printEffectiveSandboxes},
}

// printEffective names the level behind every row, so nobody changes the
// number on the wrong scope. A nil fields prints every part.
func printEffective(w *table, eff control.Effective, fields []string) {
	w.header("WHAT\tIN FORCE\tFROM")
	for _, part := range effectiveParts {
		if fields == nil || slices.Contains(fields, part.name) {
			part.print(w, eff)
		}
	}
}

func showFrom(w *table, name string, v any, from string) {
	if from == "" {
		_, _ = fmt.Fprintf(w, "%s\t%v\t\n", name, v)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\t%v\tset by %s\n", name, v, from)
}

func showSet(w *table, name, unset string, set bool, v any, from string) {
	if !set {
		v, from = unset, ""
	}
	showFrom(w, name, v, from)
}

func printEffectiveModels(w *table, eff control.Effective) {
	showSet(w, "models", "(every enabled model)", eff.AllowedModels != nil,
		strings.Join(eff.AllowedModels, ","),
		narrowedBy(eff, func(l policy.Limits) bool { return l.AllowedModels != nil }))
}

// Rate limits and budgets do not merge, since every one must hold, so each is
// printed with its level.
func printEffectiveRates(w *table, eff control.Effective) {
	printEveryLevel(w, eff, "rate limit", ratesOf)
}

func printEffectiveSpend(w *table, eff control.Effective) {
	printEveryLevel(w, eff, "budget", budgetsOf)
}

func printEveryLevel(w *table, eff control.Effective, name string, says func(policy.Limits) []string) {
	limited := false
	for _, l := range eff.Levels {
		for _, v := range says(l.Limits) {
			limited = true
			showFrom(w, name, v, levelLabel(l))
		}
	}
	if !limited {
		showFrom(w, name, "(unlimited)", "")
	}
}

func ratesOf(lim policy.Limits) []string {
	var out []string
	for _, rl := range lim.RateLimits {
		if rl.Requests > 0 || rl.Tokens > 0 {
			out = append(out, rateText(rl))
		}
	}
	return out
}

func rateText(rl policy.RateLimit) string {
	var parts []string
	if rl.Requests > 0 {
		parts = append(parts, fmt.Sprintf("%d requests", rl.Requests))
	}
	if rl.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tokens", rl.Tokens))
	}
	return strings.Join(parts, " and ") + " per " + string(rl.Per())
}

func budgetText(b policy.Budget) string {
	return policy.FormatMoney(b.BudgetMicros) + " per " + string(b.Per())
}

func budgetsOf(lim policy.Limits) []string {
	var out []string
	for _, b := range lim.Budgets {
		out = append(out, budgetText(b))
	}
	return out
}

func printEffectivePrompt(w *table, eff control.Effective) {
	showSet(w, "system prompt", "(none)", eff.SystemPrompt != "",
		firstLine(eff.SystemPrompt),
		joinedFrom(eff, func(l policy.Limits) bool { return l.SystemPrompt != nil }))
}

func printEffectiveFilters(w *table, eff control.Effective) {
	showSet(w, "filters", "(none)", len(eff.Filters) > 0,
		strings.Join(eff.Filters, ","),
		joinedFrom(eff, func(l policy.Limits) bool { return len(l.Filters) > 0 }))
}

func printEffectiveTools(w *table, eff control.Effective) {
	showSet(w, "mcp tools", "(every tool of every MCP server)", eff.AllowedTools != nil,
		joinedOr(eff.AllowedTools, "(none)"),
		narrowedBy(eff, func(l policy.Limits) bool { return l.AllowedTools != nil }))
}

func printEffectiveSandboxes(w *table, eff control.Effective) {
	sb := eff.Sandbox
	showSet(w, "sandbox classes", "(every class)", sb.SandboxClasses != nil,
		strings.Join(sb.SandboxClasses, ","),
		narrowedBy(eff, func(l policy.Limits) bool { return l.SandboxClasses != nil }))
	showSet(w, "repositories", "(none)", sb.AllowedRepos != nil,
		strings.Join(sb.AllowedRepos, ","),
		narrowedBy(eff, func(l policy.Limits) bool { return l.AllowedRepos != nil }))
	showSet(w, "max sandboxes", "(unlimited)", sb.MaxSandboxes != 0,
		sb.MaxSandboxes,
		narrowedBy(eff, func(l policy.Limits) bool { return l.MaxSandboxes != nil }))
	if sb.MaxSandboxTTLSeconds > 0 {
		showFrom(w, "max sandbox lifetime",
			shortDuration(time.Duration(sb.MaxSandboxTTLSeconds)*time.Second),
			narrowedBy(eff, func(l policy.Limits) bool { return l.MaxSandboxTTLSeconds != nil }))
	}
	showSet(w, "max sandbox size", "(unlimited)", sb.MaxSandboxCPU != 0 || sb.MaxSandboxMemory != 0,
		maxSize(sb.MaxSandboxCPU, sb.MaxSandboxMemory),
		narrowedBy(eff, func(l policy.Limits) bool {
			return l.MaxSandboxCPU != nil || l.MaxSandboxMemory != nil
		}))
}

// narrowedBy names the innermost level that set a field, which decided it.
func narrowedBy(eff control.Effective, set func(policy.Limits) bool) string {
	from := ""
	for _, l := range eff.Levels {
		if set(l.Limits) {
			from = levelLabel(l)
		}
	}
	return from
}

// joinedFrom is for fields that add up rather than narrow.
func joinedFrom(eff control.Effective, set func(policy.Limits) bool) string {
	var names []string
	for _, l := range eff.Levels {
		if set(l.Limits) {
			names = append(names, levelLabel(l))
		}
	}
	return strings.Join(names, " then ")
}

func levelLabel(l control.EffectiveLevel) string {
	if l.Name == "" {
		return string(l.Type) + " " + l.ID
	}
	return string(l.Type) + " " + l.Name
}

func showEach(w *table, name string, vs []string) {
	if len(vs) == 0 {
		vs = []string{notSet}
	}
	for _, v := range vs {
		show(w, name, v)
	}
}

// notSet is not "unlimited", because an outer level may still set the field.
const notSet = "(not set here)"

// printLimits shows one scope's own guardrail; 'guardrail effective' shows
// what applies.
func printLimits(w *table, lim policy.Limits, scope string) {
	show(w, "models", joinedOr(lim.AllowedModels, notSet))
	showEach(w, "rate limit", ratesOf(lim))
	showEach(w, "budget", budgetsOf(lim))
	if len(lim.Filters) == 0 {
		show(w, "filters", "(none)")
	} else {
		show(w, "filters", strings.Join(lim.Filters, ","))
	}
	show(w, "mcp tools", joinedOr(lim.AllowedTools, notSet))
	if lim.SystemPrompt == nil {
		show(w, "system prompt", "(none)")
	} else {
		// --json has the whole text.
		show(w, "system prompt", firstLine(*lim.SystemPrompt))
	}
	show(w, "sandbox classes", joinedOr(lim.SandboxClasses, notSet))
	// Only the organisation grants repositories, so there unset means none.
	noRepos := notSet
	if scope == string(policy.ScopeOrg) {
		noRepos = "(none)"
	}
	show(w, "repositories", joinedOr(lim.AllowedRepos, noRepos))
	show(w, "max sandboxes", notSetHere(lim.MaxSandboxes))
	if lim.MaxSandboxTTLSeconds == nil {
		show(w, "max sandbox lifetime", notSet)
	} else {
		show(w, "max sandbox lifetime",
			shortDuration(time.Duration(*lim.MaxSandboxTTLSeconds)*time.Second))
	}
	if lim.MaxSandboxCPU == nil && lim.MaxSandboxMemory == nil {
		show(w, "max sandbox size", notSet)
	} else {
		show(w, "max sandbox size", maxSize(derefInt(lim.MaxSandboxCPU), derefInt(lim.MaxSandboxMemory)))
	}
}

func notSetHere(p *int) any {
	if p == nil {
		return notSet
	}
	return *p
}

// maxSize uses cores and mebibytes, as the template file and the flags do.
func maxSize(cpuMillis, memoryMiB int) string {
	cpu, memory := "any CPU", "any memory"
	if cpuMillis > 0 {
		cpu = coresOf(cpuMillis)
	}
	if memoryMiB > 0 {
		memory = mibOf(memoryMiB)
	}
	return cpu + ", " + memory
}

func joinedOr(items []string, none string) string {
	if items == nil {
		return none
	}
	return strings.Join(items, ",")
}
