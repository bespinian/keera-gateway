package catalog

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestProviderFillsInEverythingButTheAlias(t *testing.T) {
	const in = `
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if got, want := m.Backends, []string{"https://api.anthropic.com/v1"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Backends = %v, want %v", got, want)
	}
	if m.MaxContext != 1_000_000 {
		t.Errorf("MaxContext = %d, want 1000000", m.MaxContext)
	}
	// Priced, because a model that costs nothing is a model no budget holds.
	if m.InputMicrosPerMTok == 0 || m.OutputMicrosPerMTok == 0 {
		t.Errorf("prices = %d/%d, want the provider's list price",
			m.InputMicrosPerMTok, m.OutputMicrosPerMTok)
	}
	if !m.Enabled || m.Kind != "chat" {
		t.Errorf("model = %+v, want an enabled chat alias", m)
	}
	// The name survives being applied. Every value above is now indistinguishable
	// from one somebody typed, and this is what still says where they came from.
	if m.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", m.Provider)
	}
}

func TestProviderDefaultsGiveWayToTheCatalogue(t *testing.T) {
	// A corporate egress proxy and a department's internal price. Neither
	// should be overwritten.
	const in = `
models:
  - alias: keera-frontier
    provider: ANTHROPIC
    backend_model: claude-opus-5
    backends: ["http://egress.corp:8080/v1"]
    max_context: 200000
    input_micros_per_mtok: 1
    output_micros_per_mtok: 0
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if len(m.Backends) != 1 || m.Backends[0] != "http://egress.corp:8080/v1" {
		t.Errorf("Backends = %v, want the declared proxy", m.Backends)
	}
	if m.MaxContext != 200_000 {
		t.Errorf("MaxContext = %d, want 200000", m.MaxContext)
	}
	// Overridden in every field, and still that provider's model: what the name
	// records is where the entry was declared against, not that nothing moved.
	// It is also folded to lower case, so a form matching on it matches.
	if m.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", m.Provider)
	}
	// A stated zero is a decision, not an absent value.
	if m.InputMicrosPerMTok != 1 || m.OutputMicrosPerMTok != 0 {
		t.Errorf("prices = %d/%d, want the declared 1/0",
			m.InputMicrosPerMTok, m.OutputMicrosPerMTok)
	}
}

// A model named through a provider arrives described, because a description is
// what a router decides between destinations on - see docs/routers.md - and the
// version of this where every provider model arrives without one is the version
// where nobody writes fourteen sentences and routers choose between bare
// aliases. It is a default and not an answer: an entry that writes its own
// keeps it.
func TestProviderDefaultsADescriptionAnEntryCanRewrite(t *testing.T) {
	const in = `
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
  - alias: keera-code
    provider: anthropic
    backend_model: claude-sonnet-5
    description: "  the alias every coding agent here is pointed at  "
  - alias: keera-quick
    provider: anthropic
    backend_model: claude-haiku-4-5
    description: "   "
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if models[0].Description == "" {
		t.Error("a provider model was described to nobody; a router offered it sees only its alias")
	}
	// The description of the model, not of the provider: two aliases from one
	// provider are two things a router has to be able to tell apart.
	if models[0].Description == models[2].Description {
		t.Errorf("claude-opus-5 and claude-haiku-4-5 share a description: %q",
			models[0].Description)
	}
	// Whoever knows what the alias is for wins over the table, as they do for
	// every other value it fills in.
	if got, want := models[1].Description,
		"the alias every coding agent here is pointed at"; got != want {
		t.Errorf("Description = %q, want the entry's own %q", got, want)
	}
	// A field holding nothing but spaces states nothing, so the default applies
	// rather than being written over with the whitespace.
	if models[2].Description == "" {
		t.Error("a description of spaces was taken for one somebody wrote")
	}
}

func TestProviderRejectsWhatWouldFailSilentlyAtRuntime(t *testing.T) {
	tests := []struct {
		name, in, wantErr string
	}{
		{
			name:    "an unknown provider",
			in:      "models:\n  - alias: a\n    provider: acme\n    backend_model: acme-1",
			wantErr: "unknown provider",
		},
		{
			// The provider's compatible surface is chat only, so this would be
			// a model that 404s for every request made to it.
			name:    "a surface the provider does not serve",
			in:      "models:\n  - alias: a\n    provider: anthropic\n    kind: embedding\n    backend_model: claude-opus-5",
			wantErr: "serves only chat",
		},
		{
			name:    "no model id",
			in:      "models:\n  - alias: a\n    provider: anthropic",
			wantErr: "model providers",
		},
		{
			// A model newer than this binary can be used, but not for free:
			// its price has to be stated rather than defaulted to nothing.
			name:    "an unpriced model the table has never heard of",
			in:      "models:\n  - alias: a\n    provider: anthropic\n    backend_model: claude-opus-9",
			wantErr: "not in the Keera Gateway price table",
		},
		{
			name: "an unpriced model with only half a price",
			in: "models:\n  - alias: a\n    provider: anthropic\n    backend_model: claude-opus-9\n" +
				"    input_micros_per_mtok: 5000000",
			wantErr: "not in the Keera Gateway price table",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseModelFile([]byte(tc.in))
			if err == nil {
				t.Fatal("Parse accepted a catalogue that would fail at runtime")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseAcceptsAModelNewerThanThisBinaryOnceItIsPriced(t *testing.T) {
	const in = `
models:
  - alias: a
    provider: anthropic
    backend_model: claude-opus-9
    input_micros_per_mtok: 5000000
    output_micros_per_mtok: 25000000
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if models[0].MaxContext != 0 {
		t.Errorf("MaxContext = %d; an unknown model's window cannot be guessed at",
			models[0].MaxContext)
	}
	if models[0].Backends[0] != "https://api.anthropic.com/v1" {
		t.Errorf("Backends = %v, want the provider's endpoint", models[0].Backends)
	}
}

