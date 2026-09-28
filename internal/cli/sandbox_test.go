package cli

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestSandboxApplyPutsEveryClassInTheFile(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"PUT /v1/sandbox-classes/standard": map[string]any{"name": "standard"},
		"PUT /v1/sandbox-classes/agent":    map[string]any{"name": "agent"},
	})
	path := filepath.Join(t.TempDir(), "sandboxes.yaml")
	file := "sandboxes:\n" +
		"  - name: standard\n" +
		"    image: registry.internal/keera/sandbox-base:1\n" +
		"    memory: 16Gi\n" +
		"  - name: agent\n" +
		"    image: registry.internal/keera/sandbox-agent:1\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sandboxCmd(context.Background(), []string{"apply", path, "--org", "org_1"}); err != nil {
		t.Fatal(err)
	}
	req := f.request("PUT", "/v1/sandbox-classes/standard")
	// The class goes parsed, in the shape the control plane lists it in.
	if got := req.body["memory_mib"]; got != float64(16384) {
		t.Errorf("memory_mib = %v, want the file's 16Gi", got)
	}
	if req.query != "org_id=org_1" {
		t.Errorf("query = %q, want the organisation", req.query)
	}
	f.request("PUT", "/v1/sandbox-classes/agent")
}

func TestSandboxDeleteClassDeletesTheOrganisationsClass(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{
		"DELETE /v1/sandbox-classes/standard": map[string]any{
			"name": "standard", "deleted": true, "live_sandboxes": 2,
		},
	})
	if err := sandboxCmd(context.Background(),
		[]string{"delete-class", "standard", "--org", "org_1", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if req := f.request("DELETE", "/v1/sandbox-classes/standard"); req.query != "org_id=org_1" {
		t.Errorf("query = %q, want the organisation", req.query)
	}
}

func TestSandboxApplyChecksTheFileBeforeItWritesAnything(t *testing.T) {
	quiet(t)
	f := newFakeControl(t, map[string]any{})
	path := filepath.Join(t.TempDir(), "sandboxes.yaml")
	// The second entry has no image, so neither is applied.
	file := "sandboxes:\n" +
		"  - name: standard\n" +
		"    image: registry.internal/keera/sandbox-base:1\n" +
		"  - name: broken\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sandboxCmd(context.Background(), []string{"apply", path, "--org", "org_1"}); err == nil {
		t.Fatal("a catalogue with a broken entry was applied")
	}
	if len(f.seen) != 0 {
		t.Errorf("the control plane was called anyway: %v", f.seen)
	}
}

// A bool flag passed on as "--json true" would leave "true" behind as a
// second name, and `up` would refuse it.
func TestAgentFlagsSurviveTheParser(t *testing.T) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	class := fs.String("class", "", "")
	fs.String("purpose", "", "")
	if err := parse(fs, []string{"fix", "--json", "--class", "small", "--purpose", "agent"}); err != nil {
		t.Fatal(err)
	}
	again := flag.NewFlagSet("up", flag.ContinueOnError)
	asJSON2 := again.Bool("json", false, "")
	class2 := again.String("class", "", "")
	if err := parse(again, append([]string{"fix"}, agentFlags(fs)...)); err != nil {
		t.Fatal(err)
	}
	if again.NArg() != 1 || again.Arg(0) != "fix" {
		t.Fatalf("args = %q, want only the name", again.Args())
	}
	if *asJSON2 != *asJSON || *class2 != *class {
		t.Fatalf("json=%v class=%q, want %v %q", *asJSON2, *class2, *asJSON, *class)
	}
}

// ssh runs the ProxyCommand as a new process, without this invocation's
// --url, so the command has to name the gateway itself. Otherwise the proxy
// could reach whichever gateway was signed in to last.
func TestProxyCommandNamesTheGateway(t *testing.T) {
	cmd := proxyCommand("/usr/bin/keera", "https://keera.example.ch", "%n", "org_1")
	want := "/usr/bin/keera --url https://keera.example.ch sandbox proxy %n --org org_1"
	if cmd != want {
		t.Errorf("proxy command = %q, want %q", cmd, want)
	}
	// And the command it names parses back to that gateway.
	t.Cleanup(func() { urlFlag = "" })
	rest, err := takeURL(strings.Fields(cmd)[1:])
	if err != nil {
		t.Fatal(err)
	}
	if urlFlag != "https://keera.example.ch" {
		t.Errorf("--url = %q, want the gateway", urlFlag)
	}
	if strings.Join(rest, " ") != "sandbox proxy %n --org org_1" {
		t.Errorf("left for the command: %q", rest)
	}
}
