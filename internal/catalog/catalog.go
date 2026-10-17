// Package catalog reads the template files, and knows the hosted providers a
// model can name.
//
// The model file is a template: every new organisation starts with a copy of each
// model it declares. The copies are the organisation's own, to change or
// remove like any model it adds itself.
package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// file is the on-disk shape.
type file struct {
	Models []Model `yaml:"models"`
}

// Model is one declared model.
//
// The fields a provider can fill in are pointers, so a stated zero differs from
// no value: an input or output price of 0 means unbilled, not "use the
// provider's price". A cached or cache-write price of 0 means the input price.
type Model struct {
	Alias string `yaml:"alias"`
	Kind  string `yaml:"kind"`
	// DisplayName is the model's name for people, such as Claude Haiku 5.5.
	// Clients name the alias; this is only shown.
	DisplayName string `yaml:"display_name"`
	// Provider names a hosted endpoint Keera Gateway knows (see `keera model
	// catalogue`). It fills in the backend, context window, prices, display
	// name, description, release date and location. The entry may override
	// any of them.
	Provider string   `yaml:"provider"`
	Backends []string `yaml:"backends"`
	// ProductID fills the hole in a per-customer endpoint, such as the product
	// id of an Infomaniak AI Service. An entry that writes its own backends
	// needs none.
	ProductID    string `yaml:"product_id"`
	BackendModel string `yaml:"backend_model"`
	// Description says what the model is for. Clients see it on /v1/models,
	// and routers choose between models by it. A provider fills in its own
	// when the entry states none.
	Description         string `yaml:"description"`
	InputMicrosPerMTok  *int64 `yaml:"input_micros_per_mtok"`
	OutputMicrosPerMTok *int64 `yaml:"output_micros_per_mtok"`
	// CachedInputMicrosPerMTok prices an input token served from the
	// provider's prompt cache. Without it, and without a provider rate, cached
	// tokens cost the full input price.
	CachedInputMicrosPerMTok *int64 `yaml:"cached_input_micros_per_mtok"`
	// CacheWriteMicrosPerMTok prices an input token the provider wrote to its
	// prompt cache. Without it, and without a provider rate, a write costs
	// the input price.
	CacheWriteMicrosPerMTok *int64 `yaml:"cache_write_micros_per_mtok"`
	// LongPrompt is what a request costs instead when its prompt is longer
	// than above_tokens. A provider fills it in for a model priced by prompt
	// length.
	LongPrompt *policy.PriceTier `yaml:"long_prompt"`
	MaxContext *int              `yaml:"max_context"`
	// ReleaseDate is the day the model came out, as YYYY-MM-DD. A provider
	// fills it in for the models it knows.
	ReleaseDate string `yaml:"release_date"`
	// Location is where the model runs: a country, such as ch or usa, or
	// onprem. A provider fills in its own. Without one, a backend inside the
	// network is onprem, and one outside it must state its location.
	Location string `yaml:"location"`
	// Subscription says each caller's own Claude subscription pays, and the
	// gateway forwards their sign-in. See docs/subscriptions.md.
	Subscription bool `yaml:"subscription"`
	Disabled     bool `yaml:"disabled"`
}

// LoadModels reads and validates a model template file.
func LoadModels(path string) ([]policy.Model, error) {
	return load(path, parseModelFile)
}

// load reads a template file and parses it, naming the file in any error.
func load[T any](path string, parse func([]byte) ([]T, error)) ([]T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}

// parseModelFile reads and validates a model template file, and returns its
// models.
func parseModelFile(raw []byte) ([]policy.Model, error) {
	var f file
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("parse catalogue: %w", err)
	}
	if len(f.Models) == 0 {
		return nil, fmt.Errorf("catalogue declares no models")
	}
	return parseEntries(f.Models, "models", "alias", func(m Model) string { return m.Alias }, ParseModel)
}

