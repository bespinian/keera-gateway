package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Provider is a hosted OpenAI-compatible endpoint Keera Gateway knows by name,
// so a model needs only an alias, a model id and an API key to use it.
//
// A provider is a table of defaults, not an adapter: these endpoints already
// speak the API the gateway forwards. Anthropic and OpenAI are also sent their
// own APIs untranslated when a client speaks them; the gateway does that by
// the provider's name. See docs/gateway.md.
type Provider struct {
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL, without a trailing slash.
	// With NeedsProductID it is a template holding {product_id}.
	Endpoint string `json:"endpoint"`
	// NeedsProductID says each customer has their own endpoint, as at
	// Infomaniak. An entry then states `product_id` or writes its own
	// `backends`.
	NeedsProductID bool `json:"needs_product_id,omitempty"`
	// APIKeyEnv is the environment variable holding the credential. The
	// secret never enters the catalogue or the database.
	APIKeyEnv string `json:"api_key_env"`
	// CachedInput says the provider publishes a discounted rate for tokens
	// served from its prompt cache.
	CachedInput bool `json:"cached_input"`
	// Currency the prices are quoted in, which may differ from the one this
	// deployment accounts in (see docs/providers.md).
	Currency string `json:"currency"`
	// Kinds are the API surfaces the endpoint serves.
	Kinds  []policy.Kind   `json:"kinds"`
	Models []ProviderModel `json:"models"`
	// Summary is the provider in one line, for the panel's provider tile.
	// Caveats belong in Note.
	Summary string `json:"summary"`
	// Note is what an operator needs to know before using it, shown by
	// `keera model providers` and in the control panel.
	Note string `json:"note"`
}

// ProviderModel is one model the provider serves, with the numbers a catalogue
// entry would otherwise have to state by hand.
type ProviderModel struct {
	ID                  string `json:"id"`
	MaxContext          int    `json:"max_context"`
	InputMicrosPerMTok  int64  `json:"input_micros_per_mtok"`
	OutputMicrosPerMTok int64  `json:"output_micros_per_mtok"`
	// CachedInputMicrosPerMTok is the published rate for an input token
	// served from the prompt cache. A coding agent resends its context every
	// turn, so without it a long session is priced far too high. Zero means
	// no such rate, and those tokens cost the full input price.
	CachedInputMicrosPerMTok int64 `json:"cached_input_micros_per_mtok"`
	// Description is the default for an entry that states none, so routers
	// never choose between bare aliases. An entry's own description wins.
	Description string `json:"description"`
}

