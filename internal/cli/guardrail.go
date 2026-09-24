package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/control"
	"github.com/bespinian/keera-gateway/internal/policy"
)

// Flag descriptions that 'guardrail set', 'limit' and 'budget' share.
const (
	rpmUsage    = "requests per minute (0 for unlimited)"
	tpmUsage    = "tokens per minute (0 for unlimited)"
	maxOutUsage = "cap on generated tokens per request (0 for unlimited)"
	budgetUsage = "budget per period, in whole currency units (0 for unlimited)"
	periodUsage = "budget period: day or month"
)

// guardrailFlags are the flags of 'keera guardrail'. Numbers use -1 for "not
// given" and 0 for "unlimited"; lists take 'any' to clear them.
type guardrailFlags struct {
	models           string
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
	blockHosted      bool
	allowHosted      bool
	maxSandboxes     int
	maxSandboxTTL    time.Duration
	sandboxClasses   string
	maxSandboxCPU    float64
	maxSandboxMemory int
	asJSON           bool
}

func registerGuardrailFlags(fs *flag.FlagSet) *guardrailFlags {
	f := &guardrailFlags{}
	fs.StringVar(&f.models, "models", "", "comma-separated models this scope may use ('any' to clear)")
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
		"comma-separated MCP tools this scope may call: a server, or server/tool ('any' to clear)")
	fs.BoolVar(&f.blockHosted, "block-hosted-tools", false,
		"take the tools a hosted provider runs itself, such as web search, out of every request")
	fs.BoolVar(&f.allowHosted, "allow-hosted-tools", false,
		"stop this scope blocking hosted tools; an outer level that blocks them still does")
	fs.IntVar(&f.maxSandboxes, "max-sandboxes", -1,
		"how many sandboxes this scope may run at once (0 for unlimited)")
	fs.DurationVar(&f.maxSandboxTTL, "max-sandbox-ttl", -1,
		"longest lifetime any one of them may be given (0 for unlimited)")
	fs.StringVar(&f.sandboxClasses, "sandbox-classes", "",
		"comma-separated sandbox classes this scope may use ('any' to clear)")
	fs.Float64Var(&f.maxSandboxCPU, "max-sandbox-cpu", -1,
		"largest sandbox this scope may run, in cores (0 for unlimited)")
	fs.IntVar(&f.maxSandboxMemory, "max-sandbox-memory", -1,
		"largest sandbox this scope may run, in mebibytes (0 for unlimited)")
	fs.BoolVar(&f.asJSON, "json", false, jsonUsage)
	return f
}

// apply writes the flags that were given into lim and leaves the rest alone.
func (f *guardrailFlags) apply(lim *policy.Limits) error {
	setList(&lim.AllowedModels, f.models)
	setInt(&lim.RPM, f.rpm)
	setInt(&lim.TPM, f.tpm)
	setInt(&lim.MaxOutputTokens, f.maxOut)
	if f.budget >= 0 {
		setBudget(&lim.BudgetMicros, f.budget)
	}
	if err := setPeriod(&lim.BudgetPeriod, f.period); err != nil {
		return err
	}
	// The prompt and the filters are cleared by their own flags rather than
	// by a reserved word, which nobody could then use as a prompt or an alias.
	switch {
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
	case f.noFilters:
		lim.Filters = nil
	case f.filters != "":
		lim.Filters = strings.Split(f.filters, ",")
	}
	setList(&lim.AllowedTools, f.tools)
	switch {
	case f.blockHosted && f.allowHosted:
		return fmt.Errorf("--block-hosted-tools and --allow-hosted-tools contradict each other")
	case f.blockHosted:
		yes := true
		lim.BlockHostedTools = &yes
	case f.allowHosted:
		lim.BlockHostedTools = nil
	}
	setInt(&lim.MaxSandboxes, f.maxSandboxes)
	setInt(&lim.MaxSandboxMemory, f.maxSandboxMemory)
	if f.maxSandboxCPU >= 0 {
		setInt(&lim.MaxSandboxCPU, int(f.maxSandboxCPU*1000))
	}
	if f.maxSandboxTTL >= 0 {
		setInt(&lim.MaxSandboxTTLSeconds, int(f.maxSandboxTTL.Seconds()))
	}
	setList(&lim.SandboxClasses, f.sandboxClasses)
	return nil
}

// setList applies a comma-separated list flag, where 'any' clears the list
// and an empty value means "not given".
func setList(dst *[]string, v string) {
	switch v {
	case "":
	case "any":
		*dst = nil
	default:
		*dst = strings.Split(v, ",")
	}
}

