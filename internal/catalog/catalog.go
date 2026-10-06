// Package catalog reads the catalogue files, and knows the hosted providers a
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

// File is the on-disk shape.
type File struct {
	Models []Model `yaml:"models"`
}

// Model is one declared model.
//
// The fields a provider can fill in are pointers, so a stated zero differs from
// no value: an input or output price of 0 means unbilled, not "use the
// provider's price". A cached price of 0 means the full input price.
type Model struct {
	Alias string `yaml:"alias"`
	Kind  string `yaml:"kind"`
	// Provider names a hosted endpoint Keera Gateway knows (see `keera model
	// providers`). It fills in the backend, context window, prices,
	// description, release date and location. The entry may override any of
	// them.
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
	MaxContext               *int   `yaml:"max_context"`
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

// LoadModels reads and validates a model catalogue file.
func LoadModels(path string) ([]policy.Model, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	models, err := parseModelFile(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return models, nil
}

// parseModelFile reads and validates a model catalogue file, and returns its
// models.
func parseModelFile(raw []byte) ([]policy.Model, error) {
	var f File
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("parse catalogue: %w", err)
	}
	if len(f.Models) == 0 {
		return nil, fmt.Errorf("catalogue declares no models")
	}
	return parseEntries(f.Models, "models", "alias", func(m Model) string { return m.Alias }, ParseModel)
}

// decodeStrict reads a catalogue file and refuses a field it does not know. A
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

// parseEntries parses each entry of a catalogue file and refuses a name used
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
	case !policy.ValidAlias(m.Alias):
		return policy.Model{}, fmt.Errorf("alias %q must be lowercase letters, digits and "+
			"interior hyphens: it is rendered into the client configurations `keera connect` "+
			"prints, which quote none of it", m.Alias)
	}
	// Trimmed first, so a blank description gets the provider's default.
	m.Description = strings.TrimSpace(m.Description)
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
	if m.Location == "" {
		loc, err := policy.Model{Backends: m.Backends}.LocationFromBackends()
		if err != nil {
			return policy.Model{}, err
		}
		m.Location = loc
	}
	switch {
	case !policy.ValidLocation(m.Location):
		return policy.Model{}, fmt.Errorf("location %q must be a short lowercase name, "+
			"such as ch, usa or onprem", m.Location)
	case m.ReleaseDate != "" && !policy.ValidReleaseDate(m.ReleaseDate):
		return policy.Model{}, fmt.Errorf("release_date %q must be a day written as "+
			"YYYY-MM-DD", m.ReleaseDate)
	}
	kind := policy.Kind(m.Kind)
	if kind == "" {
		kind = policy.KindChat
	}
	if !kind.Valid() {
		return policy.Model{}, fmt.Errorf("unknown kind %q", m.Kind)
	}
	parsed := policy.Model{
		Alias:        m.Alias,
		Kind:         kind,
		Backends:     m.Backends,
		BackendModel: m.BackendModel,
		// Kept so an editor can tell which values came from the provider.
		Provider:                 m.Provider,
		Description:              m.Description,
		InputMicrosPerMTok:       deref(m.InputMicrosPerMTok),
		OutputMicrosPerMTok:      deref(m.OutputMicrosPerMTok),
		CachedInputMicrosPerMTok: deref(m.CachedInputMicrosPerMTok),
		MaxContext:               deref(m.MaxContext),
		ReleaseDate:              m.ReleaseDate,
		Location:                 m.Location,
		Subscription:             m.Subscription,
		Enabled:                  !m.Disabled,
	}
	if err := CheckSubscription(parsed); err != nil {
		return policy.Model{}, err
	}
	return parsed, nil
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

// checkBackend checks that an entry says where to send a request and which
// model to ask for there.
func checkBackend(m Model) error {
	switch {
	case m.ProductID != "" && m.Provider == "":
		return fmt.Errorf("product_id fills in a hosted provider's endpoint, " +
			"and this entry names no provider; put the id in the backend URL instead")
	case len(m.Backends) > 0 && m.BackendModel == "" && m.Provider != "":
		return fmt.Errorf("backend_model is required - for the %s provider it is "+
			"the model id, which `keera model providers` lists", m.Provider)
	}
	return policy.Model{Backends: m.Backends, BackendModel: m.BackendModel}.CheckBackend()
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
