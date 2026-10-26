package catalog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Provider is a hosted OpenAI-compatible endpoint Keera Gateway knows by name,
// so a model needs only an alias, a model id and an API key to use it (and,
// for Infomaniak, a product id).
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
	// CachedInput says the provider publishes a discounted rate for tokens
	// served from its prompt cache.
	CachedInput bool `json:"cached_input"`
	// Location is where the provider serves its models, and so where prompts
	// go: ch or usa. It is the default for every model declared against it.
	Location string `json:"location"`
	// Kinds are the API surfaces the endpoint serves.
	Kinds  []policy.Kind   `json:"kinds"`
	Models []ProviderModel `json:"models"`
	// Summary is the provider in one line, for the panel's provider tile.
	Summary string `json:"summary"`
}

// ProviderModel is one model the provider serves, with the numbers a catalogue
// entry would otherwise have to state by hand.
type ProviderModel struct {
	ID string `json:"id"`
	// DisplayName is the model's name for people, such as Claude Haiku 5.5.
	DisplayName string `json:"display_name"`
	MaxContext  int    `json:"max_context_tokens"`
	// ReleaseDate is the day the model came out, as YYYY-MM-DD, or empty when
	// it is not known.
	ReleaseDate         string `json:"release_date,omitempty"`
	InputMicrosPerMTok  int64  `json:"input_micros_per_mtok"`
	OutputMicrosPerMTok int64  `json:"output_micros_per_mtok"`
	// CachedInputMicrosPerMTok is the published rate for an input token
	// served from the prompt cache. A coding agent resends its context every
	// turn, so without it a long session is priced far too high. Zero means
	// no such rate, and those tokens cost the full input price.
	CachedInputMicrosPerMTok int64 `json:"cached_input_micros_per_mtok"`
	// CacheWriteMicrosPerMTok is the published rate for an input token
	// written to the prompt cache. Zero means no such rate, and a write costs
	// the input price.
	CacheWriteMicrosPerMTok int64 `json:"cache_write_micros_per_mtok,omitempty"`
	// LongPrompt is the dearer prices for a long prompt, for a model priced
	// by prompt length.
	LongPrompt *policy.PriceTier `json:"long_prompt,omitempty"`
	// Description is the default for an entry that states none, so routers
	// never choose between bare aliases. An entry's own description wins.
	Description string `json:"description"`
	// Snapshot is the dated id the maker also names the model by, as in
	// claude-haiku-4-5-20251001. Claude Code knows such a model only by it.
	Snapshot string `json:"snapshot,omitempty"`
}