// providers is the built-in table.
//
// Prices are micro-units per million tokens at each provider's list price, in
// its currency. They are a starting point for showback, and an entry can
// override them.
//
// Last checked against each pricing page: Anthropic on 2026-09-22, OpenAI and
// Infomaniak on 2026-09-21, and stepping stone against its offer of
// 2026-09-25. This is not a live price feed, so a price cut since then is
// over-charged until somebody rebuilds.
//
// Retired models are dropped before their retirement date, because a row for
// a model nobody can call is worse than no row. OpenAI's gpt-5, gpt-5-mini and
// gpt-5-nano go on 2026-12-11 and were dropped on 2026-09-21.
var providers = []Provider{{
	Name:    "anthropic",
	Summary: "Anthropic's own Claude models, on Anthropic's own API.",
	// Both of Anthropic's APIs live under this address: /v1/messages, which
	// Messages clients are sent, and the OpenAI-compatible /v1/chat/completions,
	// which every other client is.
	Endpoint:    "https://api.anthropic.com/v1",
	APIKeyEnv:   "ANTHROPIC_API_KEY",
	CachedInput: true,
	Currency:    "USD",
	Kinds:       []policy.Kind{policy.KindChat},
	// Descriptions are written for a router's choice: how fast, how dear and
	// how thorough each model is. Where it runs depends on the deployment,
	// so they do not say.
	Models: []ProviderModel{{
		ID: "claude-opus-5-5", MaxContext: 1_000_000,
		InputMicrosPerMTok: 4_000_000, OutputMicrosPerMTok: 20_000_000,
		CachedInputMicrosPerMTok: 200_000,
		Description: "most capable of its range and the one to reach for " +
			"first - thorough on long, hard work, at a middling speed",
	}, {
		ID: "claude-sonnet-5", MaxContext: 1_000_000,
		InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 10_000_000,
		CachedInputMicrosPerMTok: 200_000,
		Description:              "capable, quick enough and mid-priced - the middle option",
	}, {
		ID: "claude-haiku-4-5", MaxContext: 200_000,
		InputMicrosPerMTok: 1_000_000, OutputMicrosPerMTok: 5_000_000,
		CachedInputMicrosPerMTok: 100_000,
		Description: "fastest, cheapest and lightest of its range - short, " +
			"simple work",
	}, {
		ID: "claude-fable-5-1", MaxContext: 1_000_000,
		InputMicrosPerMTok: 10_000_000, OutputMicrosPerMTok: 50_000_000,
		CachedInputMicrosPerMTok: 250_000,
		Description:              "powerful, slow and dear, and stronger at prose than at code",
	}, {
		ID: "claude-opus-5", MaxContext: 1_000_000,
		InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 25_000_000,
		CachedInputMicrosPerMTok: 500_000,
		Description: "the powerful one before claude-opus-5-5 - as thorough " +
			"on hard problems, and dearer than what replaced it",
	}, {
		ID: "claude-opus-4-8", MaxContext: 1_000_000,
		InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 25_000_000,
		CachedInputMicrosPerMTok: 500_000,
		Description: "two generations back, and the most powerful of them - " +
			"thorough on hard problems, slow and dear",
	}, {
		ID: "claude-sonnet-4-6", MaxContext: 1_000_000,
		InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000,
		CachedInputMicrosPerMTok: 300_000,
		Description:              "previous generation's middle option - capable and mid-priced",
	}},
	Note: "Prompts leave your infrastructure. A client that speaks the " +
		"Messages API, such as Claude Code, is forwarded to Anthropic's own " +
		"API, where prompt caching and thinking work and cached input is " +
		"charged at the cached rate. A cache write is charged at the input " +
		"price, a quarter under what Anthropic charges. Every other client goes " +
		"through Anthropic's OpenAI-compatible surface, which Anthropic calls a " +
		"layer for evaluating models: prompt caching does not work there, so an " +
		"agent on it pays full price for every resent context.",
}, {
	Name:    "openai",
	Summary: "OpenAI's own GPT models, on the API they are released on.",
	// OpenAI's own API. The Note says where it differs from what the gateway
	// sends.
	Endpoint:    "https://api.openai.com/v1",
	APIKeyEnv:   "OPENAI_API_KEY",
	CachedInput: true,
	Currency:    "USD",
	Kinds:       []policy.Kind{policy.KindChat},
	Models: []ProviderModel{{
		ID: "gpt-6-astra", MaxContext: 1_050_000,
		InputMicrosPerMTok: 10_000_000, OutputMicrosPerMTok: 50_000_000,
		CachedInputMicrosPerMTok: 1_000_000,
		Description: "most powerful and dearest here - thorough on the hardest " +
			"problems, over a million-token context",
	}, {
		ID: "gpt-5.6-sol", MaxContext: 1_050_000,
		InputMicrosPerMTok: 4_000_000, OutputMicrosPerMTok: 20_000_000,
		CachedInputMicrosPerMTok: 400_000,
		Description: "nearly as powerful, quicker and far cheaper - hard work " +
			"without the top price",
	}, {
		ID: "gpt-5.6-terra", MaxContext: 1_050_000,
		InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 12_000_000,
		CachedInputMicrosPerMTok: 200_000,
		Description: "capable, quick enough and mid-priced - the middle option, " +
			"over a million-token context",
	}, {
		ID: "gpt-5.6-luna", MaxContext: 1_050_000,
		InputMicrosPerMTok: 200_000, OutputMicrosPerMTok: 1_200_000,
		CachedInputMicrosPerMTok: 20_000,
		Description: "fastest, cheapest and lightest of its range - short, simple, " +
			"high-volume work over a very long context",
	}, {
		ID: "gpt-5.5", MaxContext: 1_050_000,
		InputMicrosPerMTok: 5_000_000, OutputMicrosPerMTok: 30_000_000,
		CachedInputMicrosPerMTok: 500_000,
		Description: "previous generation's most powerful - thorough on hard " +
			"problems, slow and dear",
	}, {
		ID: "gpt-5.4", MaxContext: 1_050_000,
		InputMicrosPerMTok: 2_500_000, OutputMicrosPerMTok: 15_000_000,
		CachedInputMicrosPerMTok: 250_000,
		Description:              "previous generation's middle option - capable and mid-priced",
	}, {
		ID: "gpt-5.4-mini", MaxContext: 400_000,
		InputMicrosPerMTok: 750_000, OutputMicrosPerMTok: 4_500_000,
		CachedInputMicrosPerMTok: 75_000,
		Description: "middling power, quick and cheap - some reasoning, without " +
			"the flagship's price",
	}, {
		ID: "gpt-5.4-nano", MaxContext: 400_000,
		InputMicrosPerMTok: 200_000, OutputMicrosPerMTok: 1_250_000,
		CachedInputMicrosPerMTok: 20_000,
		Description:              "fast, cheap and light - short, simple, high-volume work",
	}, {
		ID: "gpt-5.2", MaxContext: 400_000,
		InputMicrosPerMTok: 1_750_000, OutputMicrosPerMTok: 14_000_000,
		CachedInputMicrosPerMTok: 175_000,
		Description:              "older mid-range model - capable, slow and mid-priced",
	}, {
		ID: "gpt-5.1", MaxContext: 400_000,
		InputMicrosPerMTok: 1_250_000, OutputMicrosPerMTok: 10_000_000,
		CachedInputMicrosPerMTok: 125_000,
		Description: "older flagship - slow and thorough, and it reasons at " +
			"length before answering",
	}, {
		ID: "gpt-4.1", MaxContext: 1_047_576,
		InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 8_000_000,
		CachedInputMicrosPerMTok: 500_000,
		Description: "capable, quick and mid-priced, answering without reasoning " +
			"first, over very long inputs",
	}, {
		ID: "gpt-4.1-mini", MaxContext: 1_047_576,
		InputMicrosPerMTok: 400_000, OutputMicrosPerMTok: 1_600_000,
		CachedInputMicrosPerMTok: 100_000,
		Description:              "light, quick and cheap, over very long inputs",
	}, {
		ID: "gpt-4o", MaxContext: 128_000,
		InputMicrosPerMTok: 2_500_000, OutputMicrosPerMTok: 10_000_000,
		CachedInputMicrosPerMTok: 1_250_000,
		Description:              "middling power, quick and no reasoning step - ordinary work",
	}, {
		ID: "gpt-4o-mini", MaxContext: 128_000,
		InputMicrosPerMTok: 150_000, OutputMicrosPerMTok: 600_000,
		CachedInputMicrosPerMTok: 75_000,
		Description:              "fast, cheap and light - short, simple work",
	}},
	Note: "Prompts leave your infrastructure. Prompt caching does work here, " +
		"and the gateway reads what it discounted off the response and charges " +
		"the cached-input rate for it. Reasoning tokens are billed as output, " +
		"which is what the gateway charges them as: a reasoning model costs more " +
		"than its output price and the prompt suggest, and the row's cost is " +
		"right even though nothing separates them out. " +
		"A client that speaks the Responses API, such as Codex, is forwarded " +
		"to it untranslated. The gpt-5 and gpt-6 models refuse max_tokens and " +
		"any temperature but their default, so the gateway sends " +
		"max_completion_tokens instead and drops the temperature and top_p. " +
		"Two prices here are one rate where OpenAI charges two. gpt-5.5 and " +
		"gpt-5.4 cost about twice as much per token once a request passes " +
		"272k input tokens, and the rates above are the cheaper ones, so a long " +
		"request against those two is billed low. gpt-5.6-sol is on a " +
		"promotional price until 21 November 2026.",
}, {
	Name:    "infomaniak",
	Summary: "Open-weight models, served in Switzerland.",
	// The address carries the product id of your AI Service.
	Endpoint:       "https://api.infomaniak.com/2/ai/{product_id}/openai/v1",
	NeedsProductID: true,
	APIKeyEnv:      "INFOMANIAK_API_KEY",
	Currency:       "CHF",
	Kinds:          []policy.Kind{policy.KindChat},
	// Ids are the upstream names, capitals and all, as the endpoint expects.
	Models: []ProviderModel{{
		ID: "Qwen/Qwen3.5-397B-A17B-FP8", MaxContext: 200_000,
		InputMicrosPerMTok: 800_000, OutputMicrosPerMTok: 3_600_000,
		Description: "the most powerful here - hard reasoning, planning and " +
			"tool use, and the dearest",
	}, {
		ID: "Qwen/Qwen3.5-122B-A10B-FP8", MaxContext: 200_000,
		InputMicrosPerMTok: 400_000, OutputMicrosPerMTok: 3_200_000,
		Description: "nearly as capable, quicker and cheaper - long-context " +
			"reasoning without the top price",
	}, {
		ID: "moonshotai/Kimi-K2.6", MaxContext: 256_000,
		InputMicrosPerMTok: 600_000, OutputMicrosPerMTok: 3_000_000,
		Description: "strongest here at code and agent work, mid-priced, over a " +
			"very long context",
	}, {
		ID: "mistralai/Mistral-Small-4-119B-2603", MaxContext: 256_000,
		InputMicrosPerMTok: 200_000, OutputMicrosPerMTok: 750_000,
		Description: "capable and cheap, over a long context - plain " +
			"instructions or longer reasoning",
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B", MaxContext: 100_000,
		InputMicrosPerMTok: 700_000, OutputMicrosPerMTok: 2_500_000,
		Description: "middling power and mid-priced, with open weights and open " +
			"training data - auditable work",
	}, {
		ID: "google/gemma-4-31B-it", MaxContext: 100_000,
		InputMicrosPerMTok: 200_000, OutputMicrosPerMTok: 400_000,
		Description: "middling power, quick and cheap - document analysis, " +
			"chatbots and ordinary code",
	}, {
		ID: "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B-FP8", MaxContext: 1_000_000,
		InputMicrosPerMTok: 50_000, OutputMicrosPerMTok: 200_000,
		Description: "fastest and cheapest here, over a million-token context - " +
			"high-volume, simple work",
	}, {
		ID: "mistralai/Ministral-3-14B-Instruct-2512", MaxContext: 100_000,
		InputMicrosPerMTok: 300_000, OutputMicrosPerMTok: 400_000,
		Description: "small, quick and cheap - chatbots, short edits and " +
			"everyday questions",
	}},
	Note: "Prompts leave your infrastructure, but stay in Switzerland: " +
		"Infomaniak serves these models from its own data centres. The endpoint " +
		"is per customer, so a model needs your AI Service's product id as well " +
		"as a key - both are in the Infomaniak console under AI Tools. " +
		"Infomaniak publishes no discount for a prompt it has already read, so " +
		"there is no cached-input rate to fill in and every input token is " +
		"charged at the input price. The prices are in CHF, so a deployment " +
		"accounting in CHF needs no conversion.",
}, {
	Name:        "stepping-stone",
	Summary:     "Open-weight models, served in Switzerland.",
	Endpoint:    "https://llm.stoney-cloud.com/v1",
	APIKeyEnv:   "STEPPING_STONE_API_KEY",
	CachedInput: true,
	Currency:    "CHF",
	Kinds:       []policy.Kind{policy.KindChat},
	// Ids are the upstream names, capitals and all, as the endpoint expects.
	// The Nemotron id is spelled as stepping stone's own documentation calls
	// it, with NVIDIA in capitals.
	Models: []ProviderModel{{
		ID: "MiniMaxAI/MiniMax-M2.5", MaxContext: 192_000,
		InputMicrosPerMTok: 1_940_000, OutputMicrosPerMTok: 9_700_000,
		CachedInputMicrosPerMTok: 291_000,
		Description: "the most capable and dearest here - long code and agent " +
			"tasks that need many steps",
	}, {
		ID: "NVIDIA/NVIDIA-Nemotron-3-Super-120B-A12B", MaxContext: 128_000,
		InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 5_000_000,
		CachedInputMicrosPerMTok: 300_000,
		Description: "large reasoning model with the highest input price here - " +
			"hard problems over a mid-length context",
	}, {
		ID: "deepseek-ai/DeepSeek-V4-Flash-0731", MaxContext: 512_000,
		InputMicrosPerMTok: 450_000, OutputMicrosPerMTok: 1_800_000,
		CachedInputMicrosPerMTok: 67_500,
		Description: "capable, quick and cheap, over the longest context here - " +
			"large codebases and long documents",
	}, {
		ID: "Qwen/Qwen3-Coder-Next", MaxContext: 256_000,
		InputMicrosPerMTok: 340_000, OutputMicrosPerMTok: 1_700_000,
		CachedInputMicrosPerMTok: 51_000,
		Description: "made for code - quick and cheap at edits, code questions " +
			"and agent tool calls",
	}, {
		ID: "zai-org/GLM-5.3-Flash", MaxContext: 256_000,
		InputMicrosPerMTok: 300_000, OutputMicrosPerMTok: 1_500_000,
		CachedInputMicrosPerMTok: 45_000,
		Description: "quick and cheap all-rounder over a long context - code, " +
			"reasoning and tool use",
	}, {
		ID: "openai/gpt-oss-120b", MaxContext: 128_000,
		InputMicrosPerMTok: 400_000, OutputMicrosPerMTok: 1_600_000,
		CachedInputMicrosPerMTok: 60_000,
		Description: "middling power, quick and cheap, and reasons before it " +
			"answers - everyday work and tool use",
	}, {
		ID: "Qwen/Qwen3.8-27B", MaxContext: 256_000,
		InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 1_500_000,
		CachedInputMicrosPerMTok: 37_500,
		Description: "small and cheap, capable for its size - ordinary code and " +
			"questions over a long context",
	}, {
		ID: "google/gemma-4-31B-it", MaxContext: 160_000,
		InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 1_000_000,
		CachedInputMicrosPerMTok: 37_500,
		Description: "middling power, quick and cheap - document analysis, " +
			"chatbots and ordinary code",
	}, {
		ID: "Qwen/Qwen3.5-35B-A3B-FP8", MaxContext: 128_000,
		InputMicrosPerMTok: 170_000, OutputMicrosPerMTok: 1_000_000,
		CachedInputMicrosPerMTok: 25_500,
		Description:              "fast and cheap, light on reasoning - high-volume, simple work",
	}, {
		ID: "apertus-ai/Apertus-v1.5-8B", MaxContext: 128_000,
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 100_000,
		CachedInputMicrosPerMTok: 3_000,
		Description: "the smallest and cheapest chat model here, with open weights " +
			"and open training data - simple work",
	}, {
		ID: "allenai/olmOCR-2-7B", MaxContext: 8_000,
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 290_000,
		CachedInputMicrosPerMTok: 9_000,
		Description:              "OCR: turns images of pages into text - not for chat or code",
	}, {
		ID: "lightonai/LightOnOCR-2-1B", MaxContext: 16_000,
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 60_000,
		CachedInputMicrosPerMTok: 3_000,
		Description: "small, fast OCR: reads the text off images of pages - not " +
			"for chat or code",
	}, {
		ID: "opendatalab/MinerU2.5-2509-1.2B", MaxContext: 16_000,
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 60_000,
		CachedInputMicrosPerMTok: 3_000,
		Description: "document parsing: turns images of pages into text, tables " +
			"and formulas - not for chat or code",
	}},
	Note: "Prompts leave your infrastructure, but stay in Switzerland. " +
		"stepping stone serves these models from its own stoney cloud. " +
		"The prices are in CHF.",
}}

