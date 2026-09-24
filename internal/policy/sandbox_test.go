package policy

import (
	"strings"
	"testing"
	"time"
)

func TestIsolationOrdering(t *testing.T) {
	// The ladder has to be a ladder, because a driver refuses a class it
	// cannot deliver by comparing the two - and a comparison that got this
	// backwards would run a class asking for a kernel of its own on the node's.
	cases := []struct {
		have, want Isolation
		ok         bool
	}{
		{IsolationVM, IsolationStandard, true},
		{IsolationVM, IsolationVM, true},
		{IsolationIsolated, IsolationStandard, true},
		{IsolationIsolated, IsolationVM, false},
		{IsolationStandard, IsolationIsolated, false},
		{IsolationStandard, IsolationStandard, true},
	}
	for _, c := range cases {
		if got := c.have.AtLeast(c.want); got != c.ok {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestIsolationValid(t *testing.T) {
	// The empty string is deliberately not a tier. A class that did not say
	// would be a class whose isolation is whatever the cluster defaults to,
	// which is the one property of a sandbox nobody should get by accident.
	if Isolation("").Valid() {
		t.Error("the empty string should not be a valid isolation tier")
	}
	if Isolation("kata").Valid() {
		t.Error("a runtime name should not be a valid isolation tier")
	}
	for _, i := range Isolations {
		if !i.Valid() {
			t.Errorf("%s should be valid", i)
		}
	}
}

func TestPurposeAttachable(t *testing.T) {
	if PurposeAgent.Attachable() {
		t.Error("nothing attaches to an agent sandbox: a shell nobody recorded " +
			"would make its one claim unprovable")
	}
	if !PurposeEngineer.Attachable() {
		t.Error("an engineer's sandbox is a machine they work in")
	}
}

func TestSandboxClassTTLFor(t *testing.T) {
	c := SandboxClass{DefaultTTL: 4 * time.Hour, MaxTTL: 24 * time.Hour}
	cases := []struct {
		want, got time.Duration
	}{
		{0, 4 * time.Hour},               // nothing asked for is the default
		{time.Hour, time.Hour},           // under the ceiling is honoured
		{48 * time.Hour, 24 * time.Hour}, // over it is clamped, not refused
		{24 * time.Hour, 24 * time.Hour}, // exactly the ceiling
		{-time.Hour, 4 * time.Hour},      // nonsense falls back to the default
	}
	for _, tc := range cases {
		if got := c.TTLFor(tc.want); got != tc.got {
			t.Errorf("TTLFor(%s) = %s, want %s", tc.want, got, tc.got)
		}
	}
}

func TestSandboxClassAllows(t *testing.T) {
	// An empty list is both purposes, not neither.
	open := SandboxClass{}
	for _, p := range Purposes {
		if !open.Allows(p) {
			t.Errorf("a class stating no purposes should allow %s", p)
		}
	}
	agentOnly := SandboxClass{Purposes: []Purpose{PurposeAgent}}
	if agentOnly.Allows(PurposeEngineer) {
		t.Error("a class restricted to agents should not be offered to people")
	}
	if !agentOnly.Allows(PurposeAgent) {
		t.Error("a class restricted to agents should be offered to agents")
	}
}

func TestResolvedSandboxAdmits(t *testing.T) {
	class := SandboxClass{Name: "big", CPU: 8000, Memory: 32768}

	t.Run("unrestricted", func(t *testing.T) {
		if err := (ResolvedSandbox{}).Admits(class); err != nil {
			t.Fatalf("a scope with no limits should admit anything: %v", err)
		}
	})

	t.Run("class not allowed", func(t *testing.T) {
		r := ResolvedSandbox{SandboxClasses: []string{"small", "medium"}}
		err := r.Admits(class)
		if err == nil {
			t.Fatal("a class outside the allow-list should be refused")
		}
		// The message is read by the developer who was refused, in a terminal,
		// with no idea a limit existed - so it has to name what they may use.
		if !strings.Contains(err.Error(), "small, medium") {
			t.Errorf("the refusal should say what is allowed instead: %v", err)
		}
	})

	t.Run("too many cores", func(t *testing.T) {
		r := ResolvedSandbox{MaxSandboxCPU: 4000}
		err := r.Admits(class)
		if err == nil {
			t.Fatal("a class over the CPU ceiling should be refused")
		}
		if !strings.Contains(err.Error(), "8") || !strings.Contains(err.Error(), "4") {
			t.Errorf("the refusal should name both numbers: %v", err)
		}
	})

	t.Run("too much memory", func(t *testing.T) {
		r := ResolvedSandbox{MaxSandboxMemory: 8192}
		err := r.Admits(class)
		if err == nil {
			t.Fatal("a class over the memory ceiling should be refused")
		}
		if !strings.Contains(err.Error(), "32Gi") {
			t.Errorf("the refusal should render mebibytes readably: %v", err)
		}
	})
}

func TestResolveSandboxIsRestrictOnly(t *testing.T) {
	// The whole point of the combining rule: delegating team administration
	// must not be usable to grant that team more than its organisation allowed.
	org := &Limits{SandboxLimits: SandboxLimits{
		MaxSandboxes:         new(4),
		MaxSandboxTTLSeconds: new(8 * 3600),
		SandboxClasses:       []string{"small", "medium", "big"},
		MaxSandboxCPU:        new(8000),
	}}
	team := &Limits{SandboxLimits: SandboxLimits{
		// Every one of these asks for more than the organisation allowed.
		MaxSandboxes:         new(99),
		MaxSandboxTTLSeconds: new(720 * 3600),
		SandboxClasses:       []string{"medium", "big", "enormous"},
		MaxSandboxCPU:        new(64000),
	}}

	got := Resolve(Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"}, org, team, nil).Sandbox

	if got.MaxSandboxes != 4 {
		t.Errorf("MaxSandboxes = %d, want the organisation's 4", got.MaxSandboxes)
	}
	if got.MaxSandboxTTLSeconds != 8*3600 {
		t.Errorf("MaxSandboxTTLSeconds = %d, want the organisation's", got.MaxSandboxTTLSeconds)
	}
	if got.MaxSandboxCPU != 8000 {
		t.Errorf("MaxSandboxCPU = %d, want the organisation's 8000", got.MaxSandboxCPU)
	}
	// Allow-lists intersect: "enormous" was never the organisation's to grant.
	want := []string{"medium", "big"}
	if len(got.SandboxClasses) != len(want) {
		t.Fatalf("SandboxClasses = %v, want %v", got.SandboxClasses, want)
	}
	for i, c := range want {
		if got.SandboxClasses[i] != c {
			t.Fatalf("SandboxClasses = %v, want %v", got.SandboxClasses, want)
		}
	}
}

func TestResolveSandboxNarrowsFromNothing(t *testing.T) {
	// A team may narrow what an organisation left unlimited. Restrict-only is
	// about the direction, not about whether the outer level said anything.
	team := &Limits{SandboxLimits: SandboxLimits{MaxSandboxes: new(2)}}
	got := Resolve(Key{ID: "key_1", OrgID: "org_1", TeamID: "team_1"}, nil, team, nil).Sandbox
	if got.MaxSandboxes != 2 {
		t.Errorf("MaxSandboxes = %d, want the team's 2", got.MaxSandboxes)
	}
}

func TestResolvedSandboxAllowsClass(t *testing.T) {
	// nil is every class, exactly as a nil AllowedModels is every model - the
	// identity element rather than the empty set.
	if !(ResolvedSandbox{}).AllowsClass("anything") {
		t.Error("a nil allow-list should permit every class")
	}
	r := ResolvedSandbox{SandboxClasses: []string{}}
	if r.AllowsClass("anything") {
		t.Error("an empty allow-list should permit nothing")
	}
}

func TestValidSandboxNames(t *testing.T) {
	// A sandbox name becomes a DNS label in the cluster and half of an ssh
	// config Host pattern on somebody's laptop, so it is held to a model
	// alias's shape.
	for _, ok := range []string{"a", "fix-login", "sandbox-2", "x1"} {
		if !ValidSandboxName(ok) {
			t.Errorf("%q should be a usable sandbox name", ok)
		}
	}
	for _, bad := range []string{
		"", "-leading", "trailing-", "Upper", "with space", "with_underscore",
		"with.dot", strings.Repeat("a", 41),
	} {
		if ValidSandboxName(bad) {
			t.Errorf("%q should not be a usable sandbox name", bad)
		}
	}
	// A class name may be longer, because it is not a hostname.
	if !ValidSandboxClass(strings.Repeat("a", 64)) {
		t.Error("a 64-character class name should be allowed")
	}
	if ValidSandboxClass(strings.Repeat("a", 65)) {
		t.Error("a 65-character class name should not be")
	}
}

func TestSandboxStateGroupings(t *testing.T) {
	// Live is what a quota counts; Running is what a bill is computed from.
	// Suspended is live and not running, which is the whole reason there are
	// two questions rather than one.
	if !SandboxSuspended.Live() {
		t.Error("a suspended sandbox still holds its volume, so it is live")
	}
	if SandboxSuspended.Running() {
		t.Error("a suspended sandbox holds no compute")
	}
	for _, s := range []SandboxState{SandboxExpired, SandboxFailed, SandboxTerminated} {
		if s.Live() {
			t.Errorf("%s should not count against a quota", s)
		}
	}
	if !SandboxTerminated.Final() || !SandboxFailed.Final() {
		t.Error("terminated and failed are where a sandbox stops")
	}
	if SandboxExpired.Final() {
		t.Error("an expired engineer's sandbox can still be resumed")
	}
}
