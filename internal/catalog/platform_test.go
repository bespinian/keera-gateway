package catalog

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestTheProviderKeySettingsAreNamedAfterTheProvider(t *testing.T) {
	if got := EnvPrefix("stepping-stone"); got != "KEERA_PROVIDER_STEPPING_STONE_" {
		t.Errorf("prefix = %q", got)
	}
}

// A model on the deployment's key goes to the provider's own endpoint at its
// list prices, whatever the organisation wrote.
func TestAModelOnTheDeploymentsKeyIsLockedToTheProvider(t *testing.T) {
	p := Platform{"anthropic": {APIKey: "sk"}}
	m := policy.Model{
		Provider: "anthropic", BackendModel: "claude-haiku-4-5",
		Backends:           []string{"https://evil.example/v1"},
		InputMicrosPerMTok: 1, OutputMicrosPerMTok: 1, CachedInputMicrosPerMTok: 1,
		CacheWriteMicrosPerMTok: 1,
	}
	if err := p.Lock(&m); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	prov, _ := ProviderByName("anthropic")
	known, _ := prov.Model("claude-haiku-4-5")
	if len(m.Backends) != 1 || m.Backends[0] != prov.Endpoint {
		t.Errorf("backends = %v, want only %s", m.Backends, prov.Endpoint)
	}
	if m.InputMicrosPerMTok != known.InputMicrosPerMTok ||
		m.OutputMicrosPerMTok != known.OutputMicrosPerMTok ||
		m.CachedInputMicrosPerMTok != known.CachedInputMicrosPerMTok ||
		m.CacheWriteMicrosPerMTok != known.CacheWriteMicrosPerMTok {
		t.Errorf("prices = %d/%d/%d/%d, want the list prices", m.InputMicrosPerMTok,
			m.OutputMicrosPerMTok, m.CachedInputMicrosPerMTok, m.CacheWriteMicrosPerMTok)
	}
	if !m.PlatformKey {
		t.Error("not marked as on the deployment's key")
	}
}

// A model priced by prompt length is billed its long-prompt list prices too.
func TestAModelOnTheDeploymentsKeyTakesTheLongPromptPrices(t *testing.T) {
	p := Platform{"anthropic": {APIKey: "sk"}}
	m := policy.Model{Provider: "anthropic", BackendModel: "claude-haiku-5-5"}
	if err := p.Lock(&m); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	prov, _ := ProviderByName("anthropic")
	known, _ := prov.Model("claude-haiku-5-5")
	if m.LongPrompt == nil || known.LongPrompt == nil || *m.LongPrompt != *known.LongPrompt {
		t.Errorf("long prompt = %+v, want the list prices %+v", m.LongPrompt, known.LongPrompt)
	}
}

func TestAModelThePriceTableDoesNotKnowCannotBeBilled(t *testing.T) {
	p := Platform{"openai": {APIKey: "sk"}}
	m := policy.Model{Provider: "openai", BackendModel: "gpt-from-the-future"}
	if err := p.Lock(&m); err == nil || !strings.Contains(err.Error(), "price table") {
		t.Errorf("err = %v, want one about the price table", err)
	}
}

func TestTheDeploymentsKeyLeavesOtherModelsAlone(t *testing.T) {
	p := Platform{"anthropic": {APIKey: "sk"}}
	for _, m := range []policy.Model{
		{Provider: "openai", BackendModel: "gpt-5.5", Backends: []string{"https://proxy/v1"}},
		{Backends: []string{"http://vllm:8000/v1"}, BackendModel: "local"},
		{Provider: "anthropic", BackendModel: "claude-haiku-4-5", Subscription: true,
			Backends: []string{"https://api.anthropic.com/v1"}},
	} {
		before := m.Backends[0]
		if err := p.Lock(&m); err != nil || m.PlatformKey || m.Backends[0] != before {
			t.Errorf("%+v was locked: %v", m, err)
		}
	}
}

func TestInfomaniaksEndpointTakesTheDeploymentsProductID(t *testing.T) {
	p := Platform{"infomaniak": {APIKey: "sk", ProductID: "12345"}}
	prov, _ := ProviderByName("infomaniak")
	m := policy.Model{Provider: "infomaniak", BackendModel: prov.Models[0].ID}
	if err := p.Lock(&m); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if want := strings.ReplaceAll(prov.Endpoint, "{product_id}", "12345"); m.Backends[0] != want {
		t.Errorf("backend = %s, want %s", m.Backends[0], want)
	}
}

func TestBillingIsInTheProvidersCurrency(t *testing.T) {
	b := Platform{"cscs": {APIKey: "sk", DiscountBP: 500}}.Billing("cscs")
	if b.Provider != "cscs" || b.Currency != "CHF" || b.DiscountBP != 500 {
		t.Errorf("billing = %+v", b)
	}
}
