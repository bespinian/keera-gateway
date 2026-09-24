package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

func TestParseSandboxes(t *testing.T) {
	classes, err := ParseSandboxes([]byte(`
sandboxes:
  - name: go-standard
    description: Go and the repo toolchain
    image: registry.internal/keera/sandbox-go:1
    isolation: vm
    cpu: "4"
    memory: 16Gi
    disk: 50Gi
    default_ttl: 4h
    max_ttl: 24h
    warm: 2
    egress: [gateway, git]
    purposes: [engineer]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(classes) != 1 {
		t.Fatalf("got %d classes, want 1", len(classes))
	}
	c := classes[0]
	switch {
	case c.Name != "go-standard":
		t.Errorf("name = %q", c.Name)
	case c.Isolation != policy.IsolationVM:
		t.Errorf("isolation = %q", c.Isolation)
	case c.CPU != 4000:
		t.Errorf("cpu = %d millicores, want 4000", c.CPU)
	case c.Memory != 16*1024:
		t.Errorf("memory = %d MiB, want 16384", c.Memory)
	case c.Disk != 50*1024:
		t.Errorf("disk = %d MiB, want 51200", c.Disk)
	case c.DefaultTTL != 4*time.Hour:
		t.Errorf("default_ttl = %s", c.DefaultTTL)
	case c.Warm != 2:
		t.Errorf("warm = %d", c.Warm)
	case len(c.Purposes) != 1 || c.Purposes[0] != policy.PurposeEngineer:
		t.Errorf("purposes = %v", c.Purposes)
	}
	// Everything ParseSandboxes produces came out of a file, which is what
	// makes it the file's to own.
	if !c.Managed {
		t.Error("a class from a catalogue file should be marked managed")
	}
}

func TestParseSandboxesDefaults(t *testing.T) {
	classes, err := ParseSandboxes([]byte(`
sandboxes:
  - name: minimal
    image: example/sandbox:1
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := classes[0]
	// Not "standard". A catalogue written without thinking about isolation
	// should get the tier that needs nothing from the hardware and does hold a
	// boundary, rather than the one every other pod on the node already has.
	if c.Isolation != policy.IsolationIsolated {
		t.Errorf("default isolation = %q, want isolated", c.Isolation)
	}
	if c.DefaultTTL == 0 || c.MaxTTL == 0 {
		t.Error("a class with no lifetime is a machine somebody has to remember " +
			"to terminate; both bounds must be defaulted")
	}
	// Zero disk is a real answer: an agent sandbox exists for one task and
	// pushes a branch at the end of it.
	if c.Disk != 0 {
		t.Errorf("disk = %d, want 0 for a class that declared none", c.Disk)
	}
}

func TestParseSandboxesRefusals(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{
			"no classes", "sandboxes: []", "declares no sandbox classes",
		},
		{
			"no name",
			"sandboxes:\n  - image: x\n",
			"name is required",
		},
		{
			"bad name",
			"sandboxes:\n  - name: Go_Standard\n    image: x\n",
			"lowercase letters",
		},
		{
			"no image",
			"sandboxes:\n  - name: a\n",
			"image is required",
		},
		{
			"duplicate",
			"sandboxes:\n  - name: a\n    image: x\n  - name: a\n    image: y\n",
			"declared twice",
		},
		{
			"unknown isolation",
			"sandboxes:\n  - name: a\n    image: x\n    isolation: kata\n",
			"is not one of",
		},
		{
			"unknown purpose",
			"sandboxes:\n  - name: a\n    image: x\n    purposes: [robot]\n",
			"'engineer' or",
		},
		{
			// The two bounds that make "ephemeral" mean something.
			"lifetime too short",
			"sandboxes:\n  - name: a\n    image: x\n    default_ttl: 10s\n",
			"still being pulled",
		},
		{
			"lifetime too long",
			"sandboxes:\n  - name: a\n    image: x\n    max_ttl: 2000h\n",
			"virtual machine with extra steps",
		},
		{
			"ceiling below default",
			"sandboxes:\n  - name: a\n    image: x\n    default_ttl: 12h\n    max_ttl: 6h\n",
			"already past its ceiling",
		},
		{
			"warm out of range",
			"sandboxes:\n  - name: a\n    image: x\n    warm: 500\n",
			"holds this class's whole CPU",
		},
		{
			"nonsense cpu",
			"sandboxes:\n  - name: a\n    image: x\n    cpu: lots\n",
			"core count",
		},
		{
			"nonsense memory",
			"sandboxes:\n  - name: a\n    image: x\n    memory: plenty\n",
			"16Gi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSandboxes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseCPU(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"", 2000, true}, // the default this test passes in
		{"4", 4000, true},
		{"0.5", 500, true},
		{"500m", 500, true},
		{"2500m", 2500, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"four", 0, false},
	}
	for _, tc := range cases {
		got, err := parseCPU(tc.in, 2000)
		if tc.ok && err != nil {
			t.Errorf("parseCPU(%q): %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseCPU(%q) should have failed", tc.in)
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseCPU(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseMiB(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"", 4096, true},
		{"16Gi", 16384, true},
		{"512Mi", 512, true},
		{"1Ti", 1048576, true},
		// A bare number is mebibytes, not bytes. The Kubernetes reading would
		// make "memory: 4096" four kilobytes, which is a class nothing can
		// schedule and an error nobody would guess at.
		{"4096", 4096, true},
		{"0", 0, false},
		{"16GB", 0, false},
	}
	for _, tc := range cases {
		got, err := parseMiB(tc.in, 4096)
		if tc.ok && err != nil {
			t.Errorf("parseMiB(%q): %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseMiB(%q) should have failed", tc.in)
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseMiB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseSandboxDeduplicates(t *testing.T) {
	// A repeated egress name or purpose is somebody writing the same thing
	// twice, not an instruction to do it twice.
	c, err := ParseSandbox(Sandbox{
		Name: "a", Image: "x",
		Egress:   []string{"git", "GIT", " git ", "gateway"},
		Purposes: []string{"agent", "agent"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Egress) != 2 {
		t.Errorf("egress = %v, want it folded to two", c.Egress)
	}
	if len(c.Purposes) != 1 {
		t.Errorf("purposes = %v, want one", c.Purposes)
	}
}
