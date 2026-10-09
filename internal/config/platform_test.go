package config

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/catalog"
)

func TestAProviderKeyIsReadWithItsDiscount(t *testing.T) {
	c, err := loadWith(t, valid(map[string]string{
		"KEERA_PROVIDER_ANTHROPIC_API_KEY":      "sk-ant-test",
		"KEERA_PROVIDER_ANTHROPIC_DISCOUNT":     "12.5",
		"KEERA_PROVIDER_STEPPING_STONE_API_KEY": "sk-ss-test",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := catalog.Platform{
		"anthropic":      {APIKey: "sk-ant-test", DiscountBP: 1250},
		"stepping-stone": {APIKey: "sk-ss-test"},
	}
	if len(c.Platform) != len(want) {
		t.Fatalf("platform = %+v, want %+v", c.Platform, want)
	}
	for name, k := range want {
		if c.Platform[name] != k {
			t.Errorf("%s = %+v, want %+v", name, c.Platform[name], k)
		}
	}
}

func TestNoProviderKeyLeavesEveryOrganisationItsOwn(t *testing.T) {
	c, err := loadWith(t, valid(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Platform) != 0 {
		t.Errorf("platform = %+v, want none", c.Platform)
	}
}

func TestAProviderKeySettingThatCannotWorkStopsTheStart(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"misspelt provider", map[string]string{"KEERA_PROVIDER_ANTROPIC_API_KEY": "sk"},
			"KEERA_PROVIDER_ANTROPIC_API_KEY"},
		{"discount without a key", map[string]string{"KEERA_PROVIDER_OPENAI_DISCOUNT": "10"},
			"KEERA_PROVIDER_OPENAI_API_KEY"},
		{"discount above 100", map[string]string{"KEERA_PROVIDER_OPENAI_API_KEY": "sk",
			"KEERA_PROVIDER_OPENAI_DISCOUNT": "120"}, "percentage"},
		{"discount that is no number", map[string]string{"KEERA_PROVIDER_OPENAI_API_KEY": "sk",
			"KEERA_PROVIDER_OPENAI_DISCOUNT": "ten"}, "percentage"},
		{"Infomaniak without a product id", map[string]string{
			"KEERA_PROVIDER_INFOMANIAK_API_KEY": "sk"}, "KEERA_PROVIDER_INFOMANIAK_PRODUCT_ID"},
		{"product id where none is needed", map[string]string{
			"KEERA_PROVIDER_OPENAI_API_KEY": "sk", "KEERA_PROVIDER_OPENAI_PRODUCT_ID": "1"},
			"does nothing"},
		{"product id that is no path segment", map[string]string{
			"KEERA_PROVIDER_INFOMANIAK_API_KEY":    "sk",
			"KEERA_PROVIDER_INFOMANIAK_PRODUCT_ID": "1/../2"}, "letters, digits"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, valid(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}