func TestProvidersIsACopy(t *testing.T) {
	got := Providers()
	if len(got) == 0 {
		t.Fatal("Providers returned nothing")
	}
	got[0].Endpoint = "http://attacker.example/v1"
	got[0].Models[0].InputMicrosPerMTok = 0
	models, err := parseModelFile([]byte("models:\n  - alias: a\n    provider: " + got[0].Name +
		"\n    backend_model: " + got[0].Models[0].ID))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if models[0].Backends[0] == "http://attacker.example/v1" || models[0].InputMicrosPerMTok == 0 {
		t.Error("editing the returned table changed the defaults catalogues are parsed against")
	}
}

func TestOpenAIProviderFillsInEverythingButTheAlias(t *testing.T) {
	const in = `
models:
  - alias: keera-frontier
    provider: openai
    backend_model: gpt-5.1
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if got, want := m.Backends, []string{"https://api.openai.com/v1"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Backends = %v, want %v", got, want)
	}
	if m.MaxContext != 400_000 {
		t.Errorf("MaxContext = %d, want 400000", m.MaxContext)
	}
	if m.InputMicrosPerMTok == 0 || m.OutputMicrosPerMTok == 0 {
		t.Errorf("prices = %d/%d, want the provider's list price",
			m.InputMicrosPerMTok, m.OutputMicrosPerMTok)
	}
	if !m.Enabled || m.Kind != "chat" {
		t.Errorf("model = %+v, want an enabled chat alias", m)
	}
}

// stepping stone's ids keep their upstream capitals, and a cached prompt is
// priced at its published cache rate.
func TestSteppingStoneProviderFillsInEverythingButTheAlias(t *testing.T) {
	const in = `
models:
  - alias: keera-swiss-code
    provider: stepping-stone
    backend_model: Qwen/Qwen3-Coder-Next
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if got, want := m.Backends, []string{"https://llm.stoney-cloud.com/v1"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Backends = %v, want %v", got, want)
	}
	if m.MaxContext != 256_000 {
		t.Errorf("MaxContext = %d, want the provider's 256000", m.MaxContext)
	}
	if m.InputMicrosPerMTok != 340_000 || m.OutputMicrosPerMTok != 1_700_000 ||
		m.CachedInputMicrosPerMTok != 51_000 {
		t.Errorf("prices = %d/%d/%d, want the offer's 0.34/1.70/0.051 CHF",
			m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok)
	}
}