// setBudget sets a budget given in whole currency units; 0 removes it.
func setBudget(dst **int64, amount float64) {
	micros := int64(amount * 1_000_000)
	if micros == 0 {
		*dst = nil
		return
	}
	*dst = &micros
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
	sub, rest := split(args)
	c := newClient()
	fs := flag.NewFlagSet("guardrail "+sub, flag.ExitOnError)
	f := registerGuardrailFlags(fs)
	fs.Usage = func() { _ = printHelp(fs, "guardrail", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "guardrail", want)
	}

	switch sub {
	case "get", "show":
		if err := parseArgs(fs, rest, 2, "usage: keera guardrail get <org|team|key> <id>"); err != nil {
			return err
		}
		var lim policy.Limits
		if err := c.do(ctx, "GET", guardrailPath(fs.Arg(0), fs.Arg(1)), nil, &lim); err != nil {
			return err
		}
		return out(f.asJSON, lim, func(w *table) { printLimits(w, lim) })
	case "effective":
		if err := parseArgs(fs, rest, 2, "usage: keera guardrail effective <org|team|key> <id>"); err != nil {
			return err
		}
		eff, err := effectiveFor(ctx, c, fs.Arg(0), fs.Arg(1))
		if err != nil {
			return err
		}
		return out(f.asJSON, eff, func(w *table) { printEffective(w, eff, facetAll) })
	case "set", "edit", "update":
		if err := parseArgs(fs, rest, 2, "usage: keera guardrail set <org|team|key> <id> [flags]"); err != nil {
			return err
		}
		lim, err := updateLimits(ctx, c, fs.Arg(0), fs.Arg(1), f.apply)
		if err != nil {
			return err
		}
		return out(f.asJSON, lim, func(w *table) { printLimits(w, lim) })
	default:
		return unknownSub("guardrail", sub)
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
// and 'keera budget' for the spend. Same scopes and same call, fewer flags.
// With no flags it reports rather than writes, because `keera limit team t_1`
// is a question.

// facetAll is every part of the effective report, in the order it prints.
var facetAll = []string{"models", "rates", "spend", "prompt", "filters", "tools", "sandboxes"}

// facetFields are the parts of the effective report each facet prints.
var facetFields = map[string][]string{"limit": {"rates"}, "budget": {"spend"}}

// facetFlags are the flags of 'keera limit' or 'keera budget'.
type facetFlags struct {
	budget float64
	period string
	rpm    int
	tpm    int
	maxOut int
	asJSON bool
}

// registerFacetFlags declares the part of a guardrail one facet owns, in the
// same words 'guardrail set' uses.
func registerFacetFlags(fs *flag.FlagSet, name string) *facetFlags {
	f := &facetFlags{}
	switch name {
	case "budget":
		fs.Float64Var(&f.budget, "budget", -1, budgetUsage)
		fs.StringVar(&f.period, "period", "", periodUsage)
	default:
		fs.IntVar(&f.rpm, "rpm", -1, rpmUsage)
		fs.IntVar(&f.tpm, "tpm", -1, tpmUsage)
		fs.IntVar(&f.maxOut, "max-output-tokens", -1, maxOutUsage)
	}
	fs.BoolVar(&f.asJSON, "json", false, jsonUsage)
	return f
}

// apply writes the given flags of one facet into lim.
func (f *facetFlags) apply(name string, lim *policy.Limits) error {
	if name == "budget" {
		if f.budget != -1 {
			setBudget(&lim.BudgetMicros, f.budget)
		}
		return setPeriod(&lim.BudgetPeriod, f.period)
	}
	setInt(&lim.RPM, f.rpm)
	setInt(&lim.TPM, f.tpm)
	setInt(&lim.MaxOutputTokens, f.maxOut)
	return nil
}

func facetCmd(ctx context.Context, name string, args []string) error {
	if want, ok := wantsHelp(args); ok {
		fs := flag.NewFlagSet(name, flag.ExitOnError)
		registerFacetFlags(fs, name)
		return printHelp(fs, name, want)
	}
	if err := wrongFacet(name, args); err != nil {
		return err
	}

	fs := flag.NewFlagSet(name, flag.ExitOnError)
	f := registerFacetFlags(fs, name)
	fs.Usage = func() { _ = printHelp(fs, name, "") }
	if err := parseArgs(fs, args, 2,
		fmt.Sprintf("usage: keera %s <org|team|key> <id> [flags]", name)); err != nil {
		return err
	}
	scope, scopeID := fs.Arg(0), fs.Arg(1)

	c := newClient()
	if changesSomething(fs) {
		change := func(lim *policy.Limits) error { return f.apply(name, lim) }
		if _, err := updateLimits(ctx, c, scope, scopeID, change); err != nil {
			return err
		}
	}
	// Print only this facet's part, even after a change: 'budget' answering
	// with every sandbox ceiling would be saying more than was asked.
	eff, err := effectiveFor(ctx, c, scope, scopeID)
	if err != nil {
		return err
	}
	return out(f.asJSON, eff, func(w *table) { printEffective(w, eff, facetFields[name]) })
}

// wrongFacet refuses a flag that belongs to the other facet, naming the
// command that takes it.
func wrongFacet(name string, args []string) error {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flagName := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		if other := facetOwning(flagName); other != "" && other != name {
			return fmt.Errorf("--%s is not part of 'keera %s'; it is 'keera %s --%s'",
				flagName, name, other, flagName)
		}
	}
	return nil
}

// changesSomething reports whether any flag other than --json was given.
func changesSomething(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name != "json" {
			set = true
		}
	})
	return set
}

