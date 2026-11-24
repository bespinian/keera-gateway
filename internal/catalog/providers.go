package catalog

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Provider is a hosted OpenAI-compatible endpoint known by name, so a model
// needs only an alias, a model id and an API key (and, for Infomaniak, a
// product id).
//
// A provider is a table of defaults, not an adapter. Anthropic and OpenAI are
// also sent their own APIs untranslated. See https://docs.keera.ch/use/api/.
type Provider struct {
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL, without a trailing slash.
	// With NeedsProductID it is a template holding {product_id}.
	Endpoint string `json:"endpoint"`
	// NeedsProductID says each customer has their own endpoint, as at
	// Infomaniak. An entry then states `product_id` or its own `backends`.
	NeedsProductID bool `json:"needs_product_id,omitempty"`
	// Endpoints are the base URLs of kinds served somewhere else than
	// Endpoint, in the same form. See EndpointFor.
	Endpoints map[policy.Kind]string `json:"endpoints,omitempty"`
	// CachedInput says the provider publishes a discounted rate for tokens
	// served from its prompt cache.
	CachedInput bool `json:"cached_input"`
	// Location is where prompts go: ch, eu or usa. It is the default for its
	// models.
	Location string `json:"location"`
	// ChatCapabilities are what each of its chat models can do, unless the
	// model states its own. Model and Providers fill them in.
	ChatCapabilities []policy.Capability `json:"-"`
	// Kinds are the APIs its models are served on. The table leaves it out:
	// Providers fills it from Models, so the two cannot disagree.
	Kinds  []policy.Kind   `json:"kinds"`
	Models []ProviderModel `json:"models"`
	// Summary is the provider in one line, for the panel's provider tile.
	Summary string `json:"summary"`
}

// ProviderModel is one model the provider serves, with its numbers.
type ProviderModel struct {
	ID string `json:"id"`
	// DisplayName is the model's name for people, such as Claude Haiku 5.5.
	DisplayName string `json:"display_name"`
	// Kind is the API the model is served on. Empty means chat.
	Kind       policy.Kind `json:"kind,omitempty"`
	MaxContext int         `json:"max_context_tokens"`
	// ReleaseDate is the day the model came out, as YYYY-MM-DD, or empty.
	ReleaseDate         string `json:"release_date,omitempty"`
	InputMicrosPerMTok  int64  `json:"input_micros_per_mtok"`
	OutputMicrosPerMTok int64  `json:"output_micros_per_mtok"`
	// CachedInputMicrosPerMTok is the published rate for an input token
	// served from the prompt cache. Agents resend their context every turn,
	// so this matters. Zero means none: the input price applies.
	CachedInputMicrosPerMTok int64 `json:"cached_input_micros_per_mtok"`
	// CacheWriteMicrosPerMTok is the published rate for an input token
	// written to the prompt cache. Zero means none: the input price applies.
	CacheWriteMicrosPerMTok int64 `json:"cache_write_micros_per_mtok,omitempty"`
	// LongPrompt is the dearer prices for a long prompt, for a model priced
	// by prompt length.
	LongPrompt *policy.PriceTier `json:"long_prompt,omitempty"`
	// AudioMicrosPerMinute prices a minute of audio, for a transcription model
	// sold by the minute.
	AudioMicrosPerMinute int64 `json:"audio_micros_per_minute,omitempty"`
	// Capabilities are what the model can do beyond text, such as reading
	// images. Nil means the provider's ChatCapabilities.
	Capabilities []policy.Capability `json:"capabilities,omitempty"`
	// Description is the default for an entry that states none, so routers
	// never choose between bare aliases.
	Description string `json:"description"`
	// Snapshot is the dated id the maker also names the model by, as in
	// mistral-embed-2312. Claude Code knows a dated Anthropic model only by it.
	Snapshot string `json:"snapshot,omitempty"`
	// Location is where this model runs, for a provider that resells models
	// served in several places. Empty means the provider's.
	Location string `json:"location,omitempty"`
}

// The table shares these lists; Model and Providers hand out copies.
var (
	readsImages = []policy.Capability{policy.CapabilityVision}
	callsTools  = []policy.Capability{policy.CapabilityTools}
	// Sorted, as CheckCapabilities leaves a list.
	readsImagesCallsTools = []policy.Capability{policy.CapabilityTools, policy.CapabilityVision}
)