// Providers returns a copy of the built-in table, so a caller cannot edit the
// defaults every catalogue is parsed against.
func Providers() []Provider {
	out := make([]Provider, len(providers))
	for i, p := range providers {
		out[i] = p.clone()
	}
	return out
}

// ProviderByName returns a copy of one entry of the table.
func ProviderByName(name string) (Provider, bool) {
	p, found := provider(name)
	if !found {
		return Provider{}, false
	}
	return p.clone(), true
}

// ProviderNames lists the table, for error messages.
func ProviderNames() string {
	return joinNames(providers, func(p Provider) string { return p.Name })
}

func (p Provider) clone() Provider {
	p.Kinds = slices.Clone(p.Kinds)
	p.Models = slices.Clone(p.Models)
	return p
}

func provider(name string) (Provider, bool) {
	for _, p := range providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

func (p Provider) model(id string) (ProviderModel, bool) {
	for _, m := range p.Models {
		if m.ID == id {
			return m, true
		}
	}
	return ProviderModel{}, false
}

func (p Provider) modelIDs() string {
	return joinNames(p.Models, func(m ProviderModel) string { return m.ID })
}

func (p Provider) kindNames() string {
	return joinNames(p.Kinds, func(k policy.Kind) string { return string(k) })
}

// joinNames lists items by name, comma separated, for error messages.
func joinNames[T any](items []T, name func(T) string) string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, name(it))
	}
	return strings.Join(names, ", ")
}

