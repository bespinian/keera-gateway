package catalog

import (
	"fmt"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// PlatformKey is the deployment's own account at a provider, from the
// environment. Every organisation's models of that provider use it, and are
// billed for it at list price. See docs/billing.md.
type PlatformKey struct {
	APIKey string
	// ProductID completes the endpoint of a provider that needs one, such as
	// Infomaniak.
	ProductID string
	// DiscountBP is the discount the provider gives on its list price, in
	// basis points: 1250 is 12.5%.
	DiscountBP int64
	// CreditRate is what one unit of the provider's currency takes from an
	// organisation's credit, in CHF, times a million. Zero without payments.
	CreditRate int64
}

// Platform is the deployment's provider keys, by provider name.
type Platform map[string]PlatformKey

// EnvPrefix is the start of every environment variable for a provider's
// platform key, such as KEERA_PROVIDER_STEPPING_STONE_.
func EnvPrefix(provider string) string {
	return "KEERA_PROVIDER_" + strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_"
}

// Has reports whether the deployment holds the key for a provider.
func (p Platform) Has(provider string) bool {
	_, ok := p[provider]
	return ok
}

// Covers reports whether m is sent with the deployment's key. A subscription
// model is not: each caller's own Claude sign-in pays for it.
func (p Platform) Covers(m policy.Model) bool {
	return p.Has(m.Provider) && !m.Subscription
}

// Lock sets what a model on the deployment's key may not choose for itself:
// the provider's own endpoint, so the key goes nowhere else, and its list
// prices, which the organisation is billed at. It does nothing to a model the
// key does not cover.
func (p Platform) Lock(m *policy.Model) error {
	if !p.Covers(*m) {
		return nil
	}
	prov, _ := provider(m.Provider)
	known, ok := prov.Model(m.BackendModel)
	if !ok {
		return fmt.Errorf("%s is not in the Keera Gateway price table for %s, so it "+
			"cannot be billed on this deployment's key (it knows: %s)",
			m.BackendModel, prov.Name, prov.modelIDs())
	}
	endpoint := prov.Endpoint
	if prov.NeedsProductID {
		endpoint = strings.ReplaceAll(endpoint, productIDPlaceholder, p[m.Provider].ProductID)
	}
	m.Backends = []string{endpoint}
	m.InputMicrosPerMTok = known.InputMicrosPerMTok
	m.OutputMicrosPerMTok = known.OutputMicrosPerMTok
	m.CachedInputMicrosPerMTok = known.CachedInputMicrosPerMTok
	m.CacheWriteMicrosPerMTok = known.CacheWriteMicrosPerMTok
	m.LongPrompt = known.longPrompt()
	m.PlatformKey = true
	return nil
}

// Billing is how a model of this provider is billed on the deployment's key.
func (p Platform) Billing(provider string) *policy.Billing {
	prov, _ := ProviderByName(provider)
	return &policy.Billing{
		Provider:   provider,
		Currency:   prov.Currency,
		DiscountBP: p[provider].DiscountBP,
		CreditRate: p[provider].CreditRate,
	}
}
