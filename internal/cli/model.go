package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/gateway"
	"github.com/bespinian/keera-gateway/internal/policy"
)

const disabledUsage = "add the model without serving it yet"

// modelFlags are the fields of a catalogue entry, as flags. Each one left out
// means "leave this as it is", so `keera model set` can change one field.
type modelFlags struct {
	provider     string
	backends     stringList
	productID    string
	backendModel string
	kind         string
	description  string
	maxContext   int
	releaseDate  string
	location     string
	priceIn      float64
	priceOut     float64
	priceCached  float64
	apiKey       string
	noAPIKey     bool
	disabled     bool
}

func registerModelFlags(fs *flag.FlagSet) *modelFlags {
	f := &modelFlags{}
	fs.StringVar(&f.provider, "provider", "",
		"hosted provider filling in the endpoint, context window, prices, description, "+
			"release date and location")
	fs.Var(&f.backends, "backend", "an OpenAI-compatible base URL; repeat it for several")
	fs.StringVar(&f.productID, "product-id", "",
		"for a provider that serves each customer at their own address, the id its endpoint "+
			"carries - for infomaniak, your AI Service's product id")
	fs.StringVar(&f.backendModel, "backend-model", "",
		"the name the inference plane serves, which for vLLM is --served-model-name")
	fs.StringVar(&f.kind, "kind", "", "chat, completion or embedding (default chat)")
	fs.StringVar(&f.description, "description", "",
		"what this model is for, in a sentence; clients see it and routers decide on it "+
			"(a --provider model starts with that provider's own)")
	fs.IntVar(&f.maxContext, "max-context", -1, "context window: advertised to clients, and a request that cannot fit is refused")
	fs.StringVar(&f.releaseDate, "release-date", "",
		"the day the model came out, as YYYY-MM-DD (a --provider model starts with that provider's own)")
	fs.StringVar(&f.location, "location", "",
		"where the model runs, such as ch, usa or onprem (default: the provider's; "+
			"without one, onprem for a backend inside your network)")
	fs.Float64Var(&f.priceIn, "price-in", -1,
		"input price per million tokens, in whole currency units (0 for unbilled)")
	fs.Float64Var(&f.priceOut, "price-out", -1, "output price per million tokens")
	fs.Float64Var(&f.priceCached, "price-cached", -1,
		"price per million input tokens the provider served from its own prompt cache "+
			"(left out, they are charged at --price-in)")
	fs.StringVar(&f.apiKey, "api-key", "",
		"credential to store encrypted; @path reads a file and @- reads stdin")
	fs.BoolVar(&f.noAPIKey, "no-api-key", false, "remove the stored credential")
	return f
}

// applyModelFlags folds one invocation's flags into a declared catalogue entry.
func applyModelFlags(m catalog.Model, f *modelFlags) catalog.Model {
	if f.provider != "" {
		// Where the model runs and its address belong to the provider, so a
		// new provider brings its own, and its own values for the model. The
		// same flags given too win.
		if !strings.EqualFold(f.provider, m.Provider) {
			m.Location = ""
			m.Backends = nil
			m = clearModelValues(m)
		}
		m.Provider = f.provider
	}
	if len(f.backends) > 0 {
		m.Backends = f.backends
	}
	if f.productID != "" {
		m.ProductID = f.productID
		// The stored addresses have the old id baked in, so a new id means
		// building them again from the provider. A --backend given too wins.
		if len(f.backends) == 0 {
			m.Backends = nil
		}
	}
	if f.backendModel != "" {
		// Another model has its own release date, and with a provider its own
		// prices, context window and description, which the provider fills in
		// again. Without a provider only the date goes: nothing would refill
		// the prices, and a model with none is billed at nothing.
		if f.backendModel != m.BackendModel {
			m.ReleaseDate = ""
			if m.Provider != "" {
				m = clearModelValues(m)
			}
		}
		m.BackendModel = f.backendModel
	}
	if f.kind != "" {
		m.Kind = f.kind
	}
	if f.description != "" {
		m.Description = f.description
	}
	if f.maxContext >= 0 {
		n := f.maxContext
		m.MaxContext = &n
	}
	if f.releaseDate != "" {
		m.ReleaseDate = f.releaseDate
	}
	if f.location != "" {
		m.Location = f.location
	}
	setPrice(&m.InputMicrosPerMTok, f.priceIn)
	setPrice(&m.OutputMicrosPerMTok, f.priceOut)
	setPrice(&m.CachedInputMicrosPerMTok, f.priceCached)
	if f.disabled {
		m.Disabled = true
	}
	return m
}