// productIDPlaceholder marks the part of a per-customer endpoint only the
// deployment knows.
const productIDPlaceholder = "{product_id}"

// fillProductID writes the product id into the entry's backends and checks
// that no placeholder is left.
//
// It fills every backend, not only the provider's default, because an entry
// may route through its own egress proxy with the same hole in its path.
func fillProductID(p Provider, backends []string, productID string) ([]string, error) {
	productID = strings.TrimSpace(productID)
	switch {
	case productID != "" && !p.NeedsProductID:
		return nil, fmt.Errorf("the %s provider serves everybody at the same address and "+
			"takes no product_id", p.Name)
	case productID != "" && !validProductID(productID):
		// It goes into a URL path, where a slash or space would build a
		// different working-looking address.
		return nil, fmt.Errorf("product_id %q must be letters, digits, hyphens and "+
			"underscores: it is written into the endpoint", productID)
	}
	// A copy, because `keera model set` passes in a stored model's slice.
	filled := make([]string, len(backends))
	for i, b := range backends {
		filled[i] = b
		// Without an id the placeholder stays, so the check below reports it
		// rather than a 404 on an empty path segment later.
		if productID != "" {
			filled[i] = strings.ReplaceAll(b, productIDPlaceholder, productID)
		}
		if strings.Contains(filled[i], productIDPlaceholder) {
			return nil, fmt.Errorf("the %s provider serves each customer at their own "+
				"address: state product_id (your AI Service's product id, which the %s "+
				"console shows under AI Tools), or write the finished backends here "+
				"yourself", p.Name, p.Name)
		}
	}
	return filled, nil
}