// providers is the built-in table.
//
// Prices are micro-CHF per million tokens, or per minute of audio for a
// transcription model sold by the minute, at list price unless a provider
// says otherwise; an entry can override them. Prices listed in USD are
// converted at 0.96 CHF per USD. Last checked against each pricing page on
// 2026-11-24.
//
// Capabilities come from each provider's own pages, checked on 2026-11-24. A
// model is marked only where its provider says so for that model: a wrong
// vision mark makes a gate fail, and a model that does more than it says is
// only missing from a filter.
//
// Release dates come from each maker's announcement. An FP8 build takes the
// date of its base model. Retired models are dropped before they retire.
//
// Descriptions say how big a model is and what it suits. A router reads them
// beside other providers' models, so they do not compare a model with its
// siblings or name one by id. They leave out price, context, age and
// location.
var providers = []Provider{{
	Name:    "anthropic",
	Summary: "Anthropic's own Claude models, on Anthropic's own API.",
	// Both of Anthropic's APIs live here: /v1/messages for Messages clients,
	// and /v1/chat/completions for every other client.
	Endpoint:    "https://api.anthropic.com/v1",
	CachedInput: true,
	Location:    "usa",
	// Every chat model of Anthropic reads images and calls tools.
	ChatCapabilities: readsImagesCallsTools,
	Models: []ProviderModel{{
		ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5",
		MaxContext: 1_000_000, ReleaseDate: "2026-09-22",
		InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 19_200_000,
		CachedInputMicrosPerMTok: 192_000,
		CacheWriteMicrosPerMTok:  4_800_000,
		Description:              "flagship - long, hard coding and agent work",
	}, {
		ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5",
		MaxContext: 1_000_000, ReleaseDate: "2026-09-28",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 9_600_000,
		CachedInputMicrosPerMTok: 96_000,
		CacheWriteMicrosPerMTok:  2_400_000,
		Description:              "mid-size and quick - everyday coding and agent work",
	}, {
		ID: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5",
		MaxContext: 1_000_000, ReleaseDate: "2026-10-07",
		InputMicrosPerMTok: 96_000, OutputMicrosPerMTok: 480_000,
		CachedInputMicrosPerMTok: 9_600,
		CacheWriteMicrosPerMTok:  120_000,
		// Anthropic charges five times as much for a prompt over 100k tokens.
		LongPrompt: &policy.PriceTier{
			AboveTokens:        100_000,
			InputMicrosPerMTok: 480_000, OutputMicrosPerMTok: 2_400_000,
			CachedInputMicrosPerMTok: 48_000,
			CacheWriteMicrosPerMTok:  600_000,
		},
		Description: "small and fast - short, simple, high-volume tasks",
	}, {
		ID: "claude-fable-5-2", DisplayName: "Claude Fable 5.2",
		MaxContext: 1_000_000, ReleaseDate: "2026-11-09",
		InputMicrosPerMTok: 7_680_000, OutputMicrosPerMTok: 38_400_000,
		CachedInputMicrosPerMTok: 192_000,
		CacheWriteMicrosPerMTok:  9_600_000,
		Description:              "very large, always reasons first - demanding reasoning and agent sessions that run for hours",
	}, {
		ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1",
		MaxContext: 1_000_000, ReleaseDate: "2026-09-01",
		InputMicrosPerMTok: 9_600_000, OutputMicrosPerMTok: 48_000_000,
		CachedInputMicrosPerMTok: 240_000,
		CacheWriteMicrosPerMTok:  12_000_000,
		Description:              "very large and slow - best at writing, weaker at code; replaced by Claude Fable 5.2",
	}, {
		ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5",
		MaxContext: 1_000_000, ReleaseDate: "2026-06-30",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 9_600_000,
		CachedInputMicrosPerMTok: 192_000,
		CacheWriteMicrosPerMTok:  2_400_000,
		Description:              "mid-size and quick - everyday coding; replaced by Claude Sonnet 5.5",
	}, {
		ID: "claude-opus-5", DisplayName: "Claude Opus 5",
		MaxContext: 1_000_000, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 4_800_000, OutputMicrosPerMTok: 24_000_000,
		CachedInputMicrosPerMTok: 480_000,
		CacheWriteMicrosPerMTok:  6_000_000,
		Description:              "flagship - long, hard coding and agent work; replaced by Claude Opus 5.5",
	}, {
		ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8",
		MaxContext: 1_000_000, ReleaseDate: "2026-05-28",
		InputMicrosPerMTok: 4_800_000, OutputMicrosPerMTok: 24_000_000,
		CachedInputMicrosPerMTok: 480_000,
		CacheWriteMicrosPerMTok:  6_000_000,
		Description:              "large and slow - hard problems; replaced by Claude Opus 5",
	}, {
		ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6",
		MaxContext: 1_000_000, ReleaseDate: "2026-02-17",
		InputMicrosPerMTok: 2_880_000, OutputMicrosPerMTok: 14_400_000,
		CachedInputMicrosPerMTok: 288_000,
		CacheWriteMicrosPerMTok:  3_600_000,
		Description:              "mid-size - everyday coding; replaced by Claude Sonnet 5",
	}},
}, {
	Name:        "openai",
	Summary:     "OpenAI's own GPT models, on the API they are released on.",
	Endpoint:    "https://api.openai.com/v1",
	CachedInput: true,
	Location:    "usa",
	// Past 272k input tokens, OpenAI's newer models cost twice as much for
	// input and 1.5 times for output. From gpt-5.6 on, a cache write costs a
	// quarter more than input; before, the same. GPT-5.6 Sol's price is a
	// promotion until 2026-11-30; OpenAI has not published the price after it.
	// Every chat model of OpenAI reads images and calls tools.
	ChatCapabilities: readsImagesCallsTools,
	Models: []ProviderModel{{
		ID: "gpt-6.2-astra", DisplayName: "GPT-6.2 Astra",
		MaxContext: 1_050_000, ReleaseDate: "2026-11-17",
		InputMicrosPerMTok: 7_680_000, OutputMicrosPerMTok: 38_400_000,
		CachedInputMicrosPerMTok: 384_000,
		CacheWriteMicrosPerMTok:  9_600_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 15_360_000, OutputMicrosPerMTok: 57_600_000,
			CachedInputMicrosPerMTok: 768_000,
			CacheWriteMicrosPerMTok:  19_200_000,
		},
		Description: "flagship and slow - the hardest problems",
	}, {
		ID: "gpt-6.2-sol", DisplayName: "GPT-6.2 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-11-17",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 9_600_000,
		CachedInputMicrosPerMTok: 96_000,
		CacheWriteMicrosPerMTok:  2_400_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 14_400_000,
			CachedInputMicrosPerMTok: 192_000,
			CacheWriteMicrosPerMTok:  4_800_000,
		},
		Description: "large - hard coding and agent work",
	}, {
		ID: "gpt-6-astra", DisplayName: "GPT-6 Astra",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-03",
		InputMicrosPerMTok: 9_600_000, OutputMicrosPerMTok: 48_000_000,
		CachedInputMicrosPerMTok: 960_000,
		CacheWriteMicrosPerMTok:  12_000_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 19_200_000, OutputMicrosPerMTok: 72_000_000,
			CachedInputMicrosPerMTok: 1_920_000,
			CacheWriteMicrosPerMTok:  24_000_000,
		},
		Description: "flagship and slow - the hardest problems; replaced by GPT-6.2 Astra",
	}, {
		ID: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-29",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 9_600_000,
		CachedInputMicrosPerMTok: 96_000,
		CacheWriteMicrosPerMTok:  2_400_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 14_400_000,
			CachedInputMicrosPerMTok: 192_000,
			CacheWriteMicrosPerMTok:  4_800_000,
		},
		Description: "large - hard coding and agent work; replaced by GPT-6.2 Sol",
	}, {
		ID: "gpt-6.1-terra", DisplayName: "GPT-6.1 Terra",
		MaxContext: 1_050_000, ReleaseDate: "2026-10-26",
		InputMicrosPerMTok: 960_000, OutputMicrosPerMTok: 4_800_000,
		CachedInputMicrosPerMTok: 48_000,
		CacheWriteMicrosPerMTok:  1_200_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 7_200_000,
			CachedInputMicrosPerMTok: 96_000,
			CacheWriteMicrosPerMTok:  2_400_000,
		},
		Description: "mid-size and quick - everyday coding and questions",
	}, {
		ID: "gpt-6-sol", DisplayName: "GPT-6 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-22",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 9_600_000,
		CachedInputMicrosPerMTok: 192_000,
		CacheWriteMicrosPerMTok:  2_400_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 14_400_000,
			CachedInputMicrosPerMTok: 384_000,
			CacheWriteMicrosPerMTok:  4_800_000,
		},
		Description: "large - hard coding and agent work; replaced by GPT-6.1 Sol",
	}, {
		ID: "gpt-6-luna", DisplayName: "GPT-6 Luna",
		MaxContext: 1_050_000, ReleaseDate: "2026-09-22",
		InputMicrosPerMTok: 96_000, OutputMicrosPerMTok: 480_000,
		CachedInputMicrosPerMTok: 9_600,
		CacheWriteMicrosPerMTok:  120_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 192_000, OutputMicrosPerMTok: 720_000,
			CachedInputMicrosPerMTok: 19_200,
			CacheWriteMicrosPerMTok:  240_000,
		},
		Description: "small and fast - short, simple, high-volume tasks",
	}, {
		ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol",
		MaxContext: 1_050_000, ReleaseDate: "2026-07-09",
		InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 19_200_000,
		CachedInputMicrosPerMTok: 384_000,
		CacheWriteMicrosPerMTok:  4_800_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 7_680_000, OutputMicrosPerMTok: 28_800_000,
			CachedInputMicrosPerMTok: 768_000,
			CacheWriteMicrosPerMTok:  9_600_000,
		},
		Description: "large - hard work; replaced by GPT-6 Sol",
	}, {
		ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra",
		MaxContext: 1_050_000, ReleaseDate: "2026-07-09",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 11_520_000,
		CachedInputMicrosPerMTok: 192_000,
		CacheWriteMicrosPerMTok:  2_400_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 17_280_000,
			CachedInputMicrosPerMTok: 384_000,
			CacheWriteMicrosPerMTok:  4_800_000,
		},
		Description: "mid-size and quick - everyday coding and questions; replaced by GPT-6.1 Terra",
	}, {
		ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna",
		MaxContext: 1_050_000, ReleaseDate: "2026-07-09",
		InputMicrosPerMTok: 192_000, OutputMicrosPerMTok: 1_152_000,
		CachedInputMicrosPerMTok: 19_200,
		CacheWriteMicrosPerMTok:  240_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 384_000, OutputMicrosPerMTok: 1_728_000,
			CachedInputMicrosPerMTok: 38_400,
			CacheWriteMicrosPerMTok:  480_000,
		},
		Description: "small and fast - short, simple tasks; replaced by GPT-6 Luna",
	}, {
		ID: "gpt-5.5", DisplayName: "GPT-5.5",
		MaxContext: 1_050_000, ReleaseDate: "2026-04-24",
		InputMicrosPerMTok: 4_800_000, OutputMicrosPerMTok: 28_800_000,
		CachedInputMicrosPerMTok: 480_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 9_600_000, OutputMicrosPerMTok: 43_200_000,
			CachedInputMicrosPerMTok: 960_000,
		},
		Description: "large and slow - thorough on hard problems",
	}, {
		ID: "gpt-5.4", DisplayName: "GPT-5.4",
		MaxContext: 1_050_000, ReleaseDate: "2026-03-05",
		InputMicrosPerMTok: 2_400_000, OutputMicrosPerMTok: 14_400_000,
		CachedInputMicrosPerMTok: 240_000,
		LongPrompt: &policy.PriceTier{
			AboveTokens:        272_000,
			InputMicrosPerMTok: 4_800_000, OutputMicrosPerMTok: 21_600_000,
			CachedInputMicrosPerMTok: 480_000,
		},
		Description: "mid-size - everyday work",
	}, {
		ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 mini",
		MaxContext: 400_000, ReleaseDate: "2026-03-17",
		InputMicrosPerMTok: 720_000, OutputMicrosPerMTok: 4_320_000,
		CachedInputMicrosPerMTok: 72_000,
		Description:              "small and quick - light reasoning",
	}, {
		ID: "gpt-5.2", DisplayName: "GPT-5.2",
		MaxContext: 400_000, ReleaseDate: "2025-12-11",
		InputMicrosPerMTok: 1_680_000, OutputMicrosPerMTok: 13_440_000,
		CachedInputMicrosPerMTok: 168_000,
		Description:              "mid-size and slow - reasons before it answers",
	}, {
		// Priced per minute of audio, so it reports its length. OpenAI
		// publishes no limit for its prompt: 16k is what its GPT-4o
		// transcribers took.
		ID: "gpt-transcribe", DisplayName: "GPT-Transcribe",
		Kind: policy.KindTranscription, MaxContext: 16_000, ReleaseDate: "2026-07-28",
		AudioMicrosPerMinute: 4_320,
		Description:          "speech to text in many languages - takes keywords for names and jargon",
	}},
}, {
	Name:    "google",
	Summary: "Google's own Gemini models, on the Gemini API.",
	// The Gemini API's OpenAI-compatible surface. Gemini caches prompts
	// without charging for the write. Gemini 4 Carbon and Argon hold the
	// prices from 2026-12-01, twice those before, so a budget never runs over.
	// Gemini 3.8 and 3.6 Flash's prices double on 2027-01-01.
	Endpoint:    "https://generativelanguage.googleapis.com/v1beta/openai",
	CachedInput: true,
	Location:    "usa",
	// Every chat model of Google reads images and calls tools.
	ChatCapabilities: readsImagesCallsTools,
	Models: []ProviderModel{{
		ID: "gemini-4-neon", DisplayName: "Gemini 4 Neon",
		MaxContext: 1_048_576, ReleaseDate: "2026-11-18",
		InputMicrosPerMTok: 2_880_000, OutputMicrosPerMTok: 14_400_000,
		CachedInputMicrosPerMTok: 144_000,
		Description:              "frontier-level and fast - complex software engineering, knowledge work and science",
	}, {
		ID: "gemini-4-hydrogen", DisplayName: "Gemini 4 Hydrogen",
		MaxContext: 1_048_576, ReleaseDate: "2026-11-12",
		InputMicrosPerMTok: 1_440_000, OutputMicrosPerMTok: 7_200_000,
		CachedInputMicrosPerMTok: 72_000,
		Description:              "large and fast - everyday coding and quick agent steps",
	}, {
		ID: "gemini-4-carbon", DisplayName: "Gemini 4 Carbon",
		MaxContext: 1_048_576, ReleaseDate: "2026-11-10",
		InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 19_200_000,
		CachedInputMicrosPerMTok: 192_000,
		Description:              "flagship, thinks longest - the hardest coding, agent and science work",
	}, {
		ID: "gemini-4-argon", DisplayName: "Gemini 4 Argon",
		MaxContext: 1_048_576, ReleaseDate: "2026-10-13",
		InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 19_200_000,
		CachedInputMicrosPerMTok: 192_000,
		Description:              "large and efficient - most coding, agent and knowledge work",
	}, {
		ID: "gemini-3.8-flash", DisplayName: "Gemini 3.8 Flash",
		MaxContext: 1_048_576, ReleaseDate: "2026-09-02",
		InputMicrosPerMTok: 720_000, OutputMicrosPerMTok: 3_600_000,
		CachedInputMicrosPerMTok: 72_000,
		Description:              "mid-size and quick - long-running coding and agent tasks",
	}, {
		ID: "gemini-3.5-flash-lite", DisplayName: "Gemini 3.5 Flash-Lite",
		MaxContext: 1_048_576, ReleaseDate: "2026-07-21",
		InputMicrosPerMTok: 288_000, OutputMicrosPerMTok: 2_400_000,
		CachedInputMicrosPerMTok: 28_800,
		Description:              "small and fast - high-volume agent tasks, translation and data processing",
	}, {
		ID: "gemini-3.1-pro-preview", DisplayName: "Gemini 3.1 Pro Preview",
		MaxContext: 1_048_576, ReleaseDate: "2026-02-19",
		InputMicrosPerMTok: 1_920_000, OutputMicrosPerMTok: 11_520_000,
		CachedInputMicrosPerMTok: 192_000,
		// Google charges more for a prompt over 200k tokens.
		LongPrompt: &policy.PriceTier{
			AboveTokens:        200_000,
			InputMicrosPerMTok: 3_840_000, OutputMicrosPerMTok: 17_280_000,
			CachedInputMicrosPerMTok: 384_000,
		},
		Description: "large, in preview - hard coding and agent work",
	}, {
		ID: "gemini-3.6-flash", DisplayName: "Gemini 3.6 Flash",
		MaxContext: 1_048_576, ReleaseDate: "2026-07-21",
		InputMicrosPerMTok: 720_000, OutputMicrosPerMTok: 3_600_000,
		CachedInputMicrosPerMTok: 72_000,
		Description:              "mid-size and quick - everyday agent tasks; replaced by Gemini 3.8 Flash",
	}, {
		// Google lists no cache price for it. Its image, audio and video rates
		// are left out: the OpenAI-compatible route is shown with text only.
		ID: "gemini-embedding-2", DisplayName: "Gemini Embedding 2",
		Kind: policy.KindEmbedding, MaxContext: 8_192, ReleaseDate: "2026-03-10",
		InputMicrosPerMTok: 192_000,
		Description:        "embedding, 3072 dimensions by default - search and RAG in over 100 languages",
	}},
}, {
	Name:    "deepseek",
	Summary: "DeepSeek's own models, on DeepSeek's API in China.",
	// DeepSeek charges half outside its peak hours. The table has one
	// price, so it holds the full one: a budget never runs over. deepseek-flash is served by V4.1 Flash, so it takes that date.
	// DeepSeek publishes no price for a cache write.
	Endpoint:    "https://api.deepseek.com",
	CachedInput: true,
	Location:    "cn",
	// Every chat model of DeepSeek calls tools, as its model pages say.
	ChatCapabilities: callsTools,
	Models: []ProviderModel{{
		ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro",
		MaxContext: 1_000_000, ReleaseDate: "2026-08-13",
		InputMicrosPerMTok: 1_267_200, OutputMicrosPerMTok: 3_801_600,
		CachedInputMicrosPerMTok: 42_240,
		Description:              "very large, with reasoning - hard coding and long agent sessions",
	}, {
		ID: "deepseek-flash", DisplayName: "DeepSeek V4.1 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-10-17",
		InputMicrosPerMTok: 288_000, OutputMicrosPerMTok: 1_152_000,
		CachedInputMicrosPerMTok: 5_760,
		Description:              "large and quick, reads images - coding, agent work and long documents",
		Capabilities:             readsImagesCallsTools,
	}},
}, {
	Name:    "mistral",
	Summary: "Mistral's own models and some open-weight ones, on Mistral's API in the EU.",
	// The models and prices are those on Mistral's pricing page,
	// https://docs.mistral.ai/inference/pricing: a model not listed there is
	// dropped. Each model's page there gives its API id, release date and
	// context. Mistral publishes no price for a cache write.
	Endpoint:    "https://api.mistral.ai/v1",
	CachedInput: true,
	Location:    "eu",
	// Every chat model of Mistral calls tools, as its model pages say.
	ChatCapabilities: callsTools,
	Models: []ProviderModel{{
		ID: "mistral-large-4", DisplayName: "Mistral Large 4",
		MaxContext: 1_000_000, ReleaseDate: "2026-10-06",
		InputMicrosPerMTok: 672_000, OutputMicrosPerMTok: 2_016_000,
		CachedInputMicrosPerMTok: 67_200,
		Description:              "very large - hard coding, agent and reasoning work, with images",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "mistral-small-2603", DisplayName: "Mistral Small 4",
		MaxContext: 262_144, ReleaseDate: "2026-03-16",
		InputMicrosPerMTok: 144_000, OutputMicrosPerMTok: 576_000,
		CachedInputMicrosPerMTok: 14_400,
		Description:              "small and fast, with reasoning - everyday chat, coding and agent tasks",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "codestral-2508", DisplayName: "Codestral 25.08",
		MaxContext: 128_000, ReleaseDate: "2025-07-30",
		InputMicrosPerMTok: 288_000, OutputMicrosPerMTok: 864_000,
		CachedInputMicrosPerMTok: 28_800,
		Description:              "small, made for code - quick code generation",
	}, {
		ID: "zai-glm-5-3", DisplayName: "GLM-5.3",
		MaxContext: 1_000_000, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 1_344_000, OutputMicrosPerMTok: 4_224_000,
		CachedInputMicrosPerMTok: 134_400,
		Description:              "large - hard coding, agent and security work over very long inputs",
	}, {
		ID: "zai-glm-5-3-flash", DisplayName: "GLM-5.3 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 144_000, OutputMicrosPerMTok: 480_000,
		CachedInputMicrosPerMTok: 28_800,
		Description:              "mid-size, quick all-rounder - code, reasoning and tool use",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "deepseek-v4-1-flash-1017", DisplayName: "DeepSeek V4.1 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-10-17",
		InputMicrosPerMTok: 288_000, OutputMicrosPerMTok: 1_152_000,
		CachedInputMicrosPerMTok: 6_720,
		Description:              "large and quick, in preview - large codebases and long documents",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "ministral-14b-2512", DisplayName: "Ministral 3 14B",
		MaxContext: 262_144, ReleaseDate: "2025-12-02",
		InputMicrosPerMTok: 192_000, OutputMicrosPerMTok: 192_000,
		CachedInputMicrosPerMTok: 19_200,
		Description:              "small - simple chat, text and image tasks",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "ministral-8b-2512", DisplayName: "Ministral 3 8B",
		MaxContext: 262_144, ReleaseDate: "2025-12-02",
		InputMicrosPerMTok: 144_000, OutputMicrosPerMTok: 144_000,
		CachedInputMicrosPerMTok: 14_400,
		Description:              "very small - short, high-volume tasks",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "ministral-3b-2512", DisplayName: "Ministral 3 3B",
		MaxContext: 262_144, ReleaseDate: "2025-12-02",
		InputMicrosPerMTok: 96_000, OutputMicrosPerMTok: 96_000,
		CachedInputMicrosPerMTok: 9_600,
		Description:              "tiny - classification, extraction and other simple tasks",
		Capabilities:             readsImagesCallsTools,
	}, {
		// The Voxtral transcribers are priced per minute of audio and report
		// it as prompt_audio_seconds. Mistral's cheaper rate for cached audio
		// is not used: every minute is billed at the full rate. The realtime
		// model is left out: it only streams.
		ID: "voxtral-mini-transcribe-3-0", DisplayName: "Voxtral Mini Transcribe 3",
		Kind: policy.KindTranscription, MaxContext: 32_768, ReleaseDate: "2026-10-20",
		AudioMicrosPerMinute: 3_360,
		Description:          "small speech to text with speaker labels - meetings, calls and notes",
	}, {
		ID: "voxtral-small-transcribe-3-0", DisplayName: "Voxtral Small Transcribe 3",
		Kind: policy.KindTranscription, MaxContext: 32_768, ReleaseDate: "2026-10-20",
		AudioMicrosPerMinute: 3_840,
		Description:          "speech to text with speaker labels - hard audio, many speakers and languages",
	}, {
		ID: "mistral-embed", Snapshot: "mistral-embed-2312", DisplayName: "Mistral Embed",
		Kind: policy.KindEmbedding, MaxContext: 8_192, ReleaseDate: "2023-12-11",
		InputMicrosPerMTok: 96_000, CachedInputMicrosPerMTok: 9_600,
		Description: "embedding, 1024 dimensions - search and RAG over text",
	}, {
		ID: "codestral-embed", Snapshot: "codestral-embed-2505", DisplayName: "Codestral Embed",
		Kind: policy.KindEmbedding, MaxContext: 8_192, ReleaseDate: "2025-05-28",
		InputMicrosPerMTok: 144_000, CachedInputMicrosPerMTok: 14_400,
		Description: "code embedding, 1536 dimensions by default - code search and retrieval",
	}},
}, {
	Name:    "peakprivacy",
	Summary: "Swiss and EU models, on PeakPrivacy's API in Switzerland.",
	// The OpenAI-compatible path is /chat/completions. PeakPrivacy resells
	// models served in Switzerland and the EU, so each model says where it
	// runs. The models it resells from the USA are left out: the catalogue
	// has them from their makers. Ids are PeakPrivacy's own and say no
	// version, so a model whose version PeakPrivacy does not name has no
	// release date. Prices are listed in CHF per thousand tokens. Context
	// windows are from PeakPrivacy's help centre. GPT Swiss and GPT EU are
	// both gpt-oss-120b; deepseek-r1 is DeepSeek V4 Pro and deepseek-v4-flash
	// is DeepSeek V4.1 Flash.
	Endpoint: "https://api.peakprivacy.ch/v1/ai",
	Location: "ch",
	Models: []ProviderModel{{
		ID: "gpt-swiss", DisplayName: "GPT Swiss",
		MaxContext: 131_000, ReleaseDate: "2025-08-05",
		InputMicrosPerMTok: 20_000_000, OutputMicrosPerMTok: 60_000_000,
		Description: "mid-size, reasons before it answers - everyday work and tool use",
	}, {
		ID: "mistral-small-swiss", DisplayName: "Mistral Small Swiss",
		MaxContext:         30_000,
		InputMicrosPerMTok: 150_000, OutputMicrosPerMTok: 450_000,
		Description:  "small and fast - translation, summaries and simple chat",
		Capabilities: callsTools,
	}, {
		ID: "glm5", DisplayName: "GLM-5.2",
		MaxContext: 130_000, ReleaseDate: "2026-06-16",
		InputMicrosPerMTok: 2_154_000, OutputMicrosPerMTok: 6_772_000,
		Description:  "large - long agent sessions, hard code and terminal work",
		Capabilities: callsTools,
	}, {
		ID: "deepseek-v4-flash", DisplayName: "DeepSeek V4.1 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-10-17",
		InputMicrosPerMTok: 375_000, OutputMicrosPerMTok: 1_500_000,
		Description:  "large and quick, in preview - large codebases and long documents",
		Capabilities: callsTools,
	}, {
		ID: "gpt-eu", DisplayName: "GPT EU",
		MaxContext: 124_000, ReleaseDate: "2025-08-05", Location: "eu",
		InputMicrosPerMTok: 375_000, OutputMicrosPerMTok: 1_500_000,
		Description:  "mid-size reasoning model - general questions and tool use",
		Capabilities: callsTools,
	}, {
		ID: "deepseek-r1", DisplayName: "DeepSeek V4 Pro",
		MaxContext: 128_000, ReleaseDate: "2026-08-13", Location: "eu",
		InputMicrosPerMTok: 2_625_000, OutputMicrosPerMTok: 5_250_000,
		Description:  "very large, with reasoning - hard coding and long agent sessions",
		Capabilities: callsTools,
	}, {
		ID: "gemma-3", DisplayName: "Gemma 3 27B",
		MaxContext: 128_000, ReleaseDate: "2025-03-12", Location: "eu",
		InputMicrosPerMTok: 150_000, OutputMicrosPerMTok: 450_000,
		Description:  "mid-size and quick - documents and answers from search results",
		Capabilities: readsImages,
	}, {
		// PeakPrivacy says Qwen3 runs in Europe in one place and in
		// Switzerland in another, and does not mark it Swiss in its price list.
		ID: "qwen-3-fast", DisplayName: "Qwen3 Fast",
		MaxContext: 262_000, Location: "eu",
		InputMicrosPerMTok: 450_000, OutputMicrosPerMTok: 1_350_000,
		Description:  "mid-size and quick - everyday questions in many languages",
		Capabilities: callsTools,
	}, {
		ID: "qwen-3", DisplayName: "Qwen3",
		MaxContext: 256_000, Location: "eu",
		InputMicrosPerMTok: 300_000, OutputMicrosPerMTok: 900_000,
		Description:  "mid-size, with a thinking mode - reasoning in many languages",
		Capabilities: callsTools,
	}},
}, {
	Name:    "infomaniak",
	Summary: "Open-weight models, served in Switzerland.",
	// The address carries the product id of your AI Service.
	Endpoint:       "https://api.infomaniak.com/2/ai/{product_id}/openai/v1",
	NeedsProductID: true,
	// Infomaniak reranks on its Cohere-shaped API only, at /cohere/v2/rerank.
	Endpoints: map[policy.Kind]string{
		policy.KindRerank: "https://api.infomaniak.com/2/ai/{product_id}/cohere/v2",
	},
	Location: "ch",
	// Ids are the upstream names, capitals and all, as the endpoint expects.
	// Prices are Infomaniak's list price times 1.12, on purpose. An embedding
	// or a rerank writes nothing, so it has no output price. Capabilities are
	// the "Modality" and "Function call" of each model on its open-source
	// models page; its FAQ still says reading images is being looked at.
	Models: []ProviderModel{{
		ID: "Qwen/Qwen3.5-397B-A17B-FP8", DisplayName: "Qwen3.5 397B A17B FP8",
		MaxContext: 200_000, ReleaseDate: "2026-02-16",
		InputMicrosPerMTok: 896_000, OutputMicrosPerMTok: 4_032_000,
		Description:  "large - hard reasoning, planning and tool use",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "moonshotai/Kimi-K2.6", DisplayName: "Kimi K2.6",
		MaxContext: 256_000, ReleaseDate: "2026-04-20",
		InputMicrosPerMTok: 672_000, OutputMicrosPerMTok: 3_360_000,
		Description:  "large - coding and agent work",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "mistralai/Mistral-Small-4-119B-2603", DisplayName: "Mistral Small 4",
		MaxContext: 256_000, ReleaseDate: "2026-03-16",
		InputMicrosPerMTok: 224_000, OutputMicrosPerMTok: 840_000,
		Description:  "mid-size - plain instructions or longer reasoning",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B", DisplayName: "Apertus v1.5 70B",
		MaxContext: 100_000, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 784_000, OutputMicrosPerMTok: 2_800_000,
		Description:  "mid-size, open training data - work that must be auditable",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "google/gemma-4-31B-it", DisplayName: "Gemma 4 31B",
		MaxContext: 100_000, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 224_000, OutputMicrosPerMTok: 448_000,
		Description:  "mid-size and quick - documents, chatbots and ordinary code",
		Capabilities: callsTools,
	}, {
		ID: "Qwen/Qwen3.8-27B-FP8", DisplayName: "Qwen3.8 27B FP8",
		MaxContext: 100_000, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 448_000, OutputMicrosPerMTok: 3_360_000,
		Description:  "small but strong for its size - ordinary code and questions",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "mistralai/Ministral-3-14B-Instruct-2512", DisplayName: "Ministral 3 14B",
		MaxContext: 100_000, ReleaseDate: "2025-12-02",
		InputMicrosPerMTok: 336_000, OutputMicrosPerMTok: 448_000,
		Description:  "small and quick - chatbots, short edits and everyday questions",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "Qwen/Qwen3-Embedding-8B", DisplayName: "Qwen3 Embedding 8B",
		Kind: policy.KindEmbedding, MaxContext: 8_192, ReleaseDate: "2025-06-05",
		InputMicrosPerMTok: 78_400,
		Description:        "embedding, 4096 dimensions - search and RAG in over 100 languages",
	}, {
		// Infomaniak's own id, not the upstream BAAI/bge-multilingual-gemma2.
		ID: "bge_multilingual_gemma2", DisplayName: "BGE Multilingual Gemma2",
		Kind: policy.KindEmbedding, MaxContext: 8_000,
		InputMicrosPerMTok: 72_800,
		Description:        "embedding, 3584 dimensions - semantic search and RAG in many languages",
	}, {
		// Infomaniak's own id for all-MiniLM-L12-v2. Infomaniak serves it free;
		// a small price keeps it inside budgets.
		ID: "mini_lm_l12_v2", DisplayName: "all-MiniLM-L12-v2",
		Kind: policy.KindEmbedding, MaxContext: 128,
		InputMicrosPerMTok: 10_000,
		Description:        "tiny embedding model, 384 dimensions - fast indexing and prototypes",
	}, {
		ID: "BAAI/bge-reranker-v2-m3", DisplayName: "BGE Reranker v2 M3",
		Kind: policy.KindRerank, MaxContext: 8_192, ReleaseDate: "2024-03-18",
		InputMicrosPerMTok: 11_200,
		Description:        "reranker - orders search results by relevance, in many languages",
	}, {
		ID: "Qwen/Qwen3-Reranker-0.6B", DisplayName: "Qwen3 Reranker 0.6B",
		Kind: policy.KindRerank, MaxContext: 32_768, ReleaseDate: "2025-06-05",
		InputMicrosPerMTok: 10_080,
		Description:        "small reranker - orders search results by relevance in over 100 languages",
	}},
}, {
	Name:     "stepping-stone",
	Summary:  "Open-weight models, served in Switzerland.",
	Endpoint: "https://llm.stoney-cloud.com/v1",
	Location: "ch",
	// Ids are the upstream names, capitals and all. The Nemotron id is spelled
	// as stepping stone's documentation spells it. Contexts are the max_model_len
	// its wiki lists. An embedding or a rerank writes nothing, so it has no
	// output price.
	Models: []ProviderModel{{
		ID: "MiniMaxAI/MiniMax-M2.5", DisplayName: "MiniMax M2.5",
		MaxContext: 196_608, ReleaseDate: "2026-02-12",
		InputMicrosPerMTok: 1_940_000, OutputMicrosPerMTok: 9_700_000,
		Description:  "large - long coding and agent tasks with many steps",
		Capabilities: callsTools,
	}, {
		ID: "NVIDIA/NVIDIA-Nemotron-3-Super-120B-A12B", DisplayName: "Nemotron 3 Super",
		MaxContext: 131_072, ReleaseDate: "2026-03-11",
		InputMicrosPerMTok: 2_000_000, OutputMicrosPerMTok: 5_000_000,
		Description:  "large reasoning model - hard problems",
		Capabilities: callsTools,
	}, {
		ID: "deepseek-ai/DeepSeek-V4-Flash-0731", DisplayName: "DeepSeek V4 Flash",
		MaxContext: 524_288, ReleaseDate: "2026-07-31",
		InputMicrosPerMTok: 450_000, OutputMicrosPerMTok: 1_800_000,
		Description:  "mid-size and quick - large codebases and long documents",
		Capabilities: callsTools,
	}, {
		ID: "Qwen/Qwen3-Coder-Next", DisplayName: "Qwen3 Coder Next",
		MaxContext: 262_144, ReleaseDate: "2026-02-03",
		InputMicrosPerMTok: 340_000, OutputMicrosPerMTok: 1_700_000,
		Description: "made for code - edits, code questions and agent tool calls",
	}, {
		ID: "zai-org/GLM-5.3-Flash", DisplayName: "GLM-5.3 Flash",
		MaxContext: 262_144, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 300_000, OutputMicrosPerMTok: 1_500_000,
		Description:  "mid-size, quick all-rounder - code, reasoning and tool use",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "openai/gpt-oss-120b", DisplayName: "gpt-oss-120b",
		MaxContext: 131_072, ReleaseDate: "2025-08-05",
		InputMicrosPerMTok: 400_000, OutputMicrosPerMTok: 1_600_000,
		Description:  "mid-size, reasons before it answers - everyday work and tool use",
		Capabilities: callsTools,
	}, {
		// stepping stone names it for "multimodal tasks"; its tool calls are
		// stated only for the Qwen family, so they are not marked.
		ID: "Qwen/Qwen3.8-27B", DisplayName: "Qwen3.8 27B",
		MaxContext: 262_144, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 1_500_000,
		Description:  "small but strong for its size - ordinary code and questions",
		Capabilities: readsImages,
	}, {
		ID: "google/gemma-4-31B-it", DisplayName: "Gemma 4 31B",
		MaxContext: 163_840, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 250_000, OutputMicrosPerMTok: 1_000_000,
		Description:  "mid-size and quick - documents, chatbots and ordinary code",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "Qwen/Qwen3.5-35B-A3B-FP8", DisplayName: "Qwen3.5 35B A3B FP8",
		MaxContext: 131_072, ReleaseDate: "2026-02-24",
		InputMicrosPerMTok: 170_000, OutputMicrosPerMTok: 1_000_000,
		Description: "small and fast, light on reasoning - high-volume, simple tasks",
	}, {
		ID: "apertus-ai/Apertus-v1.5-8B", DisplayName: "Apertus v1.5 8B",
		MaxContext: 131_072, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 100_000,
		Description:  "small, open training data - simple tasks",
		Capabilities: callsTools,
	}, {
		ID: "allenai/olmOCR-2-7B", DisplayName: "olmOCR 2",
		MaxContext: 8_192, ReleaseDate: "2025-10-22",
		InputMicrosPerMTok: 60_000, OutputMicrosPerMTok: 290_000,
		Description:  "OCR - turns images of pages into text; not for chat or code",
		Capabilities: readsImages,
	}, {
		ID: "lightonai/LightOnOCR-2-1B", DisplayName: "LightOnOCR 2",
		MaxContext: 16_384, ReleaseDate: "2026-01-19",
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 60_000,
		Description:  "small, fast OCR - turns images of pages into text; not for chat or code",
		Capabilities: readsImages,
	}, {
		ID: "opendatalab/MinerU2.5-2509-1.2B", DisplayName: "MinerU 2.5",
		MaxContext: 16_384, ReleaseDate: "2025-09-19",
		InputMicrosPerMTok: 20_000, OutputMicrosPerMTok: 60_000,
		Description:  "document parsing - turns images of pages into text, tables and formulas; not for chat",
		Capabilities: readsImages,
	}, {
		ID: "BAAI/bge-m3", DisplayName: "BGE M3",
		Kind: policy.KindEmbedding, MaxContext: 8_192, ReleaseDate: "2024-01-30",
		InputMicrosPerMTok: 100_000,
		Description:        "embedding, 1024 dimensions - search and RAG in over 100 languages",
	}, {
		ID: "BAAI/bge-reranker-v2-m3", DisplayName: "BGE Reranker v2 M3",
		Kind: policy.KindRerank, MaxContext: 8_192, ReleaseDate: "2024-03-18",
		InputMicrosPerMTok: 100_000,
		Description:        "reranker - orders search results by relevance, in many languages",
	}, {
		// stepping stone prices audio by the second; this is the price of a
		// minute.
		ID: "openai/whisper-large-v3", DisplayName: "Whisper Large v3",
		Kind: policy.KindTranscription, MaxContext: 224, ReleaseDate: "2023-11-06",
		AudioMicrosPerMinute: 120_000,
		Description:          "speech to text in many languages, with timestamps and subtitles",
	}, {
		ID: "mistralai/Voxtral-Mini-3B-2507", DisplayName: "Voxtral Mini 3B",
		Kind: policy.KindTranscription, MaxContext: 32_768, ReleaseDate: "2025-07-15",
		AudioMicrosPerMinute: 12_000,
		Description:          "small speech to text in eight languages - meetings, calls and notes",
	}},
}, {
	Name:     "phoeniqs",
	Summary:  "Open-weight models, served in Switzerland.",
	Endpoint: "https://maas.phoeniqs.com/v1",
	Location: "ch",
	// Ids are Phoeniqs's own names, kept across minor upgrades. Release dates
	// are of the models served on 2026-10-21: GLM-5.2 for inference-glm5,
	// GLM-5.3-Flash for inference-glm5-flash and its -compact, and
	// MinerU2.5-Pro-2605 for inference-miner-u25. Its rerank models are left
	// out: Phoeniqs does not say which rerank API it speaks. An embedding
	// writes nothing, so it has no output price.
	Models: []ProviderModel{{
		// Phoeniqs names it and Qwen3 VL 235B as the models that work in
		// Cursor's agent mode, which calls tools.
		ID: "inference-glm5", DisplayName: "GLM-5",
		MaxContext: 131_072, ReleaseDate: "2026-06-16",
		InputMicrosPerMTok: 1_077_000, OutputMicrosPerMTok: 3_386_000,
		Description:  "large - long agent sessions, hard code and terminal work",
		Capabilities: callsTools,
	}, {
		ID: "inference-qwen3-vl-235b", DisplayName: "Qwen3 VL 235B",
		MaxContext: 128_000, ReleaseDate: "2025-09-23",
		InputMicrosPerMTok: 700_300, OutputMicrosPerMTok: 2_000_000,
		Description:  "large, reads images - screenshots, diagrams and scans",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "inference-deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-07-31",
		InputMicrosPerMTok: 146_200, OutputMicrosPerMTok: 392_500,
		Description: "mid-size and quick - large codebases and long documents",
	}, {
		ID: "inference-glm5-flash", DisplayName: "GLM-5 Flash",
		MaxContext: 1_000_000, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 123_000, OutputMicrosPerMTok: 410_000,
		Description:  "mid-size, quick all-rounder - code, reasoning and tool use",
		Capabilities: callsTools,
	}, {
		// The same model as GLM-5 Flash, so it calls tools too.
		ID: "inference-glm5-flash-compact", DisplayName: "GLM-5 Flash Compact",
		MaxContext: 524_288, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 62_500, OutputMicrosPerMTok: 416_700,
		Description:  "GLM-5 Flash with a smaller context - quicker and cheaper",
		Capabilities: callsTools,
	}, {
		ID: "inference-apertus-v15-70b", DisplayName: "Apertus v1.5 70B",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 619_500, OutputMicrosPerMTok: 2_220_100,
		Description:  "mid-size, open training data - work that must be auditable",
		Capabilities: readsImagesCallsTools,
	}, {
		ID: "inference-gpt-oss-120b", DisplayName: "gpt-oss-120b",
		MaxContext: 131_072, ReleaseDate: "2025-08-05",
		InputMicrosPerMTok: 115_400, OutputMicrosPerMTok: 461_700,
		Description: "mid-size, reasons before it answers - everyday work and tool use",
	}, {
		ID: "inference-llama4-maverick", DisplayName: "Llama 4 Maverick",
		MaxContext: 1_048_576, ReleaseDate: "2025-04-05",
		InputMicrosPerMTok: 269_300, OutputMicrosPerMTok: 1_077_300,
		Description:  "mid-size, reads up to four images a prompt - long documents",
		Capabilities: readsImages,
	}, {
		ID: "inference-gemma4-31b", DisplayName: "Gemma 4 31B",
		MaxContext: 131_072, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 118_000, OutputMicrosPerMTok: 325_000,
		Description:  "mid-size and quick, reads images - documents, chatbots and ordinary code",
		Capabilities: readsImages,
	}, {
		ID: "inference-llama4-scout-17b", DisplayName: "Llama 4 Scout",
		MaxContext: 62_256, ReleaseDate: "2025-04-05",
		InputMicrosPerMTok: 192_400, OutputMicrosPerMTok: 638_700,
		Description:  "small and quick, reads images - short inputs",
		Capabilities: readsImages,
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
		Description:  "tiny vision model - describes images and reads charts; not for chat",
		Capabilities: readsImages,
	}, {
		// Phoeniqs names no upstream model. Its description is the title of the
		// DeepSeek-OCR paper, so it is DeepSeek-OCR, not DeepSeek-OCR 2.
		ID: "inference-deepseek-ocr", DisplayName: "DeepSeek-OCR",
		MaxContext: 8_192, ReleaseDate: "2025-10-20",
		InputMicrosPerMTok: 384_800, OutputMicrosPerMTok: 1_539_100,
		Description:  "OCR - turns images of pages and receipts into text; not for chat or code",
		Capabilities: readsImages,
	}, {
		ID: "inference-miner-u25", DisplayName: "MinerU 2.5",
		MaxContext: 8_192, ReleaseDate: "2026-05-21",
		InputMicrosPerMTok: 538_500, OutputMicrosPerMTok: 76_900,
		Description:  "document parsing - turns images of pages into text, tables and formulas; not for chat",
		Capabilities: readsImages,
	}, {
		ID: "inference-bge-m3", DisplayName: "BGE M3",
		Kind: policy.KindEmbedding, MaxContext: 8_192, ReleaseDate: "2024-01-30",
		InputMicrosPerMTok: 430_900,
		Description:        "embedding, 1024 dimensions - search and RAG in over 100 languages",
	}, {
		ID: "inference-qwen3-embedding-8b", DisplayName: "Qwen3 Embedding 8B",
		Kind: policy.KindEmbedding, MaxContext: 32_768, ReleaseDate: "2025-06-05",
		InputMicrosPerMTok: 54_000,
		Description:        "embedding, 4096 dimensions - search and RAG in over 100 languages",
	}, {
		ID: "inference-granite-emb-278m", DisplayName: "Granite Embedding 278M",
		Kind: policy.KindEmbedding, MaxContext: 512, ReleaseDate: "2024-12-18",
		InputMicrosPerMTok: 77_000,
		Description:        "small embedding model, 768 dimensions - short passages in many languages",
	}, {
		ID: "inference-whisper-large-v3", DisplayName: "Whisper Large v3",
		Kind: policy.KindTranscription, MaxContext: 224, ReleaseDate: "2023-11-06",
		AudioMicrosPerMinute: 6_000,
		Description:          "speech to text in many languages, with timestamps and subtitles",
	}, {
		// Whisper Large v3 trained on Swiss German, which it writes as Standard
		// German.
		ID: "inference-flix-swissgerman-full", DisplayName: "Flix Swiss German",
		Kind: policy.KindTranscription, MaxContext: 224,
		AudioMicrosPerMinute: 6_000,
		Description:          "speech to text for Swiss German, written as Standard German",
	}},
}, {
	Name:        "cscs",
	Summary:     "Open-weight models at the Swiss National Supercomputing Centre.",
	Endpoint:    "https://api.inference.cscs.ch/v1",
	CachedInput: true,
	Location:    "ch",
	// Ids are the upstream names, capitals and all. A -thinking id is the same
	// weights with reasoning on and tool calls off, so it keeps the date.
	Models: []ProviderModel{{
		ID: "zai-org/GLM-5.3", DisplayName: "GLM-5.3",
		MaxContext: 1_048_576, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 181_100, OutputMicrosPerMTok: 6_642_000,
		CachedInputMicrosPerMTok: 31_500,
		Description:              "large - hard coding, agent and security work over very long inputs",
	}, {
		ID: "zai-org/GLM-5.3-Flash", DisplayName: "GLM-5.3 Flash",
		MaxContext: 1_048_576, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 74_700, OutputMicrosPerMTok: 1_822_500,
		CachedInputMicrosPerMTok: 16_200,
		Description:              "mid-size, quick all-rounder - code, reasoning and tool use",
		Capabilities:             readsImagesCallsTools,
	}, {
		ID: "Qwen/Qwen3.8-Flash-Next-FP8", DisplayName: "Qwen3.8 Flash Next FP8",
		MaxContext: 262_144, ReleaseDate: "2026-08-26",
		InputMicrosPerMTok: 34_800, OutputMicrosPerMTok: 2_019_500,
		CachedInputMicrosPerMTok: 4_700,
		Description:              "mid-size and quick, reads images and video - long inputs and everyday questions",
		Capabilities:             readsImages,
	}, {
		ID: "google/gemma-4-31B-it", DisplayName: "Gemma 4 31B",
		MaxContext: 262_144, ReleaseDate: "2026-04-02",
		InputMicrosPerMTok: 68_000, OutputMicrosPerMTok: 420_000,
		CachedInputMicrosPerMTok: 13_600,
		Description:              "mid-size and quick, reads images - documents, chatbots and ordinary code",
		Capabilities:             readsImages,
	}, {
		ID: "Qwen/Qwen3.8-27B-FP8", DisplayName: "Qwen3.8 27B FP8",
		MaxContext: 262_144, ReleaseDate: "2026-08-14",
		InputMicrosPerMTok: 23_400, OutputMicrosPerMTok: 504_900,
		CachedInputMicrosPerMTok: 2_300,
		Description:              "small but strong for its size, reads images - ordinary code and questions",
		Capabilities:             readsImages,
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B", DisplayName: "Apertus v1.5 70B",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 194_600, OutputMicrosPerMTok: 392_800,
		CachedInputMicrosPerMTok: 19_500,
		Description:              "mid-size, open training data - work that must be auditable",
		Capabilities:             readsImages,
	}, {
		ID: "swiss-ai/Apertus-v1.5-70B-thinking", DisplayName: "Apertus v1.5 70B Thinking",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 194_600, OutputMicrosPerMTok: 392_800,
		CachedInputMicrosPerMTok: 19_500,
		Description:              "mid-size, open training data, reasons first, no tool calls - hard questions",
		Capabilities:             readsImages,
	}, {
		ID: "swiss-ai/Apertus-v1.5-8B", DisplayName: "Apertus v1.5 8B",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 18_500, OutputMicrosPerMTok: 40_400,
		CachedInputMicrosPerMTok: 1_900,
		Description:              "small, open training data - simple tasks",
		Capabilities:             readsImages,
	}, {
		ID: "swiss-ai/Apertus-v1.5-8B-thinking", DisplayName: "Apertus v1.5 8B Thinking",
		MaxContext: 262_144, ReleaseDate: "2026-07-24",
		InputMicrosPerMTok: 18_500, OutputMicrosPerMTok: 40_400,
		CachedInputMicrosPerMTok: 1_900,
		Description:              "small, open training data, reasons first, no tool calls - simple questions",
		Capabilities:             readsImages,
	}},
}}

