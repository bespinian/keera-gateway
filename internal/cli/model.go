package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	priceIn      float64
	priceOut     float64
	priceCached  float64
	apiKeyEnv    string
	apiKey       string
	noAPIKey     bool
	disabled     bool
}

func registerModelFlags(fs *flag.FlagSet) *modelFlags {
	f := &modelFlags{}
	fs.StringVar(&f.provider, "provider", "",
		"hosted provider filling in the endpoint, credential variable, context window, "+
			"prices and description")
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
	fs.Float64Var(&f.priceIn, "price-in", -1,
		"input price per million tokens, in whole currency units (0 for unbilled)")
	fs.Float64Var(&f.priceOut, "price-out", -1, "output price per million tokens")
	fs.Float64Var(&f.priceCached, "price-cached", -1,
		"price per million input tokens the provider served from its own prompt cache "+
			"(left out, they are charged at --price-in)")
	fs.StringVar(&f.apiKeyEnv, "api-key-env", "",
		"environment variable the gateway reads this backend's credential from")
	fs.StringVar(&f.apiKey, "api-key", "",
		"credential to store encrypted; @path reads a file and @- reads stdin")
	fs.BoolVar(&f.noAPIKey, "no-api-key", false, "remove the stored credential")
	return f
}

// applyModelFlags folds one invocation's flags into a declared catalogue entry.
func applyModelFlags(m catalog.Model, f *modelFlags) catalog.Model {
	if f.provider != "" {
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
	setPrice(&m.InputMicrosPerMTok, f.priceIn)
	setPrice(&m.OutputMicrosPerMTok, f.priceOut)
	setPrice(&m.CachedInputMicrosPerMTok, f.priceCached)
	if f.apiKeyEnv != "" {
		m.APIKeyEnv = f.apiKeyEnv
	}
	if f.disabled {
		m.Disabled = true
	}
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

// onlyCredential reports whether this invocation touches nothing but the
// stored credential, the one field no catalogue file declares.
func onlyCredential(f *modelFlags) bool {
	declared := f.provider != "" || len(f.backends) > 0 || f.productID != "" ||
		f.backendModel != "" || f.kind != "" || f.description != "" || f.maxContext >= 0 ||
		f.priceIn >= 0 || f.priceOut >= 0 || f.priceCached >= 0 || f.apiKeyEnv != "" ||
		f.disabled
	return !declared && (f.apiKey != "" || f.noAPIKey)
}

// errManaged says why a model the catalogue file declares cannot be changed
// here: the file is applied on every start and would undo the change.
func errManaged(alias, verb string) error {
	const preamble = "the model %s is declared in this deployment's catalogue file, which is " +
		"applied on every start; "
	if verb == "remove" {
		return fmt.Errorf(preamble+"remove it from the file instead, or leave it there and "+
			"disable it in the file", alias)
	}
	return fmt.Errorf(preamble+"%s it there and run 'keera model apply <file>', or remove it "+
		"from the file to take it over here", alias, verb)
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
		APIKeyEnv:                m.APIKeyEnv,
		Disabled:                 !m.Enabled,
	}
}

// modelPut is a catalogue entry plus the write-only credential. The credential
// is left out unless this invocation set it, so a save never wipes a key
// nobody mentioned.
type modelPut struct {
	policy.Model
	APIKey *string `json:"api_key,omitempty"`
	// FromCatalogue marks a write from `keera model apply`, which is what
	// lets it change an entry the catalogue file declares.
	FromCatalogue bool `json:"from_catalogue,omitempty"`
}

// modelRun is one 'keera model' invocation.
type modelRun struct {
	c      *client
	fs     *flag.FlagSet
	sub    string
	args   []string
	asJSON bool
}

func modelCmd(ctx context.Context, args []string) error {
	sub, rest := split(args)
	fs := flag.NewFlagSet("model "+sub, flag.ExitOnError)
	r := &modelRun{fs: fs, sub: sub, args: rest}
	fs.BoolVar(&r.asJSON, "json", false, jsonUsage)

	fs.Usage = func() { _ = printHelp(fs, "model", sub) }
	if want, ok := wantsHelp(args); ok {
		// 'add', 'set' and 'delete' declare these themselves; help needs them
		// on the set too.
		registerModelFlags(fs)
		fs.Bool("disabled", false, disabledUsage)
		fs.Bool("yes", false, yesUsage)
		return printHelp(fs, "model", want)
	}

	// These two need no server, so a catalogue can be checked in CI.
	switch sub {
	case "validate", "lint":
		return r.validate()
	case "providers":
		return r.providers()
	}

	r.c = newClient()
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
	if err := parseArgs(r.fs, r.args, 1, "usage: keera model validate <catalogue.yaml>"); err != nil {
		return err
	}
	models, err := parseCatalogue(r.fs.Arg(0))
	if err != nil {
		return err
	}
	return out(r.asJSON, models, func(w *table) { printModels(w, models) })
}

func (r *modelRun) providers() error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	return out(r.asJSON, catalog.Providers(), printProviders)
}

