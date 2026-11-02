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

// Payments is how organisations pay for the deployment's provider keys and
// the gateway fee in advance. See docs/billing.md.
type Payments struct {
	PostFinance payment.Settings
	// VATBP is the VAT added to every payment, in basis points: 810 is 8.1%.
	VATBP int64
}

// payments reads the PostFinance Checkout account and the VAT. Without an
// account it is nil, and billed calls take nothing from credit.
func payments(platform catalog.Platform, fee int64, publicURL string) (*Payments, error) {
	space := env("KEERA_BILLING_POSTFINANCE_SPACE_ID", "")
	user := env("KEERA_BILLING_POSTFINANCE_USER_ID", "")
	key := env("KEERA_BILLING_POSTFINANCE_AUTH_KEY", "")
	if space == "" && user == "" && key == "" {
		for _, s := range []string{"KEERA_BILLING_POSTFINANCE_URL", "KEERA_BILLING_VAT"} {
			if env(s, "") != "" {
				return nil, fmt.Errorf("%s does nothing without "+
					"KEERA_BILLING_POSTFINANCE_SPACE_ID, KEERA_BILLING_POSTFINANCE_USER_ID "+
					"and KEERA_BILLING_POSTFINANCE_AUTH_KEY", s)
			}
		}
		return nil, nil
	}
	if space == "" || user == "" || key == "" {
		return nil, fmt.Errorf("payments take all three of " +
			"KEERA_BILLING_POSTFINANCE_SPACE_ID, KEERA_BILLING_POSTFINANCE_USER_ID " +
			"and KEERA_BILLING_POSTFINANCE_AUTH_KEY")
	}
	spaceID, err := strconv.ParseInt(space, 10, 64)
	if err != nil || spaceID <= 0 {
		return nil, fmt.Errorf("KEERA_BILLING_POSTFINANCE_SPACE_ID is %q; it is the "+
			"number of your space in PostFinance Checkout", space)
	}
	authKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(authKey) == 0 {
		return nil, fmt.Errorf("KEERA_BILLING_POSTFINANCE_AUTH_KEY is not the " +
			"authentication key PostFinance Checkout showed for the application user: " +
			"it is base64")
	}
	if len(platform) == 0 && fee == 0 {
		return nil, fmt.Errorf("payments are for the deployment's own provider keys and " +
			"the gateway fee, and neither a KEERA_PROVIDER_<NAME>_API_KEY nor " +
			"KEERA_BILLING_GATEWAY_FEE is set")
	}
	if !strings.HasPrefix(publicURL, "https://") && !onThisMachine(publicURL) {
		return nil, fmt.Errorf("payments need KEERA_PUBLIC_URL to be an https address: " +
			"PostFinance sends the customer back there after paying")
	}
	vat, err := percentBP(env("KEERA_BILLING_VAT", ""))
	if err != nil {
		return nil, fmt.Errorf("KEERA_BILLING_VAT: %w", err)
	}
	return &Payments{
		PostFinance: payment.Settings{
			URL:     env("KEERA_BILLING_POSTFINANCE_URL", ""),
			SpaceID: spaceID, UserID: user, AuthKey: authKey,
		},
		VATBP: vat,
	}, nil
}

// gatewayFee reads KEERA_BILLING_GATEWAY_FEE, in CHF per million tokens, as
// micros.
func gatewayFee(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	chf, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(chf) || chf < 0 || chf > maxGatewayFee {
		return 0, fmt.Errorf("KEERA_BILLING_GATEWAY_FEE is %q; it is CHF per million "+
			"tokens, from 0 to %d, such as 0.05", raw, maxGatewayFee)
	}
	return int64(math.Round(chf * 1_000_000)), nil
}

// maxGatewayFee is far above any sensible fee. It keeps a typo from billing
// thousands for one request.
const maxGatewayFee = 100

// percentBP reads a percentage, such as 8.1, as basis points.
func percentBP(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
	if err != nil || pct < 0 || pct > 100 {
		return 0, fmt.Errorf("%q must be a percentage from 0 to 100, such as 8.1", raw)
	}
	return int64(math.Round(pct * 100)), nil
}

// onThisMachine reports whether an http address is this machine, as in the
// development loop, where the browser needs no TLS. Browsers send every
// *.localhost name here too.
func onThisMachine(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
