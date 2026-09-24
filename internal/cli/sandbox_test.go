package cli

import "testing"

// ssh hands a ProxyCommand the whole hostname typed, so behind a Host pattern
// of "sbx-*" the command is asked about "sbx-fix-login", not "fix-login".
func TestSandboxRefAcceptsTheSSHHostAlias(t *testing.T) {
	cases := map[string]string{
		"fix-login":         "fix-login",
		"sbx-fix-login":     "fix-login",
		"sbx_06c1k2rt8g3m4": "sbx_06c1k2rt8g3m4",
		// The prefix is stripped once, so a sandbox called "sbx-thing" is
		// reached as "sbx-sbx-thing".
		"sbx-sbx-thing": "sbx-thing",
		"":              "",
	}
	for in, want := range cases {
		if got := sandboxRef(in); got != want {
			t.Errorf("sandboxRef(%q) = %q, want %q", in, got, want)
		}
	}
}