// Phoeniqs publishes no cache rate, so a cached prompt costs the input price.
func TestPhoeniqsProviderFillsInEverythingButTheAlias(t *testing.T) {
	const in = `
models:
  - alias: keera-swiss-agent
    provider: phoeniqs
    backend_model: inference-glm5
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if got, want := m.Backends, []string{"https://maas.phoeniqs.com/v1"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Backends = %v, want %v", got, want)
	}
	if m.Location != "ch" || m.MaxContext != 131_072 {
		t.Errorf("location, context = %q, %d, want ch, 131072", m.Location, m.MaxContext)
	}
	if m.InputMicrosPerMTok != 1_077_000 || m.OutputMicrosPerMTok != 3_386_000 ||
		m.CachedInputMicrosPerMTok != 0 {
		t.Errorf("prices = %d/%d/%d, want the list's 1.077/3.386 CHF and no cache rate",
			m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok)
	}
}

// CSCS bills cached input at the input price, so there is no cache rate.
func TestCSCSProviderFillsInEverythingButTheAlias(t *testing.T) {
	const in = `
models:
  - alias: keera-swiss-coder
    provider: cscs
    backend_model: moonshotai/Kimi-K2.7-Code
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if got, want := m.Backends, []string{"https://api.inference.cscs.ch/v1"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("Backends = %v, want %v", got, want)
	}
	if m.Location != "ch" || m.MaxContext != 262_144 {
		t.Errorf("location, context = %q, %d, want ch, 262144", m.Location, m.MaxContext)
	}
	if m.InputMicrosPerMTok != 1_300_000 || m.OutputMicrosPerMTok != 6_050_000 ||
		m.CachedInputMicrosPerMTok != 0 {
		t.Errorf("prices = %d/%d/%d, want the list's 1.30/6.05 CHF and no cache rate",
			m.InputMicrosPerMTok, m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok)
	}
}

// TestProviderTableIsUsable checks what every entry needs for a catalogue to
// name it, so a new provider cannot ship a broken URL or an unbillable model.
func TestProviderTableIsUsable(t *testing.T) {
	seenName := map[string]bool{}
	for _, p := range Providers() {
		t.Run(p.Name, func(t *testing.T) {
			if seenName[p.Name] {
				t.Errorf("name %q appears twice; the second entry is unreachable", p.Name)
			}
			seenName[p.Name] = true
			checkProvider(t, p)
			seenModel, seenDescription := map[string]bool{}, map[string]bool{}
			for _, m := range p.Models {
				checkProviderModel(t, p, m, seenDescription)
				if seenModel[m.ID] {
					t.Errorf("model %q appears twice; the second entry is unreachable", m.ID)
				}
				seenModel[m.ID] = true
			}
		})
	}
}

func checkProvider(t *testing.T, p Provider) {
	t.Helper()
	if p.Name != strings.ToLower(p.Name) {
		// Names are lowercased before lookup, so a capital is unreachable.
		t.Errorf("name %q is not lowercase; no catalogue could name it", p.Name)
	}
	if !strings.HasPrefix(p.Endpoint, "https://") {
		t.Errorf("endpoint %q is not https; prompts to a hosted model cross the internet",
			p.Endpoint)
	}
	if strings.HasSuffix(p.Endpoint, "/") {
		// The data plane appends the path, so this would double the slash.
		t.Errorf("endpoint %q has a trailing slash", p.Endpoint)
	}
	if p.Currency == "" || len(p.Kinds) == 0 || len(p.Models) == 0 {
		t.Errorf("entry = %+v, want a currency, a kind and a model", p)
	}
	// Every model declared against it takes this location, so it must be one
	// the catalogue accepts, and it must not claim to be the deployment's own.
	if !policy.ValidLocation(p.Location) || p.Location == policy.LocationOnPrem {
		t.Errorf("%s's location %q is not a provider's country", p.Name, p.Location)
	}
	switch {
	case p.Summary == "":
		t.Errorf("%s has no summary; its tile in the panel is a name and a logo",
			p.Name)
	case len(p.Summary) > 80 || strings.ContainsAny(p.Summary, "\n\t"):
		// It is read at a glance in a narrow tile.
		t.Errorf("%s's summary is not one short sentence: %q", p.Name, p.Summary)
	}
	// A placeholder and NeedsProductID must agree, or nothing fills the hole.
	if hole := strings.Contains(p.Endpoint, productIDPlaceholder); hole != p.NeedsProductID {
		t.Errorf("endpoint %q carries %s = %v, but NeedsProductID = %v",
			p.Endpoint, productIDPlaceholder, hole, p.NeedsProductID)
	}
}

