package webui

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// nameCases are the names the panel checks as they are typed, and the server
// checks again. The panel's copy of each rule is held to the same cases by
// test/ui.test.mjs, so the two cannot drift apart.
type nameCases map[string]struct {
	Valid   []string `json:"valid"`
	Invalid []string `json:"invalid"`
}

func TestTheServerAndThePanelShareTheNameRules(t *testing.T) {
	raw, err := os.ReadFile("testdata/names.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases nameCases
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	rules := map[string]func(string) bool{
		"alias":   policy.ValidAlias,
		"model":   policy.ValidModelAlias,
		"sandbox": policy.ValidSandboxName,
	}
	for kind, valid := range rules {
		c, ok := cases[kind]
		if !ok {
			t.Fatalf("testdata/names.json has no %q cases", kind)
		}
		for _, name := range c.Valid {
			if !valid(name) {
				t.Errorf("%s: %q is refused, but the cases say it is valid", kind, name)
			}
		}
		for _, name := range c.Invalid {
			if valid(name) {
				t.Errorf("%s: %q is accepted, but the cases say it is not", kind, name)
			}
		}
	}
}
