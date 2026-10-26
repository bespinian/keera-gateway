package config

import (
	"maps"
	"strings"
	"testing"
)

// A PostFinance Checkout account switches payments on, and with it each
// provider key's calls take from the credit.
func TestAPostFinanceAccountSwitchesPaymentsOn(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_PUBLIC_URL":                   "https://keera.example",
		"KEERA_PROVIDER_ANTHROPIC_API_KEY":   "sk-ant-test",
		"KEERA_PROVIDER_CSCS_API_KEY":        "sk-cscs-test",
		"KEERA_BILLING_POSTFINANCE_SPACE_ID": "4711",
		"KEERA_BILLING_POSTFINANCE_USER_ID":  "123456",
		"KEERA_BILLING_POSTFINANCE_AUTH_KEY": "c2VjcmV0",
		"KEERA_BILLING_VAT":                  "8.1",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := c.Payments
	if p == nil {
		t.Fatal("payments are off")
	}
	if p.PostFinance.SpaceID != 4711 || p.PostFinance.UserID != "123456" ||
		string(p.PostFinance.AuthKey) != "secret" || p.VATBP != 810 {
		t.Errorf("payments = %+v", p)
	}
}

func TestWithoutAnAccountNothingIsTakenFromTheCredit(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{"KEERA_PROVIDER_CSCS_API_KEY": "sk"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Payments != nil {
		t.Errorf("payments = %+v, want none", c.Payments)
	}
}

func TestAPaymentSettingThatCannotWorkStopsTheStart(t *testing.T) {
	account := map[string]string{
		"KEERA_PUBLIC_URL":                   "https://keera.example",
		"KEERA_PROVIDER_CSCS_API_KEY":        "sk",
		"KEERA_BILLING_POSTFINANCE_SPACE_ID": "4711",
		"KEERA_BILLING_POSTFINANCE_USER_ID":  "1",
		"KEERA_BILLING_POSTFINANCE_AUTH_KEY": "c2VjcmV0",
	}
	with := func(k, v string) map[string]string {
		out := maps.Clone(account)
		out[k] = v
		return out
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"half an account", map[string]string{"KEERA_BILLING_POSTFINANCE_SPACE_ID": "1"}, "all three"},
		{
			"VAT without an account",
			map[string]string{"KEERA_BILLING_VAT": "8.1"},
			"KEERA_BILLING_VAT does nothing",
		},
		{
			"space not a number", with("KEERA_BILLING_POSTFINANCE_SPACE_ID", "main"),
			"KEERA_BILLING_POSTFINANCE_SPACE_ID",
		},
		{"key not base64", with("KEERA_BILLING_POSTFINANCE_AUTH_KEY", "not base64!"), "base64"},
		{"no provider key", with("KEERA_PROVIDER_CSCS_API_KEY", ""), "KEERA_PROVIDER_<NAME>_API_KEY"},
		{"no https address", with("KEERA_PUBLIC_URL", "http://keera.example"), "KEERA_PUBLIC_URL"},
		{"not this machine", with("KEERA_PUBLIC_URL", "http://localhost.example:8080"), "KEERA_PUBLIC_URL"},
		{"VAT out of range", with("KEERA_BILLING_VAT", "120"), "KEERA_BILLING_VAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadWith(t, valid(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// In the development loop, the browser comes back to this machine over http.
func TestPaymentsWorkOnThisMachineOverHTTP(t *testing.T) {
	for _, u := range []string{
		"http://localhost:8080", "http://keera.localhost:8080",
		"http://127.0.0.1:8080", "http://[::1]:8080",
	} {
		c, err := loadWith(t, valid(map[string]string{
			"KEERA_PUBLIC_URL":                   u,
			"KEERA_PROVIDER_CSCS_API_KEY":        "sk",
			"KEERA_BILLING_POSTFINANCE_SPACE_ID": "4711",
			"KEERA_BILLING_POSTFINANCE_USER_ID":  "1",
			"KEERA_BILLING_POSTFINANCE_AUTH_KEY": "c2VjcmV0",
		}))
		if err != nil || c.Payments == nil {
			t.Errorf("%s: payments = %v, %v", u, c.Payments, err)
		}
	}
}