func checkProviderModel(t *testing.T, p Provider, m ProviderModel, seenDescription map[string]bool) {
	t.Helper()
	if m.ID == "" || m.MaxContext <= 0 {
		t.Errorf("model %+v, want an id and a context window", m)
	}
	switch {
	case m.Description == "":
		t.Errorf("%s has no description; a router offered it is told only its alias",
			m.ID)
	case len(m.Description) > 120 || strings.ContainsAny(m.Description, "\n\t"):
		// It goes into a routing instruction beside every other model's.
		t.Errorf("%s's description is not one short sentence: %q", m.ID, m.Description)
	case seenDescription[m.Description]:
		t.Errorf("%s repeats another model's description; a router cannot tell "+
			"the two apart", m.ID)
	}
	seenDescription[m.Description] = true
	if m.ReleaseDate != "" && !policy.ValidReleaseDate(m.ReleaseDate) {
		t.Errorf("%s's release date %q is not YYYY-MM-DD", m.ID, m.ReleaseDate)
	}
	if m.InputMicrosPerMTok <= 0 || m.OutputMicrosPerMTok <= 0 {
		t.Errorf("%s is priced %d/%d; a defaulted price of 0 escapes every budget",
			m.ID, m.InputMicrosPerMTok, m.OutputMicrosPerMTok)
	}
	if long := m.LongPrompt; long != nil {
		switch {
		case long.Check() != nil || long.AboveTokens >= m.MaxContext:
			t.Errorf("%s's long prompts start at %d tokens, which no prompt in its "+
				"%d-token context can reach", m.ID, long.AboveTokens, m.MaxContext)
		case long.InputMicrosPerMTok < m.InputMicrosPerMTok ||
			long.OutputMicrosPerMTok < m.OutputMicrosPerMTok ||
			long.CachedInputMicrosPerMTok < m.CachedInputMicrosPerMTok ||
			long.CacheWriteMicrosPerMTok < m.CacheWriteMicrosPerMTok:
			// Providers charge more for a long prompt, never less.
			t.Errorf("%s's long-prompt prices %+v are below its own", m.ID, *long)
		}
	}
	if m.CacheWriteMicrosPerMTok != 0 && m.CacheWriteMicrosPerMTok <= m.InputMicrosPerMTok {
		// A write rate is only stated when it is a surcharge; zero already
		// means the input price.
		t.Errorf("%s's cache-write rate %d is not above its input rate %d",
			m.ID, m.CacheWriteMicrosPerMTok, m.InputMicrosPerMTok)
	}
	switch {
	case p.CachedInput && m.CachedInputMicrosPerMTok <= 0:
		// Without it, cached tokens cost up to ten times the console's price.
		t.Errorf("%s states no cached-input rate; a coding agent's resent "+
			"context is then charged at the full input price", m.ID)
	case !p.CachedInput && m.CachedInputMicrosPerMTok != 0:
		// An invented rate would under-charge every cached prompt.
		t.Errorf("%s states a cached-input rate of %d, but %s publishes none",
			m.ID, m.CachedInputMicrosPerMTok, p.Name)
	case m.CachedInputMicrosPerMTok >= m.InputMicrosPerMTok:
		// A cache read that costs more than a miss is a typo.
		t.Errorf("%s's cached-input rate %d is not below its input rate %d",
			m.ID, m.CachedInputMicrosPerMTok, m.InputMicrosPerMTok)
	}
}

