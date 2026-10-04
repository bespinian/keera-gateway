package policy

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// A sandbox is a machine the gateway lends out. This file is its part of the
// tenancy model: the classes an organisation offers, and how much of them a
// scope may hold. internal/sandbox runs them. See docs/sandboxes.md.

// Isolation is how firmly a sandbox is separated from the node it runs on.
//
// It states intent rather than a runtime name, because clusters name their
// gVisor RuntimeClass differently ("gvisor", "runsc", "sandboxed"). A
// deployment maps these three onto its own names once, so the catalogue stays
// portable. Each tier costs more than the one before.
type Isolation string

const (
	// IsolationStandard is the cluster's usual container runtime, with the
	// hardening every sandbox gets: a non-root user, no privilege escalation,
	// no capabilities. The boundary is the host kernel, which suits a
	// developer's own code but not running a model's output.
	IsolationStandard Isolation = "standard"
	// IsolationIsolated puts a userspace kernel (gVisor) in front of the
	// host's, so a kernel bug reached from inside hits a process, not the
	// machine. It slows syscall-heavy work and lacks io_uring, some FUSE
	// mounts and nested containers, but needs no special hardware, so every
	// deployment can offer it.
	IsolationIsolated Isolation = "isolated"
	// IsolationVM gives the sandbox its own kernel in a virtual machine (Kata
	// Containers). It costs a second or two of boot and about 100 MB per
	// sandbox, and needs hardware virtualisation, which nodes that are VMs
	// themselves may not have. That is why it is not the only tier.
	IsolationVM Isolation = "vm"
)

// Isolations is every tier, weakest first.
var Isolations = []Isolation{IsolationStandard, IsolationIsolated, IsolationVM}

// Valid reports whether i names a tier. The empty string does not, because
// nobody should have to guess a sandbox's isolation. catalog.ParseSandbox fills it
// in first.
func (i Isolation) Valid() bool { return slices.Contains(Isolations, i) }

// Purpose is who a sandbox was made for. It decides the lifecycle: how long it
// may sit idle, what happens then, whether anyone may attach, and whether its
// requests form one task or a day's work.
type Purpose string

const (
	// PurposeEngineer is a person's own machine. People attach to it, it is
	// suspended rather than destroyed when idle because the volume holds their
	// work, and it states no session because its requests are a day's work.
	PurposeEngineer Purpose = "engineer"
	// PurposeAgent is one task's machine. Nothing attaches to it, it is
	// terminated when its time runs out, and it states its own session id
	// because it is one task. See docs/sessions.md.
	PurposeAgent Purpose = "agent"
)

// Purposes is every purpose.
var Purposes = []Purpose{PurposeEngineer, PurposeAgent}

// Valid reports whether p names a purpose.
func (p Purpose) Valid() bool { return slices.Contains(Purposes, p) }

// Attachable reports whether a person may open a shell on this sandbox. Only on
// an engineer's: an agent sandbox promises that nothing left it but a branch,
// and an unrecorded shell session would break that promise.
func (p Purpose) Attachable() bool { return p != PurposeAgent }