func (r *modelRun) list(ctx context.Context) error {
	if err := parse(r.fs, r.args); err != nil {
		return err
	}
	models, err := catalogue(ctx, r.c)
	if err != nil {
		return err
	}
	return out(r.asJSON, models, func(w *table) { printModels(w, models) })
}

func (r *modelRun) add(ctx context.Context) error {
	f := registerModelFlags(r.fs)
	r.fs.BoolVar(&f.disabled, "disabled", false, disabledUsage)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera model add <alias> [flags]"); err != nil {
		return err
	}
	alias := r.fs.Arg(0)
	models, err := catalogue(ctx, r.c)
	if err != nil {
		return err
	}
	if _, found := findModel(models, alias); found {
		return fmt.Errorf("the model %s already exists; change it with: keera model set %s", alias, alias)
	}
	return r.save(ctx, alias, catalog.Model{Alias: alias}, f)
}

func (r *modelRun) set(ctx context.Context) error {
	f := registerModelFlags(r.fs)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera model set <alias> [flags]"); err != nil {
		return err
	}
	// The endpoint replaces the entry, so read it first to keep what was not
	// given.
	current, err := requireModel(ctx, r.c, r.fs.Arg(0))
	if err != nil {
		return err
	}
	// No catalogue file carries a credential, so that is the one thing a
	// declared model still accepts here.
	if current.Managed && !onlyCredential(f) {
		return errManaged(current.Alias, "change")
	}
	return r.save(ctx, current.Alias, declared(current), f)
}

// save applies the flags to m, validates it and writes it.
func (r *modelRun) save(ctx context.Context, alias string, m catalog.Model, f *modelFlags) error {
	parsed, err := catalog.ParseModel(applyModelFlags(m, f))
	if err != nil {
		return fmt.Errorf("%s: %w", alias, err)
	}
	cred, err := credential(f)
	if err != nil {
		return err
	}
	return putModel(ctx, r.c, parsed, cred, r.asJSON)
}