// Providers returns a copy of the built-in table, so a caller cannot edit it.
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
	p.Kinds = p.kinds()
	p.Endpoints = maps.Clone(p.Endpoints)
	p.Models = slices.Clone(p.Models)
	for i := range p.Models {
		p.Models[i] = p.withCapabilities(p.Models[i])
		p.Models[i].LongPrompt = p.Models[i].longPrompt()
	}
	return p
}

// withCapabilities fills in the provider's capabilities for a chat model that
// states none, as a copy.
func (p Provider) withCapabilities(m ProviderModel) ProviderModel {
	if m.Capabilities == nil && m.kindOrChat() == policy.KindChat {
		m.Capabilities = p.ChatCapabilities
	}
	m.Capabilities = slices.Clone(m.Capabilities)
	return m
}

// EndpointFor is the base URL a kind is served at.
func (p Provider) EndpointFor(k policy.Kind) string {
	if e, ok := p.Endpoints[k]; ok {
		return e
	}
	return p.Endpoint
}

func provider(name string) (Provider, bool) {
	for _, p := range providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// LocationOf is where a model runs: its own location, or else the provider's.
func (p Provider) LocationOf(backendModel string) string {
	if m, ok := p.Model(backendModel); ok && m.Location != "" {
		return m.Location
	}
	return p.Location
}

// Locations are the places the provider's models run, the provider's first.
func (p Provider) Locations() []string {
	out := []string{p.Location}
	for _, m := range p.Models {
		if m.Location != "" && !slices.Contains(out, m.Location) {
			out = append(out, m.Location)
		}
	}
	return out
}

// Model finds one of the provider's models by its id or its dated id.
func (p Provider) Model(id string) (ProviderModel, bool) {
	for _, m := range p.Models {
		if m.ID == id || (m.Snapshot != "" && m.Snapshot == id) {
			return p.withCapabilities(m), true
		}
	}
	return ProviderModel{}, false
}

func (m ProviderModel) kindOrChat() policy.Kind {
	if m.Kind == "" {
		return policy.KindChat
	}
	return m.Kind
}

// longPrompt is a copy of the model's long-prompt prices, so the table stays
// unchanged.
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

// kinds are the APIs the provider's models are served on, in table order.
func (p Provider) kinds() []policy.Kind {
	var out []policy.Kind
	for _, m := range p.Models {
		if !slices.Contains(out, m.kindOrChat()) {
			out = append(out, m.kindOrChat())
		}
	}
	return out
}

func (p Provider) kindNames() string {
	return joinNames(p.kinds(), func(k policy.Kind) string { return string(k) })
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

// fillProductID writes the product id into every backend of the entry, which
// may include its own egress proxy, and checks that no placeholder is left.
func fillProductID(p Provider, backends []string, productID string) ([]string, error) {
	productID = strings.TrimSpace(productID)
	switch {
	case productID != "" && !p.NeedsProductID:
		return nil, fmt.Errorf("the %s provider serves everybody at the same address and "+
			"takes no product_id", p.Name)
	case productID != "" && !ValidProductID(productID):
		// It goes into a URL path, where a slash or space would change the
		// address.
		return nil, fmt.Errorf("product_id %q must be letters, digits, hyphens and "+
			"underscores: it is written into the endpoint", productID)
	}
	// A copy, because `keera model set` passes in a stored model's slice.
	filled := make([]string, len(backends))
	for i, b := range backends {
		filled[i] = b
		// Without an id the placeholder stays, for the check below to report.
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
// not state. The entry always wins, such as an egress proxy or its own prices.
func applyProvider(m Model) (Model, error) {
	p, found := provider(m.Provider)
	if !found {
		return m, fmt.Errorf("unknown provider %q; Keera Gateway knows: %s",
			m.Provider, ProviderNames())
	}
	known, isKnown := p.Model(m.BackendModel)
	kind := policy.Kind(m.Kind)
	if kind == "" && isKnown {
		kind = known.kindOrChat()
	}
	if len(m.Backends) == 0 {
		m.Backends = []string{p.EndpointFor(kind)}
	}
	backends, err := fillProductID(p, m.Backends, m.ProductID)
	if err != nil {
		return m, err
	}
	m.Backends = backends
	if m.Location == "" {
		m.Location = p.LocationOf(m.BackendModel)
	}
	if m.BackendModel == "" || !isKnown {
		return m, nil
	}
	return fillFromTable(m, known), nil
}

// Check refuses a model the provider cannot serve or this build cannot bill:
// a kind with no endpoint, or a model missing from the price table that states
// no prices. Files and the control API both run it.
func (p Provider) Check(kind policy.Kind, backendModel string, priced bool) error {
	if kind != "" && !slices.Contains(p.kinds(), kind) {
		return fmt.Errorf("provider %s serves only %s, not %q",
			p.Name, p.kindNames(), kind)
	}
	if backendModel == "" {
		// The caller's own check reports it, in terms of this provider.
		return nil
	}
	known, isKnown := p.Model(backendModel)
	if isKnown && kind != "" && kind != known.kindOrChat() {
		// The provider serves it on one API only; any other fails upstream.
		return fmt.Errorf("%s is a %s model at %s; set kind %s",
			backendModel, known.kindOrChat(), p.Name, known.kindOrChat())
	}
	// A model newer than this build must be priced by hand, or no budget
	// could hold it.
	if !isKnown && !priced {
		return fmt.Errorf("%s is not in the Keera Gateway price table for %s "+
			"(it knows: %s); "+
			"state input_micros_per_mtok and output_micros_per_mtok for it, or "+
			"audio_micros_per_minute for a transcription model - "+
			"0 to declare the model unbilled, which no budget can then hold",
			backendModel, p.Name, p.modelIDs())
	}
	return nil
}

// fillFromTable sets each value the entry left out to the table's.
func fillFromTable(m Model, known ProviderModel) Model {
	if m.Kind == "" {
		m.Kind = string(known.kindOrChat())
	}
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
	if m.AudioMicrosPerMinute == nil {
		m.AudioMicrosPerMinute = &known.AudioMicrosPerMinute
	}
	if m.Capabilities == nil {
		m.Capabilities = known.Capabilities
	}
	return m
}
