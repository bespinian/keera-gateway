package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestPodmanKeepsSecretsOffItsCommandLine(t *testing.T) {
	// Every user of the host can read a process's arguments, so a key or a
	// token there would be anybody's.
	p := &Podman{}
	spec := Spec{Env: map[string]string{
		"KEERA_API_KEY": "keera_sk_secret", "KEERA_GIT_TOKEN": "ghs_secret",
		"KEERA_TASK": "fix the login", "KEERA_SANDBOX_NAME": "desk",
	}}
	args, env := p.runArgs(spec, "")
	line := strings.Join(args, " ")
	for _, secret := range []string{"keera_sk_secret", "ghs_secret", "fix the login"} {
		if strings.Contains(line, secret) {
			t.Errorf("the command line holds %q: %s", secret, line)
		}
	}
	if !slices.Contains(args, "KEERA_API_KEY") || !slices.Contains(env, "KEERA_API_KEY=keera_sk_secret") {
		t.Errorf("the key is not handed to podman through its environment: %v / %v", args, env)
	}
	if !slices.Contains(args, "KEERA_SANDBOX_NAME=desk") {
		t.Errorf("an ordinary value should stay on the command line: %s", line)
	}
}
