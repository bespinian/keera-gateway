package policy

import "testing"

func TestCleanEmailDomainTakesWhatPeopleType(t *testing.T) {
	for given, want := range map[string]string{
		"example.ch":     "example.ch",
		"@example.ch":    "example.ch",
		"Example.CH":     "example.ch",
		" example.ch. ":  "example.ch",
		"sub.example.ch": "sub.example.ch",
		"":               "",
	} {
		got, err := CleanEmailDomain(given)
		if err != nil || got != want {
			t.Errorf("CleanEmailDomain(%q) = %q, %v; want %q", given, got, err, want)
		}
	}
}

func TestCleanEmailDomainRefusesAddressesAndURLs(t *testing.T) {
	for _, given := range []string{"ada@example.ch", "https://example.ch/", "exam ple.ch", "-example.ch"} {
		if got, err := CleanEmailDomain(given); err == nil {
			t.Errorf("CleanEmailDomain(%q) = %q, want an error", given, got)
		}
	}
}