// validProductID reports whether an id is safe to put into a URL path.
func validProductID(id string) bool {
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// applyProvider fills in what the named provider implies and the entry did
// not state. The entry always wins, so a catalogue can point a provider at an
// egress proxy or set its own internal prices.
func applyProvider(m Model) (Model, error) {
	p, found := provider(m.Provider)
	if !found {
		return m, fmt.Errorf("unknown provider %q; Keera Gateway knows: %s",
			m.Provider, ProviderNames())
	}
	if len(m.Backends) == 0 {
		m.Backends = []string{p.Endpoint}
	}
	backends, err := fillProductID(p, m.Backends, m.ProductID)
	if err != nil {
		return m, err
	}
	m.Backends = backends
	if m.APIKeyEnv == "" {
		m.APIKeyEnv = p.APIKeyEnv
	}
	if kind := policy.Kind(m.Kind); kind != "" && !slices.Contains(p.Kinds, kind) {
		return m, fmt.Errorf("provider %s serves only %s, not %q",
			p.Name, p.kindNames(), m.Kind)
	}
	if m.BackendModel == "" {
		// The caller's own check reports it, in terms of this provider.
		return m, nil
	}
	known, isKnown := p.model(m.BackendModel)
	if !isKnown {
		// A model newer than this build must be priced by hand: a model that
		// silently costs nothing is one no budget can hold.
		if m.InputMicrosPerMTok == nil || m.OutputMicrosPerMTok == nil {
			return m, fmt.Errorf("%s is not in the Keera Gateway price table for %s "+
				"(it knows: %s); "+
				"state input_micros_per_mtok and output_micros_per_mtok for it - "+
				"0 to declare the model unbilled, which no budget can then hold",
				m.BackendModel, p.Name, p.modelIDs())
		}
		return m, nil
	}
	return fillFromTable(m, known), nil
}

// fillFromTable sets each value the entry left out to the table's.
func fillFromTable(m Model, known ProviderModel) Model {
	if m.MaxContext == nil {
		m.MaxContext = &known.MaxContext
	}
	if m.Description == "" {
		m.Description = known.Description
	}
	if m.InputMicrosPerMTok == nil {
		m.InputMicrosPerMTok = &known.InputMicrosPerMTok
	}
	if m.OutputMicrosPerMTok == nil {
		m.OutputMicrosPerMTok = &known.OutputMicrosPerMTok
	}
	if m.CachedInputMicrosPerMTok == nil {
		m.CachedInputMicrosPerMTok = &known.CachedInputMicrosPerMTok
	}
	return m
}
