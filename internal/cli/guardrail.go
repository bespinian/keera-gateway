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

// Flag descriptions that 'guardrail set', 'limit' and 'budget' share.
const (
	rpmUsage    = "requests per minute (0 clears it)"
	tpmUsage    = "tokens per minute (0 clears it)"
	maxOutUsage = "cap on generated tokens per request (0 clears it)"
	budgetUsage = "budget per period, in CHF (0 clears it)"
	periodUsage = "budget period: day or month"
)

// guardrailFlags are the flags of 'keera guardrail'. Numbers use -1 for "not
// given" and 0 to clear the field; everything else is cleared by its --no- flag.
type guardrailFlags struct {
	models           string
	noModels         bool
	rpm              int
	tpm              int
	maxOut           int
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
	fs.IntVar(&f.rpm, "rpm", -1, rpmUsage)
	fs.IntVar(&f.tpm, "tpm", -1, tpmUsage)
	fs.IntVar(&f.maxOut, "max-output-tokens", -1, maxOutUsage)
	fs.Float64Var(&f.budget, "budget", -1, budgetUsage)
	fs.StringVar(&f.period, "period", "", periodUsage)
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
	fs.DurationVar(&f.maxSandboxTTL, "max-sandbox-ttl", -1,
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

// apply writes the flags that were given into lim and leaves the rest alone.
func (f *guardrailFlags) apply(lim *policy.Limits) error {
	if err := setList(&lim.AllowedModels, "models", f.models, f.noModels); err != nil {
		return err
	}
	setInt(&lim.RPM, f.rpm)
	setInt(&lim.TPM, f.tpm)
	setInt(&lim.MaxOutputTokens, f.maxOut)
	setBudget(&lim.BudgetMicros, f.budget)
	if err := setPeriod(&lim.BudgetPeriod, f.period); err != nil {
		return err
	}
	// Each part is cleared by its own flag rather than by a reserved word,
	// which nobody could then use as a prompt or an alias.
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
	switch {
	case f.noFilters && f.filters != "":
		return opposites("filters", "no-filters")
	case f.noFilters:
		lim.Filters = nil
	case f.filters != "":
		lim.Filters = splitList(f.filters)
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

// setList applies a comma-separated list flag and its --no- flag, which
// clears the list. An empty value means "not given".
func setList(dst *[]string, name, v string, clear bool) error {
	switch {
	case clear && v != "":
		return opposites(name, "no-"+name)
	case clear:
		*dst = nil
	case v != "":
		// Never nil, which would read as "no limit": a list of only commas
		// names nothing.
		*dst = append([]string{}, splitList(v)...)
	}
	return nil
}

// micros converts francs to millionths. It rounds, because
// 2.01 is 2009999.99... as a float and truncating it would lose a millionth.
func micros(units float64) int64 { return int64(math.Round(units * 1_000_000)) }

// setBudget sets a budget given in CHF; 0 removes it.
func setBudget(dst **int64, amount float64) {
	// Negative is the flag's default, for not given, as in setInt.
	if amount < 0 {
		return
	}
	m := micros(amount)
	if m == 0 {
		*dst = nil
		return
	}
	*dst = &m
}

// setPeriod applies --period, which may be left empty.
func setPeriod(dst **policy.Period, given string) error {
	if given == "" {
		return nil
	}
	p := policy.Period(given)
	if !p.Valid() {
		return fmt.Errorf("--period must be day or month")
	}
	*dst = &p
	return nil
}

func guardrailCmd(ctx context.Context, args []string) error {
	r := newCmdRun("guardrail", args, "json")
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
		return out(r.asJSON, eff, func(w *table) { printEffective(w, eff, facetAll) })
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
		var lim policy.Limits
		if err := c.do(ctx, "GET", guardrailPath(scope, scopeID), nil, &lim); err != nil {
			return err
		}
		return out(r.asJSON, lim, func(w *table) { printLimits(w, lim, scope) })
	}
}

// scopeArgs reads the scope and its id from the arguments already parsed. A
// project may be named by its name and a key by its name, as everywhere else,
// inside the organisation org. The id of an organisation may be left out: it
// is then org, the caller's own, or an operator's only one.
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

// updateLimits reads a scope's guardrails, changes them and writes them back,
// so that a field nobody named is kept rather than cleared.
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

// A facet is one part of a guardrail on its own: 'keera limit' for the rates
// and 'keera budget' for the spend. Same scopes, same call and the same flags
// as 'guardrail set', of which help.go lets each take its own. With no flags
// it reports rather than writes, because `keera limit project project_1` is a question.

// facetAll is every part of the effective report, in the order it prints.
var facetAll = []string{"models", "rates", "spend", "prompt", "filters", "tools", "sandboxes"}

// facetFields are the parts of the effective report each facet prints.
var facetFields = map[string][]string{"limit": {"rates"}, "budget": {"spend"}}

func facetCmd(ctx context.Context, name string, args []string) error {
	r := newCmdRun(name, args, "json")
	f := registerGuardrailFlags(r.fs)
	// Help comes first, then the other facet's flags, named by the command
	// that takes them, before parse refuses them less helpfully.
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
	// Print only this facet's part, even after a change: 'budget' answering
	// with every sandbox ceiling would be saying more than was asked.
	eff, err := effectiveFor(ctx, c, scope, scopeID)
	if err != nil {
		return err
	}
	return out(r.asJSON, eff, func(w *table) { printEffective(w, eff, facetFields[name]) })
}

// wrongFacet refuses a flag that belongs to the other facet, naming the
// command that takes it.
func wrongFacet(name string, args []string) error {
	own, _ := find(name)
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flagName := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
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

// guardrailPath addresses one scope's guardrails. Both parts are typed by
// people, so both are escaped: a slash or a space would land on another route.
func guardrailPath(scope, scopeID string) string {
	return "/v1/guardrails/" + url.PathEscape(scope) + "/" + url.PathEscape(scopeID)
}

// effectiveFor reads the whole chain above a scope.
func effectiveFor(ctx context.Context, c *client, scope, scopeID string) (control.Effective, error) {
	var eff control.Effective
	err := c.do(ctx, "GET", guardrailPath(scope, scopeID)+"/effective", nil, &eff)
	return eff, err
}

// printEffective says what actually holds a scope. Every row names the level
// that decided it, so nobody changes the number on the wrong scope.
func printEffective(w *table, eff control.Effective, fields []string) {
	want := func(f string) bool { return slices.Contains(fields, f) }
	w.header("WHAT\tIN FORCE\tFROM")
	if want("models") {
		printEffectiveModels(w, eff)
	}
	if want("rates") {
		printEffectiveRates(w, eff)
	}
	if want("spend") {
		printEffectiveSpend(w, eff)
	}
	if want("prompt") {
		printEffectivePrompt(w, eff)
	}
	if want("filters") {
		printEffectiveFilters(w, eff)
	}
	if want("tools") {
		printEffectiveTools(w, eff)
	}
	if want("sandboxes") {
		printEffectiveSandboxes(w, eff)
	}
}

func printEffectiveTools(w *table, eff control.Effective) {
	if eff.AllowedTools == nil {
		showFrom(w, "mcp tools", "(every tool of every MCP server)", "")
	} else {
		showFrom(w, "mcp tools", joinedOr(eff.AllowedTools, "(none)"),
			narrowedBy(eff, func(l policy.Limits) bool { return l.AllowedTools != nil }))
	}
}

// showFrom writes one row of the effective report.
func showFrom(w *table, name string, v any, from string) {
	if from == "" {
		_, _ = fmt.Fprintf(w, "%s\t%v\t\n", name, v)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\t%v\tset by %s\n", name, v, from)
}

func printEffectiveModels(w *table, eff control.Effective) {
	if eff.AllowedModels == nil {
		showFrom(w, "models", "(every enabled model)", "")
		return
	}
	showFrom(w, "models", strings.Join(eff.AllowedModels, ","),
		narrowedBy(eff, func(l policy.Limits) bool { return l.AllowedModels != nil }))
}

func printEffectiveRates(w *table, eff control.Effective) {
	// Rate limits are enforced at every level, so the tightest one binds.
	showTightest(w, eff, "requests/min", func(s policy.Scope) int { return s.RPM })
	showTightest(w, eff, "tokens/min", func(s policy.Scope) int { return s.TPM })
	if eff.MaxOutputTokens == 0 {
		showFrom(w, "max output tokens", "(unlimited)", "")
		return
	}
	showFrom(w, "max output tokens", eff.MaxOutputTokens,
		narrowedBy(eff, func(l policy.Limits) bool { return l.MaxOutputTokens != nil }))
}

func printEffectiveSpend(w *table, eff control.Effective) {
	// Budgets do not merge: every level's must hold, so every one is printed.
	limited := false
	for _, s := range eff.Scopes {
		if s.BudgetMicros == 0 {
			continue
		}
		limited = true
		showFrom(w, "budget", policy.FormatMicros(s.BudgetMicros)+" per "+string(s.Period),
			levelName(eff, string(s.Type), s.ID))
	}
	if !limited {
		showFrom(w, "budget", "(unlimited)", "")
	}
}

func printEffectivePrompt(w *table, eff control.Effective) {
	if eff.SystemPrompt == "" {
		showFrom(w, "system prompt", "(none)", "")
		return
	}
	showFrom(w, "system prompt", firstLine(eff.SystemPrompt),
		joinedFrom(eff, func(l policy.Limits) bool { return l.SystemPrompt != nil }))
}

func printEffectiveFilters(w *table, eff control.Effective) {
	if len(eff.Filters) == 0 {
		showFrom(w, "filters", "(none)", "")
		return
	}
	showFrom(w, "filters", strings.Join(eff.Filters, ","),
		joinedFrom(eff, func(l policy.Limits) bool { return len(l.Filters) > 0 }))
}

func printEffectiveSandboxes(w *table, eff control.Effective) {
	sb := eff.Sandbox
	if sb.SandboxClasses == nil {
		showFrom(w, "sandbox classes", "(every class)", "")
	} else {
		showFrom(w, "sandbox classes", strings.Join(sb.SandboxClasses, ","),
			narrowedBy(eff, func(l policy.Limits) bool { return l.SandboxClasses != nil }))
	}
	if sb.AllowedRepos == nil {
		showFrom(w, "repositories", "(none)", "")
	} else {
		showFrom(w, "repositories", strings.Join(sb.AllowedRepos, ","),
			narrowedBy(eff, func(l policy.Limits) bool { return l.AllowedRepos != nil }))
	}
	if sb.MaxSandboxes == 0 {
		showFrom(w, "max sandboxes", "(unlimited)", "")
	} else {
		showFrom(w, "max sandboxes", sb.MaxSandboxes,
			narrowedBy(eff, func(l policy.Limits) bool { return l.MaxSandboxes != nil }))
	}
	if sb.MaxSandboxTTLSeconds > 0 {
		showFrom(w, "max sandbox lifetime",
			shortDuration(time.Duration(sb.MaxSandboxTTLSeconds)*time.Second),
			narrowedBy(eff, func(l policy.Limits) bool { return l.MaxSandboxTTLSeconds != nil }))
	}
	if sb.MaxSandboxCPU == 0 && sb.MaxSandboxMemory == 0 {
		showFrom(w, "max sandbox size", "(unlimited)", "")
	} else {
		showFrom(w, "max sandbox size", maxSize(sb.MaxSandboxCPU, sb.MaxSandboxMemory),
			narrowedBy(eff, func(l policy.Limits) bool {
				return l.MaxSandboxCPU != nil || l.MaxSandboxMemory != nil
			}))
	}
}

func showTightest(w *table, eff control.Effective, name string,
	of func(policy.Scope) int,
) {
	tightest, from := 0, ""
	for _, s := range eff.Scopes {
		v := of(s)
		if v > 0 && (tightest == 0 || v < tightest) {
			tightest, from = v, levelName(eff, string(s.Type), s.ID)
		}
	}
	if tightest == 0 {
		showFrom(w, name, "(unlimited)", "")
		return
	}
	showFrom(w, name, tightest, from)
}

// narrowedBy names the innermost level that set a field. For a field that
// narrows, that level decided it.
func narrowedBy(eff control.Effective, set func(policy.Limits) bool) string {
	from := ""
	for _, l := range eff.Levels {
		if set(l.Limits) {
			from = levelLabel(l)
		}
	}
	return from
}

// joinedFrom names every level that contributed, for the fields that add up
// rather than narrow.
func joinedFrom(eff control.Effective, set func(policy.Limits) bool) string {
	var names []string
	for _, l := range eff.Levels {
		if set(l.Limits) {
			names = append(names, levelLabel(l))
		}
	}
	return strings.Join(names, " then ")
}

func levelName(eff control.Effective, scopeType, scopeID string) string {
	for _, l := range eff.Levels {
		if string(l.Type) == scopeType && l.ID == scopeID {
			return levelLabel(l)
		}
	}
	return scopeType
}

// levelLabel names a level by its name, or by its id when it has none.
func levelLabel(l control.EffectiveLevel) string {
	if l.Name == "" {
		return string(l.Type) + " " + l.ID
	}
	return string(l.Type) + " " + l.Name
}

// notSet is what a scope's own guardrail shows for a field it leaves to the
// levels around it. "unlimited" would be wrong: an outer level may still set it.
const notSet = "(not set here)"

// printLimits shows one scope's own guardrail, not what applies there; that is
// 'guardrail effective'.
func printLimits(w *table, lim policy.Limits, scope string) {
	show(w, "models", joinedOr(lim.AllowedModels, notSet))
	show(w, "max output tokens", notSetHere(lim.MaxOutputTokens))
	show(w, "requests/min", notSetHere(lim.RPM))
	show(w, "tokens/min", notSetHere(lim.TPM))
	if lim.BudgetMicros == nil {
		show(w, "budget", notSet)
	} else {
		p := policy.PeriodMonth
		if lim.BudgetPeriod != nil {
			p = *lim.BudgetPeriod
		}
		show(w, "budget", policy.FormatMicros(*lim.BudgetMicros)+" per "+string(p))
	}
	if len(lim.Filters) == 0 {
		show(w, "filters", "(none)")
	} else {
		show(w, "filters", strings.Join(lim.Filters, ","))
	}
	show(w, "mcp tools", joinedOr(lim.AllowedTools, notSet))
	if lim.SystemPrompt == nil {
		show(w, "system prompt", "(none)")
	} else {
		// One line; --json has the whole text.
		show(w, "system prompt", firstLine(*lim.SystemPrompt))
	}
	show(w, "sandbox classes", joinedOr(lim.SandboxClasses, notSet))
	// Only the organisation grants repositories, so there nothing set is none.
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

// maxSize is a ceiling on a sandbox's size, in cores and mebibytes, the units
// the catalogue file and the flags use. Either part may be unset.
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

// joinedOr joins a list for a table cell, or says what a nil list means.
func joinedOr(items []string, none string) string {
	if items == nil {
		return none
	}
	return strings.Join(items, ",")
}