func (r *modelRun) toggle(ctx context.Context) error {
	if err := parseArgs(r.fs, r.args, 1, fmt.Sprintf("usage: keera model %s <alias>", r.sub)); err != nil {
		return err
	}
	m, err := requireModel(ctx, r.c, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if m.Managed {
		return errManaged(m.Alias, r.sub)
	}
	m.Enabled = r.sub == "enable"
	return putModel(ctx, r.c, m, nil, r.asJSON)
}

func (r *modelRun) check(ctx context.Context) error {
	if err := parseArgs(r.fs, r.args, 1, "usage: keera model check <alias>"); err != nil {
		return err
	}
	alias := r.fs.Arg(0)
	// `check` used to take a catalogue file; point the old habit at validate.
	if looksLikeCatalogueFile(alias) {
		return fmt.Errorf("keera model check now probes a live model; "+
			"to validate a catalogue file use: keera model validate %s", alias)
	}
	var probe gateway.Probe
	if err := r.c.do(ctx, "POST", "/v1/models/"+url.PathEscape(alias)+"/check", nil, &probe); err != nil {
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
	if err := parseArgs(r.fs, r.args, 1, "usage: keera model apply <catalogue.yaml>"); err != nil {
		return err
	}
	models, err := parseCatalogue(r.fs.Arg(0))
	if err != nil {
		return err
	}
	// One model at a time, as the endpoint takes them. The file is validated
	// first, so a stop halfway is the control plane going away, and a re-run
	// is safe.
	for _, m := range models {
		put := modelPut{Model: m, FromCatalogue: true}
		if err := r.c.do(ctx, "PUT", "/v1/models/"+url.PathEscape(m.Alias), put, nil); err != nil {
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
	yes := r.fs.Bool("yes", false, yesUsage)
	if err := parseArgs(r.fs, r.args, 1, "usage: keera model delete <alias> [--yes]"); err != nil {
		return err
	}
	m, err := requireModel(ctx, r.c, r.fs.Arg(0))
	if err != nil {
		return err
	}
	if m.Managed {
		return errManaged(m.Alias, "remove")
	}
	if !*yes {
		if err := confirmModelDelete(m); err != nil {
			return err
		}
	}
	var gone struct {
		Alias   string `json:"alias"`
		Deleted bool   `json:"deleted"`
	}
	if err := r.c.do(ctx, "DELETE", "/v1/models/"+url.PathEscape(m.Alias), nil, &gone); err != nil {
		return err
	}
	return out(r.asJSON, gone, func(w *table) {
		_, _ = fmt.Fprintf(w, "deleted %s\n", gone.Alias)
	})
}

// looksLikeCatalogueFile spots the argument to the old `keera model check
// <file>`. It needs both a path-like name and an existing file, so a model is
// not mistaken for one because a file of that name happens to exist.
func looksLikeCatalogueFile(arg string) bool {
	switch strings.ToLower(filepath.Ext(arg)) {
	case ".yaml", ".yml", ".json":
	default:
		if !strings.ContainsRune(arg, filepath.Separator) {
			return false
		}
	}
	_, err := os.Stat(arg)
	return err == nil
}

// parseCatalogue reads and validates a catalogue file, without a server.
func parseCatalogue(path string) ([]policy.Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	models, err := catalog.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return models, nil
}

func catalogue(ctx context.Context, c *client) ([]policy.Model, error) {
	return list[policy.Model](ctx, c, "/v1/models")
}

func findModel(models []policy.Model, alias string) (policy.Model, bool) {
	for _, m := range models {
		if m.Alias == alias {
			return m, true
		}
	}
	return policy.Model{}, false
}

// requireModel reads one model from the catalogue. There is no endpoint for a
// single model; the list is small.
func requireModel(ctx context.Context, c *client, alias string) (policy.Model, error) {
	models, err := catalogue(ctx, c)
	if err != nil {
		return policy.Model{}, err
	}
	m, found := findModel(models, alias)
	if !found {
		return policy.Model{}, fmt.Errorf("no model %s (see: keera model list)", alias)
	}
	return m, nil
}

func putModel(ctx context.Context, c *client, m policy.Model, cred *string, asJSON bool) error {
	var saved policy.Model
	if err := c.do(ctx, "PUT", "/v1/models/"+url.PathEscape(m.Alias),
		modelPut{Model: m, APIKey: cred}, &saved); err != nil {
		return err
	}
	return out(asJSON, saved, func(w *table) { printModel(w, saved) })
}

// confirmModelDelete makes the operator type the alias back. Clients name
// models, so removing one breaks every client that still names it.
func confirmModelDelete(m policy.Model) error {
	fmt.Printf("Deleting the model %s:\n", m.Alias)
	fmt.Printf("  every client that names it starts being refused\n")
	fmt.Printf("  guardrails that allow only %s stop allowing anything\n", m.Alias)
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
		_, _ = fmt.Fprintf(w, "  reads the key from %s, serves %s\n",
			p.APIKeyEnv, kindNames(p.Kinds))
		if p.NeedsProductID {
			_, _ = fmt.Fprintf(w, "  its address is per customer, so a model "+
				"here also needs --product-id\n")
		}
		// The description goes last: a trailing cell is not padded, so a long
		// one does not push the other columns out of line.
		_, _ = fmt.Fprintf(w,
			"  MODEL\tCONTEXT\tIN/MTOK\tCACHED/MTOK\tOUT/MTOK (%s)\tDESCRIPTION\n",
			p.Currency)
		for _, m := range p.Models {
			_, _ = fmt.Fprintf(w, "  %s\t%d\t%s\t%s\t%s\t%s\n", m.ID, m.MaxContext,
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
	w.header("ALIAS\tKIND\tBACKEND MODEL\tPROVIDER\tBACKENDS\t" +
		"IN/MTOK\tCACHED/MTOK\tOUT/MTOK\tCREDENTIAL\tSOURCE\tENABLED")
	for _, m := range models {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Alias, m.Kind, m.BackendModel, dash(m.Provider), strings.Join(m.Backends, ","),
			policy.FormatMicros(m.InputMicrosPerMTok), cachedPrice(m.CachedInputMicrosPerMTok),
			policy.FormatMicros(m.OutputMicrosPerMTok),
			credentialSource(m), modelSource(m), statusWord(strconv.FormatBool(m.Enabled)))
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
	show(w, "source", modelSource(m))
	show(w, "enabled", m.Enabled)
}

// modelSource says who owns this model. A model the catalogue file declares
// is changed in the file, not with this command.
func modelSource(m policy.Model) string {
	if m.Managed {
		return "catalogue file"
	}
	return "control plane"
}

// credentialSource says where the backend credential comes from, which is
// what an operator debugging a 401 wants to know.
func credentialSource(m policy.Model) string {
	switch {
	case m.HasAPIKey && m.APIKeyEnv != "":
		return "stored (overrides " + m.APIKeyEnv + ")"
	case m.HasAPIKey:
		return "stored"
	case m.APIKeyEnv != "":
		return "$" + m.APIKeyEnv
	default:
		return "(none)"
	}
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