// clearModelValues empties what a provider's table fills in for one model, so
// the table fills it in again for another.
func clearModelValues(m catalog.Model) catalog.Model {
	m.Description = ""
	m.ReleaseDate = ""
	m.InputMicrosPerMTok = nil
	m.OutputMicrosPerMTok = nil
	m.CachedInputMicrosPerMTok = nil
	m.MaxContext = nil
	return m
}

// setPrice applies a price flag, given in whole currency units per million
// tokens, where -1 means "not given".
func setPrice(dst **int64, units float64) {
	if units >= 0 {
		micros := int64(units * 1_000_000)
		*dst = &micros
	}
}

// credential resolves the credential flags to what the request carries: nil
// to leave the stored one alone, "" to remove it, or the secret itself.
func credential(f *modelFlags) (*string, error) {
	switch {
	case f.noAPIKey && f.apiKey != "":
		return nil, fmt.Errorf("--api-key and --no-api-key contradict each other")
	case f.noAPIKey:
		empty := ""
		return &empty, nil
	case f.apiKey != "":
		text, err := textOrFile(f.apiKey)
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, fmt.Errorf("--api-key was given but is empty; use --no-api-key to remove one")
		}
		return &text, nil
	}
	return nil, nil
}

// declared turns a stored model back into catalogue-file form, so `set` can
// change one field and send the whole entry through the same validation as
// `add` and a file.
func declared(m policy.Model) catalog.Model {
	in, outPrice, maxContext := m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.MaxContext
	cachedPrice := m.CachedInputMicrosPerMTok
	return catalog.Model{
		Alias:        m.Alias,
		Kind:         string(m.Kind),
		Backends:     m.Backends,
		BackendModel: m.BackendModel,
		// Kept so changing one field does not drop the provider. Its values are
		// already filled in, so expanding it again changes nothing.
		Provider:                 m.Provider,
		Description:              m.Description,
		InputMicrosPerMTok:       &in,
		OutputMicrosPerMTok:      &outPrice,
		CachedInputMicrosPerMTok: &cachedPrice,
		MaxContext:               &maxContext,
		ReleaseDate:              m.ReleaseDate,
		Location:                 m.Location,
		Disabled:                 !m.Enabled,
	}
}

// modelPut is a catalogue entry plus the write-only credential. The credential
// is left out unless this invocation set it, so a save never wipes a key
// nobody mentioned.
type modelPut struct {
	policy.Model
	APIKey *string `json:"api_key,omitempty"`
}

// modelRun is one 'keera model' invocation.
type modelRun struct {
	c      *client
	fs     *flag.FlagSet
	sub    string
	f      *modelFlags
	yes    bool
	asJSON bool
	// org is the organisation whose models to use.
	org string
}

// modelPath is a control API path for a model of orgID.
func modelPath(orgID, alias, suffix string) string {
	return inOrg("/v1/models/"+url.PathEscape(alias)+suffix, orgID)
}

func modelCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("model "+sub, flag.ExitOnError)
	r := &modelRun{fs: fs, sub: sub, f: registerModelFlags(fs)}
	fs.BoolVar(&r.f.disabled, "disabled", false, disabledUsage)
	fs.BoolVar(&r.yes, "yes", false, yesUsage)
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)
	fs.StringVar(&r.org, "org", "", orgUsage)

	fs.Usage = func() { _ = printHelp(fs, "model", sub) }
	if want, ok := wantsHelp(args); ok {
		return printHelp(fs, "model", want)
	}
	if err := parse(fs, rest); err != nil {
		return err
	}
	if err := verbFlags(fs, "model", sub); err != nil {
		return err
	}

	// These two need no server, so a catalogue can be checked in CI.
	switch sub {
	case "validate", "lint":
		return r.validate()
	case "providers":
		return r.providers()
	}

	r.c = newClient()
	// apply picks the organisation once the file has been read, so a bad file
	// fails without calling the control plane.
	if sub != "apply" {
		if err := r.resolveOrg(ctx); err != nil {
			return err
		}
	}
	switch sub {
	case "list", "ls", "":
		return r.list(ctx)
	case "add", "create", "new":
		return r.add(ctx)
	case "set", "edit", "update":
		return r.set(ctx)
	case "enable", "disable":
		return r.toggle(ctx)
	case "check", "probe", "test":
		return r.check(ctx)
	case "apply":
		return r.apply(ctx)
	case "delete", "rm", "remove":
		return r.delete(ctx)
	default:
		return unknownSub("model", sub)
	}
}

