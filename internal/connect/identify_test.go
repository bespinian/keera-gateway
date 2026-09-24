package connect

import "strings"

import "testing"

func TestIdentifyReadsTheAgentOffTheWire(t *testing.T) {
	tests := []struct {
		name   string
		stated string
		ua     string
		want   string
	}{
		{
			name: "a client that names itself is taken at its word",
			// This is the whole point of the header: what arrives at the
			// gateway from the panel's playground is a browser.
			stated: "keera-playground",
			ua:     "Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/128.0",
			want:   "keera-playground",
		},
		{
			name: "the comment on a product token is not the product",
			ua:   "claude-cli/1.0.83 (external, cli)",
			want: "claude-code",
		},
		{
			name: "a client that names its runtime after itself is still itself",
			ua:   "opencode/0.4.2 node/22.3.0 undici/6.19.2",
			want: "opencode",
		},
		{
			name: "a runtime named first does not win over a client named second",
			ua:   "node/22.3.0 opencode/0.4.2",
			want: "opencode",
		},
		{
			name: "an SDK is a client too",
			ua:   "OpenAI/Python 1.40.0",
			want: "openai-sdk",
		},
		{
			// The fallback is what keeps this table from having to know about
			// an editor released next month.
			name: "an unrecognised agent keeps its own name",
			ua:   "SomeNewEditor/2.0 (linux)",
			want: "someneweditor",
		},
		{
			name: "a client that says nothing is a client that says nothing",
			ua:   "",
			want: "",
		},
		{
			// A product token is matched whole. Anything else would file every
			// OpenAPI-generated client under Pi.
			name: "a name that merely contains a known one is not that one",
			ua:   "openapi-generator/7.1.0",
			want: "openapi-generator",
		},
		{
			name:   "a stated name is bounded and reduced like any other",
			stated: "  My Editor/1.0!!  ",
			want:   "myeditor",
		},
		{
			name:   "a stated name of nothing but punctuation is no name",
			stated: "---",
			ua:     "curl/8.9.1",
			want:   "curl",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Identify(tc.stated, tc.ua); got != tc.want {
				t.Errorf("Identify(%q, %q) = %q, want %q", tc.stated, tc.ua, got, tc.want)
			}
		})
	}
}

func TestAStatedNameCannotGrowWithoutBound(t *testing.T) {
	var long strings.Builder
	for range 200 {
		long.WriteString("a")
	}
	got := Identify(long.String(), "")
	if len(got) != maxClientName {
		t.Errorf("a %d-character name was stored as %d characters, want %d",
			len(long.String()), len(got), maxClientName)
	}
}

func TestClientLabelAgreesWithTheCatalogue(t *testing.T) {
	// The catalogue configures these and the map names them. Two labels for one
	// client is two clients as far as anybody reading a screen is concerned.
	for _, c := range Clients() {
		if got := ClientLabel(c.Key); got != c.Label {
			t.Errorf("ClientLabel(%q) = %q, but the catalogue calls it %q", c.Key, got, c.Label)
		}
	}
	if got := ClientLabel("codex"); got != "Codex" {
		t.Errorf("ClientLabel(codex) = %q, want its own label", got)
	}
	// An unrecognised client is shown under the name it gave, which is more
	// use than "other".
	if got := ClientLabel("someneweditor"); got != "someneweditor" {
		t.Errorf("ClientLabel of an unknown client = %q, want the key itself", got)
	}
	if got := ClientLabel(""); got != "Unidentified" {
		t.Errorf("ClientLabel(\"\") = %q, want a name for the nameless", got)
	}
}

func TestEveryAgentIsReachableByItsOwnKey(t *testing.T) {
	// A key that Identify cannot produce is a row nothing can ever be filed
	// under - and the stated header is written by hand, so the keys have to be
	// the names people would state.
	for _, a := range agents {
		if got := Identify(a.key, ""); got != a.key {
			t.Errorf("a client stating %q is filed as %q", a.key, got)
		}
	}
}
