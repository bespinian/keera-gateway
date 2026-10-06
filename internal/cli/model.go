package cli

import (
	"context"
	"flag"
	"fmt"
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
	// describe is whether --description was given, so an empty one clears.
	describe       bool
	maxContext     int
	releaseDate    string
	location       string
	priceIn        float64
	priceOut       float64
	priceCached    float64
	apiKey         string
	noAPIKey       bool
	subscription   bool
	noSubscription bool
	disabled       bool
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
			"(a --provider model starts with that provider's own); an empty one clears it")
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
	fs.BoolVar(&f.subscription, "subscription", false,
		"each caller's own Claude subscription pays; needs --provider anthropic, and takes no "+
			"API key (see docs/subscriptions.md)")
	fs.BoolVar(&f.noSubscription, "no-subscription", false,
		"the organisation pays again, with the model's stored API key")
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
	if f.describe {
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
	if f.subscription {
		m.Subscription = true
	}
	if f.noSubscription {
		m.Subscription = false
	}
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
		m := micros(units)
		*dst = &m
	}
}

// credential resolves --api-key and --no-api-key to what the request carries:
// nil to leave the stored one alone, "" to remove it, or the secret itself.
func credential(apiKey string, noAPIKey bool) (*string, error) {
	switch {
	case noAPIKey && apiKey != "":
		return nil, opposites("api-key", "no-api-key")
	case noAPIKey:
		empty := ""
		return &empty, nil
	case apiKey != "":
		text, err := textOrFile(apiKey)
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
		Subscription:             m.Subscription,
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
	*aliasRun
	f *modelFlags
}

func modelCmd(ctx context.Context, args []string) error {
	r := &modelRun{aliasRun: newAliasRun("model", "models", "model", args)}
	r.f = registerModelFlags(r.fs)
	r.fs.BoolVar(&r.f.disabled, "disabled", false, disabledUsage)
	if done, err := r.parse(); done {
		return err
	}
	r.f.describe = given(r.fs, "description")

	// These two need no server, so a catalogue can be checked in CI.
	switch r.verb {
	case "validate":
		return r.validate()
	case "providers":
		return r.providers()
	}

	// apply picks the organisation once the file has been read, so a bad file
	// fails without calling the control plane.
	if r.verb != "apply" {
		if err := r.resolveOrg(ctx); err != nil {
			return err
		}
	}
	switch r.verb {
	case "add":
		return r.add(ctx)
	case "set":
		return r.set(ctx)
	case "enable", "disable":
		return r.toggle(ctx)
	case "check":
		return r.check(ctx)
	case "apply":
		return r.apply(ctx)
	case "delete":
		return r.delete(ctx)
	default:
		return listAliases(ctx, r.aliasRun, printModels)
	}
}

func (r *modelRun) validate() error {
	models, err := catalog.LoadModels(r.fs.Arg(0))
	if err != nil {
		return err
	}
	return out(r.asJSON, models, func(w *table) { printModels(w, models) })
}

func (r *modelRun) providers() error {
	return out(r.asJSON, catalog.Providers(), printProviders)
}

func modelAlias(m policy.Model) string { return m.Alias }

func (r *modelRun) add(ctx context.Context) error {
	alias := r.fs.Arg(0)
	if err := alreadyExists(ctx, r.aliasRun, alias, modelAlias); err != nil {
		return err
	}
	return r.save(ctx, alias, catalog.Model{Alias: alias})
}

func (r *modelRun) set(ctx context.Context) error {
	if !changesSomething(r.fs) {
		return nothingToChange("model set")
	}
	// The endpoint replaces the entry, so read it first to keep what was not
	// given.
	current, err := r.find(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	return r.save(ctx, current.Alias, declared(current))
}

// save applies the flags to m, validates it and writes it to the
// organisation's models.
func (r *modelRun) save(ctx context.Context, alias string, m catalog.Model) error {
	parsed, err := catalog.ParseModel(applyModelFlags(m, r.f))
	if err != nil {
		return fmt.Errorf("%s: %w", alias, err)
	}
	parsed.OrgID = r.orgID
	if r.f.subscription && r.f.noSubscription {
		return opposites("subscription", "no-subscription")
	}
	cred, err := credential(r.f.apiKey, r.f.noAPIKey)
	if err != nil {
		return err
	}
	return r.putModel(ctx, parsed, cred)
}

func (r *modelRun) toggle(ctx context.Context) error {
	m, err := r.find(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	m.Enabled = r.verb == "enable"
	return r.putModel(ctx, m, nil)
}

func (r *modelRun) check(ctx context.Context) error {
	alias := r.fs.Arg(0)
	if _, err := r.find(ctx, alias); err != nil {
		return err
	}
	return checkProbe(ctx, r.c, r.path(alias, "/check"), "the model "+alias,
		r.asJSON, printProbe, func(p gateway.Probe) bool { return p.OK })
}

func (r *modelRun) apply(ctx context.Context) error {
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
		m.OrgID = r.orgID
		if err := r.c.do(ctx, "PUT", r.path(m.Alias, ""), modelPut{Model: m}, &models[i]); err != nil {
			return fmt.Errorf("applying %s: %w", m.Alias, err)
		}
	}
	return out(r.asJSON, models, func(w *table) {
		printModels(w, models)
		_, _ = fmt.Fprintf(w, "\napplied %s. A model the file does not name is left alone.\n",
			plural(len(models), "model"))
	})
}

func (r *modelRun) delete(ctx context.Context) error {
	m, err := r.find(ctx, r.fs.Arg(0))
	if err != nil {
		return err
	}
	var lines []string
	if !r.yes {
		filters, routers, err := modelUsers(ctx, r.c, m.OrgID, m.Alias)
		if err != nil {
			return err
		}
		lines = modelDeleteLines(m, filters, routers)
	}
	return r.deleteEntry(ctx, m.Alias, lines)
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

// find reads one of the organisation's models.
func (r *modelRun) find(ctx context.Context, alias string) (policy.Model, error) {
	return findAlias(ctx, r.aliasRun, alias, modelAlias)
}

func (r *modelRun) putModel(ctx context.Context, m policy.Model, cred *string) error {
	return putAlias(ctx, r.aliasRun, m.Alias, modelPut{Model: m, APIKey: cred}, printModel)
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

// modelDeleteLines is what deleting a model takes with it. Clients name
// models, so removing one breaks every client that still names it.
func modelDeleteLines(m policy.Model, filters, routers []string) []string {
	lines := []string{
		"  every client that names it starts being refused",
		"  guardrails that allow only " + m.Alias + " stop allowing anything",
	}
	if len(filters) > 0 {
		lines = append(lines, "  the filters that run on it refuse every request they cover: "+
			strings.Join(filters, ", "))
	}
	if len(routers) > 0 {
		lines = append(lines, "  the routers that name it lose it: "+strings.Join(routers, ", "))
	}
	if m.HasAPIKey {
		lines = append(lines, "  its stored credential is removed")
	}
	return append(lines, "Usage history and the audit log are kept.")
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
		w.header(fmt.Sprintf(
			"  MODEL\tRELEASED\tCONTEXT\tIN/MTOK\tCACHED/MTOK\tOUT/MTOK (%s)\tDESCRIPTION", p.Currency))
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
			credentialSource(m), yesNo(m.Enabled))
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
	if m.Subscription {
		show(w, "paid by", "each caller's own Claude subscription; the prices only say "+
			"what the API would have charged")
	}
	show(w, "enabled", yesNo(m.Enabled))
}

// credentialSource says whether a backend credential is stored, which is what
// an operator debugging a 401 wants to know.
func credentialSource(m policy.Model) string {
	switch {
	case m.Subscription:
		return "caller's Claude sign-in"
	case m.HasAPIKey:
		return "stored"
	}
	return "-"
}

func printProbe(w *table, p gateway.Probe) {
	show(w, "alias", p.Alias)
	if p.Backend != "" {
		show(w, "backend", p.Backend)
	}
	if p.Reachable {
		show(w, "reachable", fmt.Sprintf("%s (HTTP %d)", yesNo(true), p.Status))
	} else {
		show(w, "reachable", yesNo(false))
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
	show(w, "result", yesNo(p.OK))
}

func kindNames(kinds []policy.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}