func (r *modelRun) validate() error {
	if err := r.args(1, "usage: keera model validate <catalogue.yaml>"); err != nil {
		return err
	}
	models, err := catalog.LoadModels(r.fs.Arg(0))
	if err != nil {
		return err
	}
	return out(r.asJSON, models, func(w *table) { printModels(w, models) })
}

func (r *modelRun) providers() error {
	return out(r.asJSON, catalog.Providers(), printProviders)
}

func (r *modelRun) list(ctx context.Context) error {
	models, err := r.catalogue(ctx)
	if err != nil {
		return err
	}
	return out(r.asJSON, models, func(w *table) { printModels(w, models) })
}

func (r *modelRun) add(ctx context.Context) error {
	if err := r.args(1, "usage: keera model add <alias> [flags]"); err != nil {
		return err
	}
	alias := r.fs.Arg(0)
	models, err := r.catalogue(ctx)
	if err != nil {
		return err
	}
	if _, found := findModel(models, alias); found {
		return fmt.Errorf("the model %s already exists; change it with: keera model set %s", alias, alias)
	}
	return r.save(ctx, r.org, alias, catalog.Model{Alias: alias})
}

func (r *modelRun) set(ctx context.Context) error {
	if err := r.args(1, "usage: keera model set <alias> [flags]"); err != nil {
		return err
	}
	// The endpoint replaces the entry, so read it first to keep what was not
	// given.
	current, err := r.requireModel(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return r.save(ctx, current.OrgID, current.Alias, declared(current))
}

// save applies the flags to m, validates it and writes it to orgID's models.
func (r *modelRun) save(ctx context.Context, orgID, alias string, m catalog.Model) error {
	parsed, err := catalog.ParseModel(applyModelFlags(m, r.f))
	if err != nil {
		return fmt.Errorf("%s: %w", alias, err)
	}
	parsed.OrgID = orgID
	cred, err := credential(r.f)
	if err != nil {
		return err
	}
	return r.putModel(ctx, parsed, cred)
}

func (r *modelRun) toggle(ctx context.Context) error {
	if err := r.args(1, fmt.Sprintf("usage: keera model %s <alias>", r.sub)); err != nil {
		return err
	}
	m, err := r.requireModel(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	m.Enabled = r.sub == "enable"
	return r.putModel(ctx, m, nil)
}

func (r *modelRun) check(ctx context.Context) error {
	if err := r.args(1, "usage: keera model check <alias>"); err != nil {
		return err
	}
	alias := r.fs.Arg(0)
	m, err := r.requireModel(ctx, alias)
	if err != nil {
		return err
	}
	var probe gateway.Probe
	if err := r.c.do(ctx, "POST", modelPath(m.OrgID, alias, "/check"), nil, &probe); err != nil {
		return err
	}
	if err := out(r.asJSON, probe, func(w *table) { printProbe(w, probe) }); err != nil {
		return err
	}
	// A failed check fails the command, so a pipeline can gate on it.
	if !probe.OK {
		return fmt.Errorf("%s did not pass its check", alias)
	}
	return nil
}

func (r *modelRun) apply(ctx context.Context) error {
	if err := r.args(1, "usage: keera model apply <catalogue.yaml>"); err != nil {
		return err
	}
	models, err := catalog.LoadModels(r.fs.Arg(0))
	if err != nil {
		return err
	}
	if err := r.resolveOrg(ctx); err != nil {
		return err
	}
	// One model at a time, as the endpoint takes them. The file is validated
	// first, so a stop halfway is the control plane going away, and a re-run
	// is safe.
	for i, m := range models {
		m.OrgID = r.org
		if err := r.c.do(ctx, "PUT", modelPath(r.org, m.Alias, ""), modelPut{Model: m}, &models[i]); err != nil {
			return fmt.Errorf("applying %s: %w", m.Alias, err)
		}
	}
	return out(r.asJSON, models, func(w *table) {
		printModels(w, models)
		_, _ = fmt.Fprintf(w, "\napplied %d model(s). A model the file does not name is left alone.\n",
			len(models))
	})
}

func (r *modelRun) delete(ctx context.Context) error {
	if err := r.args(1, "usage: keera model delete <alias> [--yes]"); err != nil {
		return err
	}
	m, err := r.requireModel(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if !r.yes {
		filters, routers, err := modelUsers(ctx, r.c, m.OrgID, m.Alias)
		if err != nil {
			return err
		}
		if err := confirmModelDelete(m, filters, routers); err != nil {
			return err
		}
	}
	return deleteAlias(ctx, r.c, modelPath(m.OrgID, m.Alias, ""), m.Alias, r.asJSON)
}

// args checks the number of positional arguments.
func (r *modelRun) args(n int, usage string) error {
	if r.fs.NArg() != n {
		return errors.New(usage)
	}
	return nil
}

// resolveOrg fills in the organisation, as every command does.
func (r *modelRun) resolveOrg(ctx context.Context) (err error) {
	r.org, err = resolveOrg(ctx, r.c, r.org)
	return err
}

func (r *modelRun) catalogue(ctx context.Context) ([]policy.Model, error) {
	return catalogue(ctx, r.c, r.org)
}

// catalogue lists an organisation's models. An empty orgID leaves the
// organisation to the control plane.
func catalogue(ctx context.Context, c *client, orgID string) ([]policy.Model, error) {
	return list[policy.Model](ctx, c, inOrg("/v1/models", orgID))
}

func findModel(models []policy.Model, alias string) (policy.Model, bool) {
	for _, m := range models {
		if m.Alias == alias {
			return m, true
		}
	}
	return policy.Model{}, false
}

// requireModel reads one of the organisation's models. There is no endpoint
// for a single model; the list is small.
func (r *modelRun) requireModel(ctx context.Context, alias string) (policy.Model, error) {
	return findAlias(ctx, r.c, inOrg("/v1/models", r.org), alias, "model",
		func(m policy.Model) string { return m.Alias })
}

func (r *modelRun) putModel(ctx context.Context, m policy.Model, cred *string) error {
	var saved policy.Model
	if err := r.c.do(ctx, "PUT", modelPath(m.OrgID, m.Alias, ""),
		modelPut{Model: m, APIKey: cred}, &saved); err != nil {
		return err
	}
	return out(r.asJSON, saved, func(w *table) { printModel(w, saved) })
}

// modelUsers lists the filters and routers that name a model. Deleting it
// does not stop them, so the confirmation names them.
func modelUsers(ctx context.Context, c *client, orgID, alias string) (filters, routers []string, err error) {
	fs, err := list[policy.Filter](ctx, c, inOrg("/v1/filters", orgID))
	if err != nil {
		return nil, nil, err
	}
	for _, f := range fs {
		if f.Model == alias {
			filters = append(filters, f.Alias)
		}
	}
	rs, err := list[policy.Router](ctx, c, inOrg("/v1/routers", orgID))
	if err != nil {
		return nil, nil, err
	}
	for _, rt := range rs {
		if rt.Model == alias || rt.Offers(alias) {
			routers = append(routers, rt.Alias)
		}
	}
	return filters, routers, nil
}

// confirmModelDelete makes the caller type the alias back. Clients name
// models, so removing one breaks every client that still names it.
func confirmModelDelete(m policy.Model, filters, routers []string) error {
	fmt.Printf("%s\n", style.head("Deleting the model "+m.Alias+":"))
	fmt.Printf("  every client that names it starts being refused\n")
	fmt.Printf("  guardrails that allow only %s stop allowing anything\n", m.Alias)
	if len(filters) > 0 {
		fmt.Printf("  the filters that run on it refuse every request they cover: %s\n",
			strings.Join(filters, ", "))
	}
	if len(routers) > 0 {
		fmt.Printf("  the routers that name it lose it: %s\n", strings.Join(routers, ", "))
	}
	if m.HasAPIKey {
		fmt.Println("  its stored credential is removed")
	}
	fmt.Println("Usage history and the audit log are kept.")
	return confirmTyping("alias", m.Alias, "nothing was deleted")
}

func printProviders(w *table) {
	for i, p := range catalog.Providers() {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		_, _ = fmt.Fprintf(w, "%s - %s\n", p.Name, p.Endpoint)
		if p.Summary != "" {
			_, _ = fmt.Fprintf(w, "  %s\n", p.Summary)
		}
		_, _ = fmt.Fprintf(w, "  runs in %s, serves %s\n", p.Location, kindNames(p.Kinds))
		if p.NeedsProductID {
			_, _ = fmt.Fprintf(w, "  its address is per customer, so a model "+
				"here also needs --product-id\n")
		}
		// The description goes last: a trailing cell is not padded, so a long
		// one does not push the other columns out of line.
		_, _ = fmt.Fprintf(w,
			"  MODEL\tRELEASED\tCONTEXT\tIN/MTOK\tCACHED/MTOK\tOUT/MTOK (%s)\tDESCRIPTION\n",
			p.Currency)
		for _, m := range p.Models {
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%d\t%s\t%s\t%s\t%s\n", m.ID, dash(m.ReleaseDate),
				m.MaxContext,
				policy.FormatMicros(m.InputMicrosPerMTok),
				cachedPrice(m.CachedInputMicrosPerMTok),
				policy.FormatMicros(m.OutputMicrosPerMTok), m.Description)
		}
		if p.Note != "" {
			_, _ = fmt.Fprintf(w, "  %s\n", p.Note)
		}
	}
}

func printModels(w *table, models []policy.Model) {
	w.header("ALIAS\tKIND\tBACKEND MODEL\tPROVIDER\tLOCATION\tRELEASED\tBACKENDS\t" +
		"IN/MTOK\tCACHED/MTOK\tOUT/MTOK\tCREDENTIAL\tENABLED")
	for _, m := range models {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Alias, m.Kind, m.BackendModel, dash(m.Provider), dash(m.Location),
			dash(m.ReleaseDate), strings.Join(m.Backends, ","),
			policy.FormatMicros(m.InputMicrosPerMTok), cachedPrice(m.CachedInputMicrosPerMTok),
			policy.FormatMicros(m.OutputMicrosPerMTok),
			credentialSource(m), statusWord(strconv.FormatBool(m.Enabled)))
	}
}

// cachedPrice is the cached-input column. A dash rather than 0.00 when no rate
// is stated, because those tokens are charged at the input price, not free.
func cachedPrice(micros int64) string {
	if micros == 0 {
		return "-"
	}
	return policy.FormatMicros(micros)
}

func printModel(w *table, m policy.Model) {
	show(w, "alias", m.Alias)
	show(w, "kind", m.Kind)
	show(w, "backend model", m.BackendModel)
	show(w, "backends", strings.Join(m.Backends, ", "))
	if m.Provider != "" {
		show(w, "provider", m.Provider)
	}
	show(w, "location", dash(m.Location))
	if m.ReleaseDate != "" {
		show(w, "released", m.ReleaseDate)
	}
	// Printed even when missing: without a description a router knows the
	// model by its alias alone.
	if m.Description != "" {
		show(w, "description", m.Description)
	} else {
		show(w, "description", "(none - a router choosing this model is told only its alias)")
	}
	if m.MaxContext > 0 {
		show(w, "max context", m.MaxContext)
	} else {
		show(w, "max context", "(not advertised)")
	}
	show(w, "in/mtok", policy.FormatMicros(m.InputMicrosPerMTok))
	show(w, "out/mtok", policy.FormatMicros(m.OutputMicrosPerMTok))
	// In words rather than 0.00: an unstated rate is charged at the input
	// price, not free.
	if m.CachedInputMicrosPerMTok > 0 {
		show(w, "cached in/mtok", policy.FormatMicros(m.CachedInputMicrosPerMTok))
	} else {
		show(w, "cached in/mtok", "(not stated - charged at the input price)")
	}
	show(w, "credential", credentialSource(m))
	show(w, "enabled", m.Enabled)
}

// credentialSource says whether a backend credential is stored, which is what
// an operator debugging a 401 wants to know.
func credentialSource(m policy.Model) string {
	if m.HasAPIKey {
		return "stored"
	}
	return "(none)"
}

func printProbe(w *table, p gateway.Probe) {
	show(w, "alias", p.Alias)
	if p.Backend != "" {
		show(w, "backend", p.Backend)
	}
	if p.Reachable {
		show(w, "reachable", fmt.Sprintf("yes (HTTP %d)", p.Status))
	} else {
		show(w, "reachable", "no")
	}
	show(w, "streamed", yesNo(p.Streamed))
	if p.ToolCallAsText {
		show(w, "tool calls", "returned as text - the backend's tool parser does not match this model")
	} else {
		show(w, "tool calls", yesNo(p.ToolCalls))
	}
	if p.Served != "" {
		show(w, "served model", p.Served)
	}
	if p.TTFTMillis > 0 {
		show(w, "first token", fmt.Sprintf("%dms", p.TTFTMillis))
	}
	if p.TotalMS > 0 {
		show(w, "total", fmt.Sprintf("%dms", p.TotalMS))
	}
	for _, warning := range p.Warnings {
		show(w, "warning", warning)
	}
	if p.Error != "" {
		show(w, "error", p.Error)
	}
	if !p.OK && p.Sample != "" {
		show(w, "sample", firstLine(p.Sample))
	}
	if p.OK {
		show(w, "result", "ok")
	} else {
		show(w, "result", "not ok")
	}
}

func kindNames(kinds []policy.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}