// decodeStrict reads a template file and refuses a field it does not know. A
// misspelt field would otherwise be dropped without a word, and whoever set it
// would think it took effect.
func decodeStrict(raw []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// parseEntries parses each entry of a template file and refuses a name used
// twice. Errors name the entry by index, and by name when it has one.
func parseEntries[E, T any](entries []E, list, field string, name func(E) string, parse func(E) (T, error)) ([]T, error) {
	seen := make(map[string]bool, len(entries))
	out := make([]T, 0, len(entries))
	for i, e := range entries {
		n := name(e)
		var parsed T
		var err error
		if n != "" && seen[n] {
			err = fmt.Errorf("%s %q is declared twice", field, n)
		} else {
			parsed, err = parse(e)
		}
		switch {
		case err != nil && n == "":
			return nil, fmt.Errorf("%s[%d]: %w", list, i, err)
		case err != nil:
			return nil, fmt.Errorf("%s[%d] (%s): %w", list, i, n, err)
		}
		seen[n] = true
		out = append(out, parsed)
	}
	return out, nil
}

// ParseModel validates and expands a single declared model, applying its
// provider's defaults. `keera model add` and `keera model set` use it, so an
// entry on the command line means what the same entry in a file means.
//
// The provider is applied before the checks, because it fills in the fields
// they check.
func ParseModel(m Model) (policy.Model, error) {
	switch {
	case m.Alias == "":
		return policy.Model{}, fmt.Errorf("alias is required")
	case !policy.ValidModelAlias(m.Alias):
		return policy.Model{}, fmt.Errorf("alias %q must be lowercase letters, digits, "+
			"interior hyphens and slashes: it is rendered into the client configurations `keera connect` "+
			"prints, which quote none of it", m.Alias)
	}
	// Trimmed first, so a blank description or name gets the provider's
	// default.
	m.Description = strings.TrimSpace(m.Description)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	m.ProductID = strings.TrimSpace(m.ProductID)
	m.ReleaseDate = strings.TrimSpace(m.ReleaseDate)
	m.Location = strings.ToLower(strings.TrimSpace(m.Location))
	if m.Provider = strings.ToLower(strings.TrimSpace(m.Provider)); m.Provider != "" {
		var err error
		if m, err = applyProvider(m); err != nil {
			return policy.Model{}, err
		}
	}
	if err := checkBackend(m); err != nil {
		return policy.Model{}, err
	}
	parsed := policy.Model{
		Alias:        m.Alias,
		Kind:         policy.Kind(m.Kind),
		Backends:     m.Backends,
		BackendModel: m.BackendModel,
		// Kept so an editor can tell which values came from the provider.
		Provider:                 m.Provider,
		DisplayName:              m.DisplayName,
		Description:              m.Description,
		InputMicrosPerMTok:       deref(m.InputMicrosPerMTok),
		OutputMicrosPerMTok:      deref(m.OutputMicrosPerMTok),
		CachedInputMicrosPerMTok: deref(m.CachedInputMicrosPerMTok),
		CacheWriteMicrosPerMTok:  deref(m.CacheWriteMicrosPerMTok),
		LongPrompt:               m.LongPrompt,
		MaxContext:               deref(m.MaxContext),
		ReleaseDate:              m.ReleaseDate,
		Location:                 m.Location,
		Subscription:             m.Subscription,
		Enabled:                  !m.Disabled,
	}
	priced := m.InputMicrosPerMTok != nil && m.OutputMicrosPerMTok != nil
	if err := CheckModel(&parsed, priced); err != nil {
		return policy.Model{}, err
	}
	return parsed, nil
}

// CheckModel fills in what a model may leave out and says what is wrong with
// it. Template files, the command line and the control API all run it, so
// none accepts what another refuses. priced says both prices were stated.
//
// A provider's other defaults are not applied here: the control API is sent
// every value, and the caller may have changed any of them.
func CheckModel(m *policy.Model, priced bool) error {
	if err := m.CheckBackend(); err != nil {
		return err
	}
	if m.Kind == "" {
		m.Kind = policy.KindChat
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("kind %q must be chat, completion, embedding or decision", m.Kind)
	}
	// The gateway reads the provider to choose how to talk to the backend,
	// so it must be one this build knows.
	prov, known := provider(m.Provider)
	switch {
	case m.Provider != "" && !known:
		return fmt.Errorf("unknown provider %q; Keera Gateway knows: %s",
			m.Provider, ProviderNames())
	case known:
		if err := prov.Check(m.Kind, m.BackendModel, priced); err != nil {
			return err
		}
	}
	// Every model runs somewhere: unstated, it is the provider's location, or
	// read off the backend.
	m.Location = strings.ToLower(strings.TrimSpace(m.Location))
	switch {
	case m.Location == "" && known:
		m.Location = prov.Location
	case m.Location == "":
		loc, err := m.LocationFromBackends()
		if err != nil {
			return err
		}
		m.Location = loc
	}
	m.ReleaseDate = strings.TrimSpace(m.ReleaseDate)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	switch {
	case !policy.ValidLocation(m.Location):
		return fmt.Errorf("location %q must be a short lowercase name, "+
			"such as ch, usa or onprem", m.Location)
	case m.ReleaseDate != "" && !policy.ValidReleaseDate(m.ReleaseDate):
		return fmt.Errorf("release_date %q must be a day written as "+
			"YYYY-MM-DD", m.ReleaseDate)
	}
	if m.LongPrompt != nil {
		if err := m.LongPrompt.Check(); err != nil {
			return err
		}
	}
	return CheckSubscription(*m)
}

// CheckSubscription says what is wrong with a subscription model, or nil when
// nothing is, or when m is not one.
//
// Each caller's Claude sign-in is sent to the model's backend. So the backend
// must be Anthropic's own API: any other address would collect those sign-ins.
func CheckSubscription(m policy.Model) error {
	if !m.Subscription {
		return nil
	}
	p, _ := provider("anthropic")
	switch {
	case m.Provider != p.Name:
		return fmt.Errorf("a subscription model must name the provider %s: "+
			"only a Claude plan can pay for it", p.Name)
	case m.Kind != policy.KindChat:
		return fmt.Errorf("a subscription model must be a chat model")
	case len(m.Backends) != 1 || strings.TrimRight(m.Backends[0], "/") != p.Endpoint:
		return fmt.Errorf("a subscription model is sent each caller's Claude sign-in, "+
			"so its only backend must be Anthropic's own API, %s; to go through "+
			"an egress proxy, set HTTPS_PROXY on the gateway", p.Endpoint)
	}
	return nil
}

// checkBackend makes the checks of an entry that only a template file or the
// command line can fail, before CheckModel makes the rest.
func checkBackend(m Model) error {
	switch {
	case m.ProductID != "" && m.Provider == "":
		return fmt.Errorf("product_id fills in a hosted provider's endpoint, " +
			"and this entry names no provider; put the id in the backend URL instead")
	case len(m.Backends) > 0 && m.BackendModel == "" && m.Provider != "":
		return fmt.Errorf("backend_model is required - for the %s provider it is "+
			"the model id, which `keera model catalogue` lists", m.Provider)
	}
	return nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
