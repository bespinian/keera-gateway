package config

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bespinian/keera-gateway/internal/catalog"
)

// The settings a provider's platform key takes, after its envPrefix.
const (
	platformAPIKey    = "API_KEY"
	platformProductID = "PRODUCT_ID"
)

// platformKeys reads the deployment's own provider keys, such as
// KEERA_PROVIDER_ANTHROPIC_API_KEY. See docs/billing.md.
func platformKeys() (catalog.Platform, error) {
	out := catalog.Platform{}
	known := []string{}
	for _, p := range catalog.Providers() {
		prefix := envPrefix("PROVIDER", p.Name)
		known = append(known, prefix+platformAPIKey, prefix+platformProductID)
		key := env(prefix+platformAPIKey, "")
		productID := env(prefix+platformProductID, "")
		if key == "" {
			if productID != "" {
				return nil, fmt.Errorf("%s is not set, so the other %s settings do nothing",
					prefix+platformAPIKey, prefix+"*")
			}
			continue
		}
		switch {
		case p.NeedsProductID && productID == "":
			return nil, fmt.Errorf("%s is required with %s: %s serves each customer "+
				"at their own address", prefix+platformProductID, prefix+platformAPIKey, p.Name)
		case !p.NeedsProductID && productID != "":
			return nil, fmt.Errorf("%s does nothing: %s serves everybody at the same address",
				prefix+platformProductID, p.Name)
		case productID != "" && !catalog.ValidProductID(productID):
			return nil, fmt.Errorf("%s must be letters, digits, hyphens and underscores",
				prefix+platformProductID)
		}
		out[p.Name] = catalog.PlatformKey{APIKey: key, ProductID: productID}
	}
	// A misspelt provider name would otherwise leave every model of it
	// waiting for a key nobody can enter.
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "KEERA_PROVIDER_") && strings.TrimSpace(value) != "" &&
			!slices.Contains(known, name) {
			return nil, fmt.Errorf("%s is not a setting Keera Gateway knows; the providers are: %s",
				name, catalog.ProviderNames())
		}
	}
	return out, nil
}