// The cached-input rate is filled in like every other number the table knows,
// and given way on like every other one: a department charged an internal rate
// for a hosted model is charged it for the cached half of its prompts too.
func TestProviderFillsInTheCachedInputRate(t *testing.T) {
	models, err := parseModelFile([]byte(`
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	if m.CachedInputMicrosPerMTok <= 0 || m.CachedInputMicrosPerMTok >= m.InputMicrosPerMTok {
		t.Errorf("cached rate = %d against an input rate of %d, want the provider's "+
			"discounted one", m.CachedInputMicrosPerMTok, m.InputMicrosPerMTok)
	}
	if m.CacheWriteMicrosPerMTok <= m.InputMicrosPerMTok {
		t.Errorf("cache-write rate = %d against an input rate of %d, want the "+
			"provider's surcharge", m.CacheWriteMicrosPerMTok, m.InputMicrosPerMTok)
	}

	overridden, err := parseModelFile([]byte(`
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
    cached_input_micros_per_mtok: 7
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := overridden[0].CachedInputMicrosPerMTok; got != 7 {
		t.Errorf("cached rate = %d, want the declared 7", got)
	}
}

// A model the table has never heard of has to be priced by hand, and the
// cached rate is the one price that is not required with it: left out, those
// tokens are charged at the input price, which is too high rather than too
// low and is what the gateway charged for them before the column existed.
func TestAnUnknownModelNeedsNoCachedRate(t *testing.T) {
	models, err := parseModelFile([]byte(`
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-9
    input_micros_per_mtok: 1000000
    output_micros_per_mtok: 5000000
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := models[0].CachedInputMicrosPerMTok; got != 0 {
		t.Errorf("cached rate = %d, want 0 - unstated, and charged at the input price", got)
	}
	if got := models[0].Cost(policy.Tokens{Input: 1000, CachedInput: 1000}); got != 1000 {
		t.Errorf("cost of a wholly cached prompt = %d, want it charged at the input "+
			"price, 1000", got)
	}
}

// Infomaniak serves every customer at their own address, so the table's
// endpoint is a template and the entry supplies the one part of it the
// provider cannot know.
func TestProductIDFillsInThePerCustomerEndpoint(t *testing.T) {
	const in = `
models:
  - alias: keera-swiss
    provider: infomaniak
    product_id: 100234
    backend_model: swiss-ai/Apertus-v1.5-70B
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m := models[0]
	const want = "https://api.infomaniak.com/2/ai/100234/openai/v1"
	if len(m.Backends) != 1 || m.Backends[0] != want {
		t.Errorf("Backends = %v, want [%s]", m.Backends, want)
	}
	if m.MaxContext != 100_000 {
		t.Errorf("MaxContext = %d, want the provider's 100000", m.MaxContext)
	}
	// No cached-input rate is published, so there is none to fill in and those
	// tokens are charged at the input price.
	if m.CachedInputMicrosPerMTok != 0 {
		t.Errorf("CachedInputMicrosPerMTok = %d, want 0", m.CachedInputMicrosPerMTok)
	}
}

// A deployment sending these prompts through an egress proxy mirrors the path,
// so the hole is in the same place in an address of its own.
func TestProductIDFillsInACatalogueOwnAddressToo(t *testing.T) {
	const in = `
models:
  - alias: keera-swiss
    provider: infomaniak
    product_id: 100234
    backends: ["http://egress.corp:8080/2/ai/{product_id}/openai/v1"]
    backend_model: google/gemma-4-31B-it
`
	models, err := parseModelFile([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	const want = "http://egress.corp:8080/2/ai/100234/openai/v1"
	if got := models[0].Backends; len(got) != 1 || got[0] != want {
		t.Errorf("Backends = %v, want [%s]", got, want)
	}
}

// An entry that wrote the finished address itself has already answered the
// question, so it is never asked.
func TestAFinishedAddressNeedsNoProductID(t *testing.T) {
	const in = `
models:
  - alias: keera-swiss
    provider: infomaniak
    backends: ["https://api.infomaniak.com/2/ai/100234/openai/v1"]
    backend_model: google/gemma-4-31B-it
`
	if _, err := parseModelFile([]byte(in)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestProductIDRejectsWhatWouldFailSilentlyAtRuntime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{{
		// Without it the address still has a hole in it, and every request
		// would go to a URL with braces in the path.
		name: "none given",
		in: `
models:
  - alias: keera-swiss
    provider: infomaniak
    backend_model: google/gemma-4-31B-it
`,
		want: "product_id",
	}, {
		// A provider whose endpoint is the same for everybody has nowhere to
		// put one, so accepting it would be accepting a value that does nothing.
		name: "provider that needs none",
		in: `
models:
  - alias: keera-frontier
    provider: anthropic
    product_id: 100234
    backend_model: claude-opus-5
`,
		want: "takes no product_id",
	}, {
		// It is pasted into a URL path, so a slash builds a working-looking
		// address that is not the one meant.
		name: "not safe in a path",
		in: `
models:
  - alias: keera-swiss
    provider: infomaniak
    product_id: "100234/../1"
    backend_model: google/gemma-4-31B-it
`,
		want: "must be letters, digits",
	}, {
		// Nothing would fill it in, so it would be silently dropped.
		name: "no provider to fill in",
		in: `
models:
  - alias: keera-local
    product_id: 100234
    backends: ["http://keera-code:8000/v1"]
    backend_model: keera-speed
`,
		want: "names no provider",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseModelFile([]byte(c.in))
			if err == nil {
				t.Fatalf("Parse accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want one mentioning %q", err, c.want)
			}
		})
	}
}

// Where a model runs is the provider's. Without one, a backend inside the
// network is onprem. An entry may say otherwise, for an endpoint that runs
// somewhere else.
func TestLocationComesFromTheProvider(t *testing.T) {
	models, err := parseModelFile([]byte(`
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: claude-opus-5
  - alias: keera-swiss
    provider: stepping-stone
    backend_model: Qwen/Qwen3-Coder-Next
  - alias: keera-code
    backends: ["http://keera-code:8000/v1"]
    backend_model: keera-code
  - alias: keera-proxied
    provider: openai
    backend_model: gpt-5.5
    location: CH
  - alias: keera-embed
    kind: embedding
    backends: ["https://llm.stoney-cloud.com/v1"]
    backend_model: BAAI/bge-m3
    location: ch
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for i, want := range []string{"usa", "ch", "onprem", "ch", "ch"} {
		if got := models[i].Location; got != want {
			t.Errorf("%s: location = %q, want %q", models[i].Alias, got, want)
		}
	}
}

// A backend on the internet does not say which country it is in, so a model
// with no provider there must state it rather than be called onprem.
func TestAnOutsideBackendWithNoProviderMustStateItsLocation(t *testing.T) {
	_, err := parseModelFile([]byte(`
models:
  - alias: keera-embed
    kind: embedding
    backends: ["https://llm.stoney-cloud.com/v1"]
    backend_model: BAAI/bge-m3
`))
	if err == nil || !strings.Contains(err.Error(), "location is required") {
		t.Errorf("Parse = %v, want it to ask for the location", err)
	}
}

// The release date is filled in like the other values the table knows, and an
// entry's own date wins. It may be written unquoted, as YAML users will.
func TestReleaseDateComesFromTheTable(t *testing.T) {
	p, _ := ProviderByName("anthropic")
	var known ProviderModel
	for _, m := range p.Models {
		if m.ReleaseDate != "" {
			known = m
			break
		}
	}
	if known.ID == "" {
		t.Skip("the table states no release date")
	}
	models, err := parseModelFile([]byte(`
models:
  - alias: keera-frontier
    provider: anthropic
    backend_model: ` + known.ID + `
  - alias: keera-code
    backends: ["http://keera-code:8000/v1"]
    backend_model: keera-code
    release_date: 2026-03-01
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := models[0].ReleaseDate; got != known.ReleaseDate {
		t.Errorf("release date = %q, want the table's %q", got, known.ReleaseDate)
	}
	if got := models[1].ReleaseDate; got != "2026-03-01" {
		t.Errorf("release date = %q, want the declared 2026-03-01", got)
	}
}

// A model Anthropic dates can be named by either id, and is priced the same.
func TestADatedModelIsTheSameModel(t *testing.T) {
	models, err := parseModelFile([]byte(`
models:
  - alias: claude-haiku
    provider: anthropic
    backend_model: claude-haiku-4-5-20251001
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p := models[0].InputMicrosPerMTok; p != 1_000_000 {
		t.Errorf("input price = %d, want claude-haiku-4-5's", p)
	}
}

// A model priced by prompt length brings its long-prompt prices with it, and
// the copy is the entry's own: changing it leaves the table alone.
func TestProviderFillsInTheLongPromptPrices(t *testing.T) {
	file := []byte(`
models:
  - alias: keera-light
    provider: anthropic
    backend_model: claude-haiku-5-5
`)
	models, err := parseModelFile(file)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	long := models[0].LongPrompt
	if long == nil || long.AboveTokens != 100_000 ||
		long.InputMicrosPerMTok <= models[0].InputMicrosPerMTok {
		t.Fatalf("long prompt = %+v, want the table's dearer prices past 100k tokens", long)
	}
	long.InputMicrosPerMTok = 0
	again, err := parseModelFile(file)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if again[0].LongPrompt.InputMicrosPerMTok == 0 {
		t.Error("changing one entry's long-prompt prices changed the table")
	}
}

// A long-prompt tier without a threshold would charge every prompt its prices.
func TestALongPromptNeedsAThreshold(t *testing.T) {
	_, err := parseModelFile([]byte(`
models:
  - alias: local
    backends: [http://vllm:8000/v1]
    backend_model: local
    long_prompt:
      input_micros_per_mtok: 1
`))
	if err == nil || !strings.Contains(err.Error(), "above_tokens") {
		t.Errorf("err = %v, want one naming above_tokens", err)
	}
}