// SandboxClass is one entry in an organisation's sandbox catalogue: a machine a
// developer or an agent can ask for by name.
//
// Like a model alias, the name goes into command lines and repository
// configuration, so an administrator can change what is behind it without
// anyone editing anything.
type SandboxClass struct {
	// OrgID is the organisation the class belongs to. Only its people and
	// agents can ask for it.
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
	// Description says why to pick this class over another.
	Description string `json:"description,omitempty"`
	// Image is the OCI image the sandbox runs. It should have the toolchain
	// already installed: installing on start is slow, and impossible on an
	// air-gapped site.
	Image     string    `json:"image"`
	Isolation Isolation `json:"isolation"`
	// CPU is in millicores and Memory in mebibytes, the units both drivers
	// share. Disk is the persistent volume in mebibytes; zero keeps nothing
	// across a suspend.
	CPU    int `json:"cpu_millis"`
	Memory int `json:"memory_mib"`
	Disk   int `json:"disk_mib"`
	// DefaultTTL applies when nobody asks for a lifetime, and MaxTTL caps what
	// anybody may ask for before policy narrows it. Without an expiry,
	// somebody has to remember to terminate the machine.
	DefaultTTL time.Duration `json:"default_ttl"`
	MaxTTL     time.Duration `json:"max_ttl"`
	// Warm is how many idle sandboxes are kept started so asking for one is
	// instant. They cost resources all the time. Zero is the default.
	Warm int `json:"warm"`
	// Purposes are what this class may be asked for. Empty means both, so a
	// deployment can put agents on the VM tier and people on a cheaper one.
	Purposes  []Purpose `json:"purposes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Allows reports whether this class may be asked for on behalf of p.
func (c SandboxClass) Allows(p Purpose) bool {
	return len(c.Purposes) == 0 || slices.Contains(c.Purposes, p)
}

// TTLFor returns the lifetime a sandbox of this class gets when asked for
// want, capped at the class's MaxTTL. Zero want is the class's default.
func (c SandboxClass) TTLFor(want time.Duration) time.Duration {
	if want <= 0 {
		want = c.DefaultTTL
	}
	if c.MaxTTL > 0 && want > c.MaxTTL {
		want = c.MaxTTL
	}
	return want
}

// maxSandboxName is shorter than a class name's limit, because a sandbox name
// is also a hostname in the cluster and part of an ssh config Host pattern.
const maxSandboxName = 40

// ValidSandboxName reports whether s is a name a sandbox may have. It becomes
// a DNS label in the cluster and what a developer types after
// `keera sandbox ssh`.
func ValidSandboxName(s string) bool { return validName(s, maxSandboxName) }

// SandboxState is where one sandbox is in its life. It is one enum so every
// caller has one column to read.
type SandboxState string

const (
	// SandboxPending is asked for and not yet usable: scheduling, pulling,
	// booting. If it stays here, the row's Detail says why.
	SandboxPending SandboxState = "pending"
	// SandboxReady is up, reachable and inside its lifetime.
	SandboxReady SandboxState = "ready"
	// SandboxSuspended has released its compute and kept its volume. Resuming
	// reverses it.
	SandboxSuspended SandboxState = "suspended"
	// SandboxExpired has run out of lifetime and lost its key, but keeps its
	// volume. This is an engineer's sandbox, whose owner has not said they are
	// done. Resuming gives it a new key and a new lifetime.
	SandboxExpired SandboxState = "expired"
	// SandboxFailed means the driver gave up: the pod died, the node went
	// away, the claim could not be met.
	SandboxFailed SandboxState = "failed"
	// SandboxTerminated is gone, with the row kept so its cost and purpose
	// still resolve.
	SandboxTerminated SandboxState = "terminated"
)

// Live reports whether this state still holds resources: what a quota counts
// and a bill is computed from. A failed engineer sandbox holds its volume
// too, which store.Sandbox.Live adds, since that depends on the purpose.
func (s SandboxState) Live() bool {
	return s == SandboxPending || s == SandboxReady || s == SandboxSuspended ||
		s == SandboxExpired
}

// Running reports whether compute is allocated. A suspended sandbox is live
// but not running: it holds its volume and no CPU.
func (s SandboxState) Running() bool {
	return s == SandboxPending || s == SandboxReady
}

// Final reports whether nothing more will happen to this sandbox on its own.
func (s SandboxState) Final() bool {
	return s == SandboxTerminated || s == SandboxFailed
}

// SandboxLimits is the part of Limits about sandboxes: four ceilings and two
// allow-lists. They combine like every other limit: minimum for a ceiling,
// intersection for a list.
type SandboxLimits struct {
	// MaxSandboxes is how many live sandboxes this scope may hold at once.
	// Zero means unlimited, as for every ceiling here.
	MaxSandboxes *int `json:"max_sandboxes,omitempty"`
	// MaxSandboxTTLSeconds caps any one sandbox's lifetime. It is in seconds
	// because it passes through JSON, SQL and a form field.
	MaxSandboxTTLSeconds *int `json:"max_sandbox_ttl_seconds,omitempty"`
	// SandboxClasses is the allow-list of classes. Nil means every class, as
	// a nil AllowedModels means every model.
	SandboxClasses []string `json:"sandbox_classes"`
	// MaxSandboxCPU and MaxSandboxMemory cap one sandbox's size, in millicores
	// and mebibytes. A class above the cap is refused, not shrunk.
	MaxSandboxCPU    *int `json:"max_sandbox_cpu_millis,omitempty"`
	MaxSandboxMemory *int `json:"max_sandbox_memory_mib,omitempty"`
	// AllowedRepos is which repositories a sandbox may check out, by their
	// path on the forge: "bankb/core" for one, "bankb" for all under it, or
	// "*" for any. Nil inherits. Unlike the other lists, a scope where no level
	// sets it may check out nothing: one forge credential can reach every
	// tenant's repositories, so someone has to say which are whose.
	AllowedRepos []string `json:"allowed_repos"`
}

// IsZero reports whether l sets nothing.
func (l SandboxLimits) IsZero() bool {
	return l.MaxSandboxes == nil && l.MaxSandboxTTLSeconds == nil && l.SandboxClasses == nil &&
		l.MaxSandboxCPU == nil && l.MaxSandboxMemory == nil && l.AllowedRepos == nil
}

// ResolvedSandbox is the sandbox half of Resolved, with every level's limits
// combined.
type ResolvedSandbox struct {
	// MaxSandboxes and MaxSandboxTTLSeconds are zero for unlimited.
	MaxSandboxes         int      `json:"max_sandboxes"`
	MaxSandboxTTLSeconds int      `json:"max_sandbox_ttl_seconds"`
	SandboxClasses       []string `json:"sandbox_classes"`
	MaxSandboxCPU        int      `json:"max_sandbox_cpu_millis"`
	MaxSandboxMemory     int      `json:"max_sandbox_memory_mib"`
	// AllowedRepos nil means no repository, not every one.
	AllowedRepos []string `json:"allowed_repos"`
}

// AllowsClass reports whether name is inside the resolved allow-list.
func (r ResolvedSandbox) AllowsClass(name string) bool {
	return r.SandboxClasses == nil || slices.Contains(r.SandboxClasses, name)
}

// MaxTTL is the resolved lifetime ceiling, or zero for none.
func (r ResolvedSandbox) MaxTTL() time.Duration {
	return time.Duration(r.MaxSandboxTTLSeconds) * time.Second
}

// Admits reports whether this scope may run a sandbox of class c, and why not
// when it may not. The refused developer reads it, so it names the ceiling and
// what was asked for.
func (r ResolvedSandbox) Admits(c SandboxClass) error {
	if !r.AllowsClass(c.Name) {
		return fmt.Errorf("the sandbox class %q is not allowed here; this scope may use %s",
			c.Name, strings.Join(r.SandboxClasses, ", "))
	}
	if r.MaxSandboxCPU > 0 && c.CPU > r.MaxSandboxCPU {
		return fmt.Errorf("the class %q asks for %s of CPU and this scope is capped at %s",
			c.Name, millis(c.CPU), millis(r.MaxSandboxCPU))
	}
	if r.MaxSandboxMemory > 0 && c.Memory > r.MaxSandboxMemory {
		return fmt.Errorf("the class %q asks for %s of memory and this scope is capped at %s",
			c.Name, mib(c.Memory), mib(r.MaxSandboxMemory))
	}
	return nil
}

// AnyRepo in AllowedRepos allows every repository the forge credential reaches.
const AnyRepo = "*"

// AdmitsRepo reports whether a sandbox here may check out the repository at
// path on the forge, and why not when it may not.
func (r ResolvedSandbox) AdmitsRepo(path string) error {
	if r.AllowedRepos == nil {
		return fmt.Errorf("no repository is allowed for sandboxes here; an operator sets " +
			"'allowed_repos' on the organisation's guardrail")
	}
	for _, p := range r.AllowedRepos {
		if repoCovers(p, path) {
			return nil
		}
	}
	if len(r.AllowedRepos) == 0 {
		return fmt.Errorf("no repository is allowed for sandboxes in this scope")
	}
	return fmt.Errorf("%s is not allowed here; this scope may check out %s",
		path, strings.Join(r.AllowedRepos, ", "))
}

// ValidRepoPattern reports whether s can be an AllowedRepos entry.
func ValidRepoPattern(s string) bool {
	if s == AnyRepo {
		return true
	}
	parts := strings.SplitSeq(s, "/")
	for p := range parts {
		if p == "" || p == "." || p == ".." || strings.Trim(p, "abcdefghijklmnopqrstuvwxyz"+
			"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" {
			return false
		}
	}
	return true
}

// repoCovers reports whether pattern allows path: the same path, a path under
// it, or anything for "*". Forges ignore case in paths, so this does too.
func repoCovers(pattern, path string) bool {
	if pattern == AnyRepo {
		return true
	}
	pattern, path = strings.ToLower(pattern), strings.ToLower(path)
	return path == pattern || strings.HasPrefix(path, pattern+"/")
}

// narrowRepos combines two levels' repository lists. What is left is what
// both allow: of two entries where one covers the other, the narrower. Nil cur
// allows nothing, so a lower level cannot add to it.
func narrowRepos(cur, next []string) []string {
	if cur == nil || next == nil {
		return cur
	}
	out := []string{}
	for _, a := range cur {
		for _, b := range next {
			var keep string
			switch {
			case repoCovers(a, b):
				keep = b
			case repoCovers(b, a):
				keep = a
			default:
				continue
			}
			if !slices.Contains(out, keep) {
				out = append(out, keep)
			}
		}
	}
	return out
}

// millis renders millicores as cores.
func millis(m int) string {
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%.1f", float64(m)/1000)
}

// mib renders mebibytes, in gibibytes when it divides evenly.
func mib(m int) string {
	if m >= 1024 && m%1024 == 0 {
		return fmt.Sprintf("%dGi", m/1024)
	}
	return fmt.Sprintf("%dMi", m)
}

// narrow applies one level's sandbox limits to what the levels above allowed.
// Only the organisation's level, top, grants repositories: one forge
// credential reaches every tenant's, and a project must not reach past its own.
func (r *ResolvedSandbox) narrow(lim *Limits, top bool) {
	r.MaxSandboxes = minPositive(r.MaxSandboxes, deref(lim.MaxSandboxes))
	r.MaxSandboxTTLSeconds = minPositive(r.MaxSandboxTTLSeconds, deref(lim.MaxSandboxTTLSeconds))
	r.MaxSandboxCPU = minPositive(r.MaxSandboxCPU, deref(lim.MaxSandboxCPU))
	r.MaxSandboxMemory = minPositive(r.MaxSandboxMemory, deref(lim.MaxSandboxMemory))
	r.SandboxClasses = intersect(r.SandboxClasses, lim.SandboxClasses)
	if top {
		r.AllowedRepos = slices.Clone(lim.AllowedRepos)
	} else {
		r.AllowedRepos = narrowRepos(r.AllowedRepos, lim.AllowedRepos)
	}
}