// providers is the built-in table.
//
// Prices are micro-units of CHF per million tokens, at each provider's list
// price: a starting point for showback, which an entry can override.
//
// Release dates come from each maker's own announcement. An FP8 build takes
// the date of the model it was made from.
//
// Last checked against each pricing page on 2026-10-08. This is not a live
// feed, so a later price cut is over-charged until the next build.
//
// Retired models are dropped before their retirement date: a row for a model
// nobody can call is worse than none.
//
// Descriptions say what the other fields cannot: how big a model is and what
// it suits. A router reads them beside other providers' models and knows none
// by id, so they neither rank a model against its siblings nor name one by
// id. They leave out price, context, age and where the model runs.
var providers = []Provider{{
	Name:    "anthropic",
	Summary: "Anthropic's own Claude models, on Anthropic's own API.",
	// Both of Anthropic's APIs live under this address: /v1/messages, which
	// Messages clients are sent, and the OpenAI-compatible /v1/chat/completions,
	// which every other client is.
	Endpoint:    "https://api.anthropic.com/v1",
	CachedInput: true,
	Location:    "usa",
	Kinds:       []policy.Kind{policy.KindChat},
	Models: []ProviderModel{{
		ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5",
		MaxContext: 1_000_000, ReleaseDate: "2026-09-22",
		InputMicrosPerMTok: 3_680_000, OutputMicrosPerMTok: 18_400_000,
		CachedInputMicrosPerMTok: 184_000,
		CacheWriteMicrosPerMTok:  4_600_000,
		Description:              "flagship - long, hard coding and agent work",
	}, {
		ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5",
		MaxContext: 1_000_000, ReleaseDate: "2026-09-28",
		InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 9_200_000,
		CachedInputMicrosPerMTok: 92_000,
		CacheWriteMicrosPerMTok:  2_300_000,
		Description:              "mid-size and quick - everyday coding and agent work",
	}, {
		ID: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5",
		MaxContext: 1_000_000, ReleaseDate: "2026-10-07",
		InputMicrosPerMTok: 92_000, OutputMicrosPerMTok: 460_000,
		CachedInputMicrosPerMTok: 9_200,
		CacheWriteMicrosPerMTok:  115_000,
		// Anthropic charges five times as much for a prompt over 100k tokens.
		LongPrompt: &policy.PriceTier{
			AboveTokens:        100_000,
			InputMicrosPerMTok: 460_000, OutputMicrosPerMTok: 2_300_000,
			CachedInputMicrosPerMTok: 46_000,
			CacheWriteMicrosPerMTok:  575_000,
		},
		Description: "small and fast - short, simple, high-volume tasks",
	}, {
		ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1",
		MaxContext: 1_000_000, ReleaseDate: "2026-09-01",
		InputMicrosPerMTok: 9_200_000, OutputMicrosPerMTok: 46_000_000,
		CachedInputMicrosPerMTok: 230_000,
		CacheWriteMicrosPerMTok:  11_500_000,
		Description:              "very large and slow - best at writing, weaker at code",
	}, {
		ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5",
		MaxContext: 1_000_000, ReleaseDate: "2026-06-30",
		InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 9_200_000,
		CachedInputMicrosPerMTok: 184_000,
		CacheWriteMicrosPerMTok:  2_300_000,
		Description:              "mid-size and quick - everyday coding; replaced by Claude Sonnet 5.5",
	}, {
		ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5",
		Snapshot:   "claude-haiku-4-5-20251001",
		MaxContext: 200_000, ReleaseDate: "2025-10-15",
		InputMicrosPerMTok: 920_000, OutputMicrosPerMTok: 4_600_000,
		CachedInputMicrosPerMTok: 92_000,
		CacheWriteMicrosPerMTok:  1_150_000,
		Description:              "small and fast - short, simple tasks; replaced by Claude Haiku 5.5",
	}, {
		ID: "claude-opus-5", DisplayName: "Claude Opus 5",
		MaxContext: 1_000_000, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 4_600_000, OutputMicrosPerMTok: 23_000_000,
		CachedInputMicrosPerMTok: 460_000,
		CacheWriteMicrosPerMTok:  5_750_000,
		Description:              "flagship - long, hard coding and agent work; replaced by Claude Opus 5.5",
	}, {
		ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8",
		MaxContext: 1_000_000, ReleaseDate: "2026-05-28",
		InputMicrosPerMTok: 4_600_000, OutputMicrosPerMTok: 23_000_000,
		CachedInputMicrosPerMTok: 460_000,
		CacheWriteMicrosPerMTok:  5_750_000,
		Description:              "large and slow - hard problems; replaced by Claude Opus 5",
	}, {
		ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6",
		MaxContext: 1_000_000, ReleaseDate: "2026-02-17",
		InputMicrosPerMTok: 2_760_000, OutputMicrosPerMTok: 13_800_000,
		CachedInputMicrosPerMTok: 276_000,
		CacheWriteMicrosPerMTok:  3_450_000,
		Description:              "mid-size - everyday coding; replaced by Claude Sonnet 5",
	}},
}, {
	Name:        "openai",
	Summary:     "OpenAI's own GPT models, on the API they are released on.",
	Endpoint:    "https://api.openai.com/v1",
	CachedInput: true,
	Location:    "usa",
	Kinds:       []policy.Kind{policy.KindChat},
	// Past 272k input tokens, OpenAI charges twice as much for input and one
	// and a half times as much for output on its newer models. From gpt-5.6
	// on, a cache write costs a quarter more than input; before, nothing more.
	Models: []ProviderModel{{
		ID: "gpt-6-astra", DisplayName: "GPT-6 Astra",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-03",
		InputMicrosPerMTok: 9_200_000, OutputMicrosPerMTok: 46_000_000,
		CachedInputMicrosPerMTok: 920_000,
		CacheWriteMicrosPerMTok:  11_500_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 18_400_000, OutputMicrosPerMTok: 69_000_000,
			CachedInputMicrosPerMTok: 1_840_000,
			CacheWriteMicrosPerMTok:  23_000_000,
		},
		Description: "flagship and slow - the hardest problems",
	}, {
		ID: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-29",
		InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 9_200_000,
		CachedInputMicrosPerMTok: 92_000,
		CacheWriteMicrosPerMTok:  2_300_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_680_000, OutputMicrosPerMTok: 13_800_000,
			CachedInputMicrosPerMTok: 184_000,
			CacheWriteMicrosPerMTok:  4_600_000,
		},
		Description: "large - hard coding and agent work",
	}, {
		ID: "gpt-6.1-terra", DisplayName: "GPT-6.1 Terra",
		MaxContext: 1_050_000, ReleaseDate: "2026-10-26",
		InputMicrosPerMTok: 920_000, OutputMicrosPerMTok: 4_600_000,
		CachedInputMicrosPerMTok: 46_000,
		CacheWriteMicrosPerMTok:  1_150_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 6_900_000,
			CachedInputMicrosPerMTok: 92_000,
			CacheWriteMicrosPerMTok:  2_300_000,
		},
		Description: "mid-size and quick - everyday coding and questions",
	}, {
		ID: "gpt-6-sol", DisplayName: "GPT-6 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-22",
		InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 9_200_000,
		CachedInputMicrosPerMTok: 184_000,
		CacheWriteMicrosPerMTok:  2_300_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_680_000, OutputMicrosPerMTok: 13_800_000,
			CachedInputMicrosPerMTok: 368_000,
			CacheWriteMicrosPerMTok:  4_600_000,
		},
		Description: "large - hard coding and agent work; replaced by GPT-6.1 Sol",
	}, {
		ID: "gpt-6-luna", DisplayName: "GPT-6 Luna",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-22",
		InputMicrosPerMTok: 92_000, OutputMicrosPerMTok: 460_000,
		CachedInputMicrosPerMTok: 9_200,
		CacheWriteMicrosPerMTok:  115_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 184_000, OutputMicrosPerMTok: 690_000,
			CachedInputMicrosPerMTok: 18_400,
			CacheWriteMicrosPerMTok:  230_000,
		},
		Description: "small and fast - short, simple, high-volume tasks",
	}, {
		ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-07-09",
		InputMicrosPerMTok: 3_680_000, OutputMicrosPerMTok: 18_400_000,
		CachedInputMicrosPerMTok: 368_000,
		CacheWriteMicrosPerMTok:  4_600_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 7_360_000, OutputMicrosPerMTok: 27_600_000,
			CachedInputMicrosPerMTok: 736_000,
			CacheWriteMicrosPerMTok:  9_200_000,
		},
		Description: "large - hard work; replaced by GPT-6 Sol",
	}, {
		ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra",
		MaxContext: 1_050_000, ReleaseDate: "2026-07-09",
		InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 11_040_000,
		CachedInputMicrosPerMTok: 184_000,
		CacheWriteMicrosPerMTok:  2_300_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_680_000, OutputMicrosPerMTok: 16_560_000,
			CachedInputMicrosPerMTok: 368_000,
			CacheWriteMicrosPerMTok:  4_600_000,
		},
		Description: "mid-size and quick - everyday coding and questions; replaced by GPT-6.1 Terra",
	}, {
		ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna",
		MaxContext: 1_050_000, ReleaseDate: "2026-07-09",
		InputMicrosPerMTok: 184_000, OutputMicrosPerMTok: 1_104_000,
		CachedInputMicrosPerMTok: 18_400,
		CacheWriteMicrosPerMTok:  230_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 368_000, OutputMicrosPerMTok: 1_656_000,
			CachedInputMicrosPerMTok: 36_800,
			CacheWriteMicrosPerMTok:  460_000,
		},
		Description: "small and fast - short, simple tasks; replaced by GPT-6 Luna",
	}, {
		ID: "gpt-5.5", DisplayName: "GPT-5.5",
		MaxContext: 1_050_000, ReleaseDate: "2026-04-24",
		InputMicrosPerMTok: 4_600_000, OutputMicrosPerMTok: 27_600_000,
		CachedInputMicrosPerMTok: 460_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 9_200_000, OutputMicrosPerMTok: 41_400_000,
			CachedInputMicrosPerMTok: 920_000,
		},
		Description: "large and slow - thorough on hard problems",
	}, {
		ID: "gpt-5.4", DisplayName: "GPT-5.4",
		MaxContext: 1_050_000, ReleaseDate: "2026-03-05",
		InputMicrosPerMTok: 2_300_000, OutputMicrosPerMTok: 13_800_000,
		CachedInputMicrosPerMTok: 230_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 4_600_000, OutputMicrosPerMTok: 20_700_000,
			CachedInputMicrosPerMTok: 460_000,
		},
		Description: "mid-size - everyday work",
	}, {
		ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 mini",
		MaxContext: 400_000, ReleaseDate: "2026-03-17",
		InputMicrosPerMTok: 690_000, OutputMicrosPerMTok: 4_140_000,
		CachedInputMicrosPerMTok: 69_000,
		Description:              "small and quick - light reasoning",
	}, {
		ID: "gpt-5.4-nano", DisplayName: "GPT-5.4 nano",
		MaxContext: 400_000, ReleaseDate: "2026-03-17",
		InputMicrosPerMTok: 184_000, OutputMicrosPerMTok: 1_150_000,
		CachedInputMicrosPerMTok: 18_400,
		Description:              "tiny and fast - short, simple, high-volume tasks",
	}, {
		ID: "gpt-5.2", DisplayName: "GPT-5.2",
		MaxContext: 400_000, ReleaseDate: "2025-12-11",
		InputMicrosPerMTok: 1_610_000, OutputMicrosPerMTok: 12_880_000,
		CachedInputMicrosPerMTok: 161_000,
		Description:              "mid-size and slow - reasons before it answers",
	}, {
		ID: "gpt-5.1", DisplayName: "GPT-5.1",
		MaxContext: 400_000, ReleaseDate: "2025-11-13",
		InputMicrosPerMTok: 1_150_000, OutputMicrosPerMTok: 9_200_000,
		CachedInputMicrosPerMTok: 115_000,
		Description:              "large and slow - reasons at length before it answers",
	}, {
		ID: "gpt-4.1", DisplayName: "GPT-4.1",
		MaxContext: 1_047_576, ReleaseDate: "2025-04-14",
		InputMicrosPerMTok: 1_840_000, OutputMicrosPerMTok: 7_360_000,
		CachedInputMicrosPerMTok: 460_000,
		Description:              "mid-size and quick, no reasoning step - long documents",
	}, {
		ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 mini",
		MaxContext: 1_047_576, ReleaseDate: "2025-04-14",
		InputMicrosPerMTok: 368_000, OutputMicrosPerMTok: 1_472_000,
		CachedInputMicrosPerMTok: 92_000,
		Description:              "small and quick, no reasoning step - long documents",
	}, {
		ID: "gpt-4o", DisplayName: "GPT-4o",
		MaxContext: 128_000, ReleaseDate: "2024-05-13",
		InputMicrosPerMTok: 2_300_000, OutputMicrosPerMTok: 9_200_000,
		CachedInputMicrosPerMTok: 1_150_000,
		Description:              "mid-size and quick, no reasoning step - ordinary chat",
	}, {
		ID: "gpt-4o-mini", DisplayName: "GPT-4o mini",
		MaxContext: 128_000, ReleaseDate: "2024-07-18",
		InputMicrosPerMTok: 138_000, OutputMicrosPerMTok: 552_000,
		CachedInputMicrosPerMTok: 69_000,
		Description:              "small and fast, no reasoning step - short, simple tasks",
	}},
}, {
	Name:    "infomaniak",
	Summary: "Open-weight models, served in Switzerland.",
	// The address carries the product id of your AI Service.
	Endpoint:       "https://api.infomaniak.com/2/ai/{product_id}/openai/v1",
	NeedsProductID: true,
	Location:       "ch",
	Kinds:          []policy.Kind{policy.KindChat},
	// Ids are the upstream names, capitals and all, as the endpoint expects.
	Models: []ProviderModel{{
		ID: "Qwen/Qwen3.5-397B-A17B-FP8", DisplayName: "Qwen3.5 397B A17B FP8",
		MaxContext: 200_000, ReleaseDate: "2026-02-16",
		InputMicrosPerMTok: 896_000, OutputMicrosPerMTok: 4_032_000,
		Description: "large - hard reasoning, planning and tool use",
	}, {
		ID: "moonshotai/Kimi-K2.6", DisplayName: "Kimi K2.6",
		MaxContext: 256_000, ReleaseDate: "2026-04-20",
		InputMicrosPerMTok: 672_000, OutputMicrosPerMTok: 3_360_000,
		Description: "large - coding and agent work",
	}, {
		ID: "mistralai/Mistral-Small-4-119B-2603", DisplayName: "Mistral Small 4",
		MaxContext: 256_000, ReleaseDate: "2026-03-16",
		InputMicrosPerMTok: 224_000, OutputMicrosPerMTok: 840_000,
		Description: "mid-size - plain instructions or longer reasoning",
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B", DisplayName: "Apertus v1.5 70B",
		MaxContext: 100_000, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 784_000, OutputMicrosPerMTok: 2_800_000,
		Description: "mid-size, open training data - work that must be auditable",
	}, {
		ID: "google/gemma-4-31B-it", DisplayName: "Gemma 4 31B",
		MaxContext: 100_000, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 224_000, OutputMicrosPerMTok: 448_000,
		Description: "mid-size and quick - documents, chatbots and ordinary code",
	}, {
		ID: "Qwen/Qwen3.8-27B-FP8", DisplayName: "Qwen3.8 27B FP8",
		MaxContext: 256_000, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 448_000, OutputMicrosPerMTok: 3_360_000,
		Description: "small but strong for its size - ordinary code and questions",
	}, {
		ID: "mistralai/Ministral-3-14B-Instruct-2512", DisplayName: "Ministral 3 14B",
		MaxContext: 100_000, ReleaseDate: "2025-12-02",
		InputMicrosPerMTok: 336_000, OutputMicrosPerMTok: 448_000,
		Description: "small and quick - chatbots, short edits and everyday questions",
	}},
}, {
	Name:     "stepping-stone",
	Summary:  "Open-weight models, served in Switzerland.",
	Endpoint: "https://llm.stoney-cloud.com/v1",
	Location: "ch",
	Kinds:    []policy.Kind{policy.KindChat},
	// Ids are the upstream names, capitals and all, as the endpoint expects.
	// The Nemotron id is spelled as stepping stone's own documentation calls
	// it, with NVIDIA in capitals.
	Models: []ProviderModel{{
		ID: "MiniMaxAI/MiniMax-M2.5", DisplayName: "MiniMax M2.5",
		MaxContext: 192_000, ReleaseDate: "2026-02-12",
		InputMicrosPerMTok: 1_940_000, OutputMicrosPerMTok: 9_700_000,
		Description: "large - long coding and agent tasks with many steps",
	}, {
		ID: "NVIDIA/NVIDIA-Nemotron-3-Super-120B-A12B", DisplayName: "Nemotron 3 Super",
		MaxContext: 128_000, ReleaseDate: "2026-03-11",
		InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 5_000_000,
		Description: "large reasoning model - hard problems",
	}, {
		ID: "deepseek-ai/DeepSeek-V4-Flash-0731", DisplayName: "DeepSeek V4 Flash",
		MaxContext: 512_000, ReleaseDate: "2026-07-31",
		InputMicrosPerMTok: 450_000, OutputMicrosPerMTok: 1_800_000,
		Description: "mid-size and quick - large codebases and long documents",
	}, {
		ID: "Qwen/Qwen3-Coder-Next", DisplayName: "Qwen3 Coder Next",
		MaxContext: 256_000, ReleaseDate: "2026-02-03",
		InputMicrosPerMTok: 340_000, OutputMicrosPerMTok: 1_700_000,
		Description: "made for code - edits, code questions and agent tool calls",
	}, {
		ID: "zai-org/GLM-5.3-Flash", DisplayName: "GLM-5.3 Flash",
		MaxContext: 256_000, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 300_000, OutputMicrosPerMTok: 1_500_000,
		Description: "mid-size, quick all-rounder - code, reasoning and tool use",
	}, {
		ID: "openai/gpt-oss-120b", DisplayName: "gpt-oss-120b",
		MaxContext: 128_000, ReleaseDate: "2025-08-05",
		InputMicrosPerMTok: 400_000, OutputMicrosPerMTok: 1_600_000,
		Description: "mid-size, reasons before it answers - everyday work and tool use",
	}, {
		ID: "Qwen/Qwen3.8-27B", DisplayName: "Qwen3.8 27B",
		MaxContext: 256_000, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 1_500_000,
		Description: "small but strong for its size - ordinary code and questions",
	}, {
		ID: "google/gemma-4-31B-it", DisplayName: "Gemma 4 31B",
		MaxContext: 160_000, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 1_000_000,
		Description: "mid-size and quick - documents, chatbots and ordinary code",
	}, {
		ID: "Qwen/Qwen3.5-35B-A3B-FP8", DisplayName: "Qwen3.5 35B A3B FP8",
		MaxContext: 128_000, ReleaseDate: "2026-02-24",
		InputMicrosPerMTok: 170_000, OutputMicrosPerMTok: 1_000_000,
		Description: "small and fast, light on reasoning - high-volume, simple tasks",
	}, {
		ID: "apertus-ai/Apertus-v1.5-8B", DisplayName: "Apertus v1.5 8B",
		MaxContext: 128_000, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 100_000,
		Description: "small, open training data - simple tasks",
	}, {
		ID: "allenai/olmOCR-2-7B", DisplayName: "olmOCR 2",
		MaxContext: 8_000, ReleaseDate: "2025-10-22",
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 290_000,
		Description: "OCR - turns images of pages into text; not for chat or code",
	}, {
		ID: "lightonai/LightOnOCR-2-1B", DisplayName: "LightOnOCR 2",
		MaxContext: 16_000, ReleaseDate: "2026-01-19",
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 60_000,
		Description: "small, fast OCR - turns images of pages into text; not for chat or code",
	}, {
		ID: "opendatalab/MinerU2.5-2509-1.2B", DisplayName: "MinerU 2.5",
		MaxContext: 16_000, ReleaseDate: "2025-09-19",
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 60_000,
		Description: "document parsing - turns images of pages into text, tables and formulas; not for chat",
	}},
}, {
	Name:     "phoeniqs",
	Summary:  "Open-weight models, served in Switzerland.",
	Endpoint: "https://maas.phoeniqs.com/v1",
	Location: "ch",
	Kinds:    []policy.Kind{policy.KindChat},
	// Ids are Phoeniqs's own names. One keeps its name when the model behind
	// it gets a minor upgrade, so the release dates are those of the models
	// served on 2026-10-21: GLM-5.2 for inference-glm5, GLM-5.3-Flash for
	// inference-glm5-flash and its -compact, and MinerU2.5-Pro-2605 for
	// inference-miner-u25.
	Models: []ProviderModel{{
		ID: "inference-glm5", DisplayName: "GLM-5",
		MaxContext: 131_072, ReleaseDate: "2026-08-28",
		InputMicrosPerMTok: 1_077_000, OutputMicrosPerMTok: 3_386_000,
		Description: "large - long agent sessions, hard code and terminal work",
	}, {
		ID: "inference-deepseek-v32", DisplayName: "DeepSeek V3.2",
		MaxContext: 163_840, ReleaseDate: "2025-12-01",
		InputMicrosPerMTok: 615_600, OutputMicrosPerMTok: 1_846_900,
		Description: "large reasoning model - hard questions and planning",
	}, {
		ID: "inference-qwen3-vl-235b", DisplayName: "Qwen3 VL 235B",
		MaxContext: 128_000, ReleaseDate: "2025-09-23",
		InputMicrosPerMTok: 700_300, OutputMicrosPerMTok: 2_000_000,
		Description: "large, reads images - screenshots, diagrams and scans",
	}, {
		ID: "inference-deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-07-31",
		InputMicrosPerMTok: 146_200, OutputMicrosPerMTok: 392_500,
		Description: "mid-size and quick - large codebases and long documents",
	}, {
		ID: "inference-glm5-flash", DisplayName: "GLM-5 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 123_000, OutputMicrosPerMTok: 410_000,
		Description: "mid-size, quick all-rounder - code, reasoning and tool use",
	}, {
		ID: "inference-glm5-flash-compact", DisplayName: "GLM-5 Flash Compact",
		MaxContext: 524_288, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 62_500, OutputMicrosPerMTok: 416_700,
		Description: "GLM-5 Flash with a smaller context - quicker and cheaper",
	}, {
		ID: "inference-apertus-v15-70b", DisplayName: "Apertus v1.5 70B",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 619_500, OutputMicrosPerMTok: 2_220_100,
		Description: "mid-size, open training data - work that must be auditable",
	}, {
		ID: "inference-gpt-oss-120b", DisplayName: "gpt-oss-120b",
		MaxContext: 131_072, ReleaseDate: "2025-08-05",
		InputMicrosPerMTok: 115_400, OutputMicrosPerMTok: 461_700,
		Description: "mid-size, reasons before it answers - everyday work and tool use",
	}, {
		ID: "inference-llama4-maverick", DisplayName: "Llama 4 Maverick",
		MaxContext: 1_048_576, ReleaseDate: "2025-04-05",
		InputMicrosPerMTok: 269_300, OutputMicrosPerMTok: 1_077_300,
		Description: "mid-size, reads up to four images a prompt - long documents",
	}, {
		ID: "inference-gemma4-31b", DisplayName: "Gemma 4 31B",
		MaxContext: 131_072, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 118_000, OutputMicrosPerMTok: 325_000,
		Description: "mid-size and quick, reads images - documents, chatbots and ordinary code",
	}, {
		ID: "inference-llama4-scout-17b", DisplayName: "Llama 4 Scout",
		MaxContext: 62_256, ReleaseDate: "2025-04-05",
		InputMicrosPerMTok: 192_400, OutputMicrosPerMTok: 638_700,
		Description: "small and quick, reads images - short inputs",
	}, {
		ID: "inference-qwq-32b", DisplayName: "QwQ 32B",
		MaxContext: 32_768, ReleaseDate: "2025-03-06",
		InputMicrosPerMTok: 923_400, OutputMicrosPerMTok: 923_400,
		Description: "reasoning model that thinks at length - short inputs only",
	}, {
		ID: "inference-qwen3-8b", DisplayName: "Qwen3 8B",
		MaxContext: 40_960, ReleaseDate: "2025-04-29",
		InputMicrosPerMTok: 26_900, OutputMicrosPerMTok: 106_200,
		Description: "small and quick, with a thinking mode - simple tasks",
	}, {
		ID: "inference-granite-33-8b", DisplayName: "Granite 3.3 8B",
		MaxContext: 32_768, ReleaseDate: "2025-04-16",
		InputMicrosPerMTok: 153_900, OutputMicrosPerMTok: 153_900,
		Description: "small - plain instructions, short and simple tasks",
	}, {
		ID: "inference-mistral-v03-7b", DisplayName: "Mistral 7B v0.3",
		MaxContext: 32_768, ReleaseDate: "2024-05-22",
		InputMicrosPerMTok: 153_900, OutputMicrosPerMTok: 153_900,
		Description: "small - chat in many languages, short and simple tasks",
	}, {
		ID: "inference-granite-vision-2b", DisplayName: "Granite Vision 3.2 2B",
		MaxContext: 8_192, ReleaseDate: "2025-02-26",
		InputMicrosPerMTok: 77_000, OutputMicrosPerMTok: 77_000,
		Description: "tiny vision model - describes images and reads charts; not for chat",
	}, {
		ID: "inference-miner-u25", DisplayName: "MinerU 2.5",
		MaxContext: 8_192, ReleaseDate: "2026-05-21",
		InputMicrosPerMTok: 380_000, OutputMicrosPerMTok: 230_000,
		Description: "document parsing - turns images of pages into text, tables and formulas; not for chat",
	}},
}, {
	Name:     "cscs",
	Summary:  "Open-weight models at the Swiss National Supercomputing Centre.",
	Endpoint: "https://api.inference.cscs.ch/v1",
	Location: "ch",
	Kinds:    []policy.Kind{policy.KindChat},
	// Ids are the upstream names, capitals and all, as the endpoint expects.
	// A -thinking id is the same weights with reasoning on and tool calls off,
	// so it keeps the release date of its model.
	Models: []ProviderModel{{
		ID: "moonshotai/Kimi-K2.7-Code", DisplayName: "Kimi K2.7 Code",
		MaxContext: 262_144, ReleaseDate: "2026-06-12",
		InputMicrosPerMTok: 1_300_000, OutputMicrosPerMTok: 6_050_000,
		Description: "made for code - long agent sessions over many files; reads images",
	}, {
		ID: "zai-org/GLM-5.2", DisplayName: "GLM-5.2",
		MaxContext: 976_000, ReleaseDate: "2026-08-28",
		InputMicrosPerMTok: 6_360_000, OutputMicrosPerMTok: 27_310_000,
		Description: "large and costly all-rounder - very long inputs",
	}, {
		ID: "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-BF16", DisplayName: "Nemotron 3 Super BF16",
		MaxContext: 262_144, ReleaseDate: "2026-03-11",
		InputMicrosPerMTok: 300_000, OutputMicrosPerMTok: 1_490_000,
		Description: "large reasoning model, quick - hard questions and tool use",
	}, {
		ID: "google/gemma-4-31B-it", DisplayName: "Gemma 4 31B",
		MaxContext: 262_144, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 420_000,
		Description: "mid-size and quick, reads images - documents, chatbots and ordinary code",
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B", DisplayName: "Apertus v1.5 70B",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 400_000,
		Description: "mid-size, open training data - work that must be auditable",
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B-thinking", DisplayName: "Apertus v1.5 70B Thinking",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 400_000,
		Description: "mid-size, open training data, reasons first, no tool calls - hard questions",
	}, {
		ID: "swiss-ai/Apertus-v1.5-8B", DisplayName: "Apertus v1.5 8B",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 10_000, OutputMicrosPerMTok: 40_000,
		Description: "small, open training data - simple tasks",
	}, {
		ID: "swiss-ai/Apertus-v1.5-8B-thinking", DisplayName: "Apertus v1.5 8B Thinking",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 10_000, OutputMicrosPerMTok: 40_000,
		Description: "small, open training data, reasons first, no tool calls - simple questions",
	}, {
		ID: "swiss-ai/Apertus-70B-Instruct-2509", DisplayName: "Apertus 70B",
		MaxContext: 64_000, ReleaseDate: "2025-09-02",
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 400_000,
		Description: "mid-size, open training data, weaker at tool use; replaced by Apertus v1.5 70B",
	}, {
		ID: "swiss-ai/Apertus-8B-Instruct-2509", DisplayName: "Apertus 8B",
		MaxContext: 32_768, ReleaseDate: "2025-09-02",
		InputMicrosPerMTok: 10_000, OutputMicrosPerMTok: 40_000,
		Description: "small, open training data; replaced by Apertus v1.5 8B",
	}},
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
	for i := range p.Models {
		p.Models[i].LongPrompt = p.Models[i].longPrompt()
	}
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

// Model finds one of the provider's models by its id or its dated id.
func (p Provider) Model(id string) (ProviderModel, bool) {
	for _, m := range p.Models {
		if m.ID == id || (m.Snapshot != "" && m.Snapshot == id) {
			return m, true
		}
	}
	return ProviderModel{}, false
}

// longPrompt is a copy of the model's long-prompt prices, so a model given
// them cannot change the table's.
func (m ProviderModel) longPrompt() *policy.PriceTier {
	if m.LongPrompt == nil {
		return nil
	}
	t := *m.LongPrompt
	return &t
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
	case productID != "" && !ValidProductID(productID):
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

// ValidProductID reports whether an id is safe to put into a URL path.
func ValidProductID(id string) bool {
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
	if m.Location == "" {
		m.Location = p.Location
	}
	known, isKnown := p.Model(m.BackendModel)
	if m.BackendModel == "" || !isKnown {
		return m, nil
	}
	return fillFromTable(m, known), nil
}

// Check refuses a model the provider cannot serve or this build cannot bill:
// a kind the provider has no endpoint for, or a model missing from the price
// table that states no prices of its own. Catalogue files and the control API
// both run it, so neither accepts what the other refuses.
func (p Provider) Check(kind policy.Kind, backendModel string, priced bool) error {
	if kind != "" && !slices.Contains(p.Kinds, kind) {
		return fmt.Errorf("provider %s serves only %s, not %q",
			p.Name, p.kindNames(), kind)
	}
	if backendModel == "" {
		// The caller's own check reports it, in terms of this provider.
		return nil
	}
	// A model newer than this build must be priced by hand: a model that
	// silently costs nothing is one no budget can hold.
	if _, known := p.Model(backendModel); !known && !priced {
		return fmt.Errorf("%s is not in the Keera Gateway price table for %s "+
			"(it knows: %s); "+
			"state input_micros_per_mtok and output_micros_per_mtok for it - "+
			"0 to declare the model unbilled, which no budget can then hold",
			backendModel, p.Name, p.modelIDs())
	}
	return nil
}

// fillFromTable sets each value the entry left out to the table's.
func fillFromTable(m Model, known ProviderModel) Model {
	if m.MaxContext == nil {
		m.MaxContext = &known.MaxContext
	}
	if m.Description == "" {
		m.Description = known.Description
	}
	if m.DisplayName == "" {
		m.DisplayName = known.DisplayName
	}
	if m.ReleaseDate == "" {
		m.ReleaseDate = known.ReleaseDate
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
	if m.CacheWriteMicrosPerMTok == nil {
		m.CacheWriteMicrosPerMTok = &known.CacheWriteMicrosPerMTok
	}
	if m.LongPrompt == nil {
		m.LongPrompt = known.longPrompt()
	}
	return m
}
