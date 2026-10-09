package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/payment"
)

// Payments is how organisations pay for the deployment's provider keys in
// advance. See docs/billing.md.
type Payments struct {
	PostFinance payment.Settings
	// VATBP is the VAT added to every payment, in basis points: 810 is 8.1%.
	VATBP int64
}

// creditCurrency is what credit is held and paid in.
const creditCurrency = "CHF"

// payments reads the PostFinance Checkout account, and sets on each provider
// key what its use takes from an organisation's credit. Without an account
// it is nil, and nothing is taken.
func payments(platform catalog.Platform, publicURL string) (*Payments, error) {
	space := env("KEERA_POSTFINANCE_SPACE_ID", "")
	user := env("KEERA_POSTFINANCE_USER_ID", "")
	key := env("KEERA_POSTFINANCE_AUTH_KEY", "")
	if space == "" && user == "" && key == "" {
		for _, s := range []string{"KEERA_POSTFINANCE_URL", "KEERA_BILLING_VAT",
			rateSetting("USD")} {
			if env(s, "") != "" {
				return nil, fmt.Errorf("%s does nothing without KEERA_POSTFINANCE_SPACE_ID, "+
					"KEERA_POSTFINANCE_USER_ID and KEERA_POSTFINANCE_AUTH_KEY", s)
			}
		}
		return nil, nil
	}
	if space == "" || user == "" || key == "" {
		return nil, fmt.Errorf("payments take all three of KEERA_POSTFINANCE_SPACE_ID, " +
			"KEERA_POSTFINANCE_USER_ID and KEERA_POSTFINANCE_AUTH_KEY")
	}
	spaceID, err := strconv.ParseInt(space, 10, 64)
	if err != nil || spaceID <= 0 {
		return nil, fmt.Errorf("KEERA_POSTFINANCE_SPACE_ID is %q; it is the number of "+
			"your space in PostFinance Checkout", space)
	}
	authKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(authKey) == 0 {
		return nil, fmt.Errorf("KEERA_POSTFINANCE_AUTH_KEY is not the authentication key " +
			"PostFinance Checkout showed for the application user: it is base64")
	}
	if len(platform) == 0 {
		return nil, fmt.Errorf("payments are for the deployment's own provider keys, and " +
			"no KEERA_PROVIDER_<NAME>_API_KEY is set")
	}
	if !strings.HasPrefix(publicURL, "https://") && !onThisMachine(publicURL) {
		return nil, fmt.Errorf("payments need KEERA_PUBLIC_URL to be an https address: " +
			"PostFinance sends the customer back there after paying")
	}
	vat, err := percentBP(env("KEERA_BILLING_VAT", ""))
	if err != nil {
		return nil, fmt.Errorf("KEERA_BILLING_VAT: %w", err)
	}
	if err := creditRates(platform); err != nil {
		return nil, err
	}
	return &Payments{
		PostFinance: payment.Settings{
			URL:     env("KEERA_POSTFINANCE_URL", payment.DefaultURL),
			SpaceID: spaceID, UserID: user, AuthKey: authKey,
		},
		VATBP: vat,
	}, nil
}

// creditRates sets what each provider key's use takes from the credit. A
// provider that prices in francs takes its list price; any other needs the
// rate it is converted at.
func creditRates(platform catalog.Platform) error {
	for name, k := range platform {
		prov, _ := catalog.ProviderByName(name)
		if prov.Currency == creditCurrency {
			k.CreditRate = 1_000_000
			platform[name] = k
			continue
		}
		setting := rateSetting(prov.Currency)
		raw := env(setting, "")
		if raw == "" {
			return fmt.Errorf("%s is required: %s prices in %s, and credit is in %s",
				setting, prov.Name, prov.Currency, creditCurrency)
		}
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil || rate <= 0 || rate > 100 {
			return fmt.Errorf("%s is %q; it is how many francs one %s is, such as 0.80",
				setting, raw, prov.Currency)
		}
		k.CreditRate = int64(math.Round(rate * 1_000_000))
		platform[name] = k
	}
	return nil
}

// rateSetting names the exchange rate for a currency.
func rateSetting(currency string) string { return "KEERA_BILLING_CHF_PER_" + currency }

// percentBP reads a percentage, such as 8.1, as basis points.
func percentBP(raw string) (int64, error) { return discountBP(raw) }

// onThisMachine reports whether an http address is this machine, as in the
// development loop. Only the person's own browser is sent back there, and it
// reaches this machine without TLS.
func onThisMachine(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