// facetOwning names the facet a flag belongs to.
func facetOwning(flagName string) string {
	switch flagName {
	case "rpm", "tpm", "max-output-tokens":
		return "limit"
	case "budget", "period":
		return "budget"
	default:
		return ""
	}
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
	if eff.BlockHostedTools {
		showFrom(w, "hosted tools", "blocked",
			joinedFrom(eff, func(l policy.Limits) bool {
				return l.BlockHostedTools != nil && *l.BlockHostedTools
			}))
	} else {
		showFrom(w, "hosted tools", "(allowed)", "")
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
	if sb.MaxSandboxes == 0 {
		showFrom(w, "max sandboxes", "(unlimited)", "")
	} else {
		showFrom(w, "max sandboxes", sb.MaxSandboxes,
			narrowedBy(eff, func(l policy.Limits) bool { return l.MaxSandboxes != nil }))
	}
	if sb.MaxSandboxTTLSeconds > 0 {
		showFrom(w, "max sandbox lifetime",
			(time.Duration(sb.MaxSandboxTTLSeconds) * time.Second).String(),
			narrowedBy(eff, func(l policy.Limits) bool { return l.MaxSandboxTTLSeconds != nil }))
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
		_, _ = fmt.Fprintf(w, "%s\t(unlimited)\t\n", name)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\t%d\tset by %s\n", name, tightest, from)
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

// levelLabel names a level by its name, or by its id when it has none (a key
// issued without an alias).
func levelLabel(l control.EffectiveLevel) string {
	if l.Name == "" {
		return string(l.Type) + " " + l.ID
	}
	return string(l.Type) + " " + l.Name
}

func printLimits(w *table, lim policy.Limits) {
	show(w, "models", joinedOr(lim.AllowedModels, "(all)"))
	show(w, "max output tokens", orUnlimited(lim.MaxOutputTokens))
	show(w, "requests/min", orUnlimited(lim.RPM))
	show(w, "tokens/min", orUnlimited(lim.TPM))
	if lim.BudgetMicros == nil {
		show(w, "budget", "(unlimited)")
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
	show(w, "mcp tools", joinedOr(lim.AllowedTools, "(all)"))
	if lim.BlockHostedTools != nil && *lim.BlockHostedTools {
		show(w, "hosted tools", "blocked")
	} else {
		show(w, "hosted tools", "(not blocked here)")
	}
	if lim.SystemPrompt == nil {
		show(w, "system prompt", "(none)")
	} else {
		// One line; --json has the whole text.
		show(w, "system prompt", firstLine(*lim.SystemPrompt))
	}
	show(w, "sandbox classes", joinedOr(lim.SandboxClasses, "(all)"))
	show(w, "max sandboxes", orUnlimited(lim.MaxSandboxes))
	if lim.MaxSandboxTTLSeconds == nil {
		show(w, "max sandbox lifetime", "(unlimited)")
	} else {
		show(w, "max sandbox lifetime",
			(time.Duration(*lim.MaxSandboxTTLSeconds) * time.Second).String())
	}
	// In cores and mebibytes, the units the catalogue file and the flags use.
	if lim.MaxSandboxCPU == nil {
		show(w, "max sandbox size", "(unlimited)")
	} else {
		show(w, "max sandbox size", sizeOf(*lim.MaxSandboxCPU, derefInt(lim.MaxSandboxMemory)))
	}
}

// joinedOr joins a list for a table cell, or says what a nil list means.
func joinedOr(items []string, none string) string {
	if items == nil {
		return none
	}
	return strings.Join(items, ",")
}
