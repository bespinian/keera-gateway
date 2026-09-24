// Package catalog applies a declared model catalogue to the database.
//
// The catalogue is the gateway's API contract, so it lives in a file applied on
// every start: the same file and an empty database give the same gateway.
package catalog

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// File is the on-disk shape.
type File struct {
	Models     []Model     `yaml:"models"`
	MCPServers []MCPServer `yaml:"mcp_servers"`
}

// Catalogue is a parsed file: the models and the MCP servers it declares.
type Catalogue struct {
	Models     []policy.Model
	MCPServers []policy.MCPServer
}

// Model is one declared model.
//
// The fields a provider can fill in are pointers, so a stated zero differs from
// no value: a price of 0 means unbilled, not "use the provider's price".
type Model struct {
	Alias string `yaml:"alias"`
	Kind  string `yaml:"kind"`
	// Provider names a hosted endpoint Keera Gateway knows (see `keera model
	// providers`). It fills in the backend, credential variable, context
	// window, prices and description. The entry may override any of them.
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
	APIKeyEnv                string `yaml:"api_key_env"`
	Disabled                 bool   `yaml:"disabled"`
}

// Parse reads and validates a catalogue file, and returns its models.
func Parse(raw []byte) ([]policy.Model, error) {
	c, err := ParseFile(raw)
	return c.Models, err
}

// ParseFile reads and validates a catalogue file.
func ParseFile(raw []byte) (Catalogue, error) {
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return Catalogue{}, fmt.Errorf("parse catalogue: %w", err)
	}
	if len(f.Models) == 0 && len(f.MCPServers) == 0 {
		return Catalogue{}, fmt.Errorf("catalogue declares no models")
	}
	alias := func(m Model) string { return m.Alias }
	models, err := parseEntries(f.Models, "models", "alias", alias, func(m Model) (policy.Model, error) {
		parsed, err := ParseModel(m)
		// The file owns what it declares, so only another apply may change it.
		parsed.Managed = true
		return parsed, err
	})
	if err != nil {
		return Catalogue{}, err
	}
	servers, err := parseEntries(f.MCPServers, "mcp_servers", "alias",
		func(m MCPServer) string { return m.Alias },
		func(m MCPServer) (policy.MCPServer, error) {
			parsed, err := ParseMCPServer(m)
			parsed.Managed = true
			return parsed, err
		})
	if err != nil {
		return Catalogue{}, err
	}
	return Catalogue{Models: models, MCPServers: servers}, nil
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
	if m.Provider = strings.ToLower(strings.TrimSpace(m.Provider)); m.Provider != "" {
		var err error
		if m, err = applyProvider(m); err != nil {
			return policy.Model{}, err
		}
	}
	if err := checkBackend(m); err != nil {
		return policy.Model{}, err
	}
	kind := policy.Kind(m.Kind)
	if kind == "" {
		kind = policy.KindChat
	}
	if !kind.Valid() {
		return policy.Model{}, fmt.Errorf("unknown kind %q", m.Kind)
	}
	return policy.Model{
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
		APIKeyEnv:                m.APIKeyEnv,
		Enabled:                  !m.Disabled,
	}, nil
}

// checkBackend checks that an entry says where to send a request and which
// model to ask for there.
func checkBackend(m Model) error {
	switch {
	case m.ProductID != "" && m.Provider == "":
		return fmt.Errorf("product_id fills in a hosted provider's endpoint, " +
			"and this entry names no provider; put the id in the backend URL instead")
	case len(m.Backends) == 0:
		return fmt.Errorf("at least one backend is required")
	case m.BackendModel == "" && m.Provider != "":
		return fmt.Errorf("backend_model is required - for the %s provider it is "+
			"the model id, which `keera model providers` lists", m.Provider)
	case m.BackendModel == "":
		return fmt.Errorf("backend_model is required - it is the name the " +
			"inference plane serves, which for vLLM is --served-model-name")
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

// Apply writes a declared catalogue to the database. Models and MCP servers
// the file does not mention are kept, because removing one would break every
// client that names it.
//
// Declared models are marked as managed, so the panel and the CLI refuse to
// edit them: the next start would undo the edit. A model dropped from the file
// loses the mark on the next apply.
func Apply(ctx context.Context, st *store.Store, path string) (Catalogue, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Catalogue{}, err
	}
	c, err := ParseFile(raw)
	if err != nil {
		return Catalogue{}, fmt.Errorf("%s: %w", path, err)
	}
	declared := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		if err := st.UpsertModel(ctx, m); err != nil {
			return Catalogue{}, fmt.Errorf("apply %s: %w", m.Alias, err)
		}
		declared = append(declared, m.Alias)
	}
	if err := st.UnmanageModels(ctx, declared); err != nil {
		return Catalogue{}, fmt.Errorf("release models the catalogue no longer declares: %w", err)
	}
	declared = declared[:0]
	for _, m := range c.MCPServers {
		if err := st.UpsertMCPServer(ctx, m); err != nil {
			return Catalogue{}, fmt.Errorf("apply %s: %w", m.Alias, err)
		}
		declared = append(declared, m.Alias)
	}
	if err := st.UnmanageMCPServers(ctx, declared); err != nil {
		return Catalogue{}, fmt.Errorf("release MCP servers the catalogue no longer declares: %w", err)
	}
	return c, nil
}
