package config

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/catalog"
)

// The settings a provider's platform key takes, after its EnvPrefix.
const (
	platformAPIKey    = "API_KEY"
	platformProductID = "PRODUCT_ID"
	platformDiscount  = "DISCOUNT"
)

// platformKeys reads the deployment's own provider keys, such as
// KEERA_PROVIDER_ANTHROPIC_API_KEY. See docs/billing.md.
func platformKeys() (catalog.Platform, error) {
	out := catalog.Platform{}
	known := []string{}
	for _, p := range catalog.Providers() {
		prefix := catalog.EnvPrefix(p.Name)
		for _, s := range []string{platformAPIKey, platformProductID, platformDiscount} {
			known = append(known, prefix+s)
		}
		key := env(prefix+platformAPIKey, "")
		productID := env(prefix+platformProductID, "")
		discount := env(prefix+platformDiscount, "")
		if key == "" {
			if productID != "" || discount != "" {
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
		case productID != "" && !validPathSegment(productID):
			return nil, fmt.Errorf("%s must be letters, digits, hyphens and underscores",
				prefix+platformProductID)
		}
		bp, err := discountBP(discount)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", prefix+platformDiscount, err)
		}
		out[p.Name] = catalog.PlatformKey{APIKey: key, ProductID: productID, DiscountBP: bp}
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

// discountBP reads a percentage, such as 12.5, as basis points.
func discountBP(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
	if err != nil || pct < 0 || pct > 100 {
		return 0, fmt.Errorf("%q must be a percentage from 0 to 100, such as 12.5", raw)
	}
	return int64(math.Round(pct * 100)), nil
}

// validPathSegment reports whether s is safe to put into a URL path.
func validPathSegment(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '_'
	})
}
