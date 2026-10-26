package policy

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// A sandbox is a machine the gateway lends out. This file holds the classes an
// organisation offers and how much a scope may hold. internal/sandbox runs
// them. See docs/sandboxes.md.

// Isolation is how firmly a sandbox is separated from the node it runs on.
//
// It states intent rather than a runtime name, because clusters name their
// gVisor RuntimeClass differently. A deployment maps the three tiers to its
// own names, so the catalogue stays portable.
type Isolation string

const (
	// IsolationStandard is the usual container runtime, hardened: non-root, no
	// privilege escalation, no capabilities. The boundary is the host kernel,
	// which suits a developer's own code but not a model's output.
	IsolationStandard Isolation = "standard"
	// IsolationIsolated puts a userspace kernel (gVisor) in front of the
	// host's, so a kernel bug hits a process, not the machine. It slows
	// syscall-heavy work and lacks some features, but needs no special
	// hardware.
	IsolationIsolated Isolation = "isolated"
	// IsolationVM gives the sandbox its own kernel in a VM (Kata Containers).
	// It costs a second or two of boot and about 100 MB, and needs hardware
	// virtualisation, which nodes that are VMs may not have.
	IsolationVM Isolation = "vm"
)

// Isolations is every tier, weakest first.
var Isolations = []Isolation{IsolationStandard, IsolationIsolated, IsolationVM}

// Valid reports whether i names a tier. Empty does not: nobody should guess a
// sandbox's isolation. catalog.ParseSandbox fills it in first.
func (i Isolation) Valid() bool { return slices.Contains(Isolations, i) }

// Purpose is who a sandbox was made for. It decides the lifecycle: idle time,
// what happens then, whether anyone may attach, and whether its requests form
// one task or a day's work.
type Purpose string

const (
	// PurposeEngineer is a person's own machine. People attach to it, and it
	// is suspended, not destroyed, when idle, because the volume holds their
	// work.
	PurposeEngineer Purpose = "engineer"
	// PurposeAgent is one task's machine. Nothing attaches to it, it is
	// terminated when its time runs out, and it states its own session id.
	// See docs/sessions.md.
	PurposeAgent Purpose = "agent"
)

var Purposes = []Purpose{PurposeEngineer, PurposeAgent}

func (p Purpose) Valid() bool { return slices.Contains(Purposes, p) }

// Attachable reports whether a person may open a shell. An agent sandbox
// promises nothing left it but a branch, and a shell would break that.
func (p Purpose) Attachable() bool { return p != PurposeAgent }

// SandboxClass is one entry in an organisation's sandbox catalogue. Like a
// model alias, people use the name, so what is behind it can change.
type SandboxClass struct {
	OrgID       string `json:"org_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Image should have the toolchain installed: installing on start is slow,
	// and impossible on an air-gapped site.
	Image     string    `json:"image"`
	Isolation Isolation `json:"isolation"`
	// CPU is in millicores and Memory in mebibytes. Disk is the persistent
	// volume in mebibytes; zero keeps nothing across a suspend.
	CPU    int `json:"cpu_millis"`
	Memory int `json:"memory_mib"`
	Disk   int `json:"disk_mib"`
	// DefaultTTLSeconds applies when nobody asks for a lifetime, and
	// MaxTTLSeconds caps what anybody may ask for.
	DefaultTTLSeconds int `json:"default_ttl_seconds"`
	MaxTTLSeconds     int `json:"max_ttl_seconds"`
	// Warm is how many idle sandboxes are kept started, so asking for one is
	// instant. They cost resources all the time.
	Warm int `json:"warm"`
	// Purposes are what this class may be asked for. Empty means both.
	Purposes  []Purpose `json:"purposes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c SandboxClass) DefaultTTL() time.Duration {
	return time.Duration(c.DefaultTTLSeconds) * time.Second
}

func (c SandboxClass) MaxTTL() time.Duration {
	return time.Duration(c.MaxTTLSeconds) * time.Second
}

func (c SandboxClass) Allows(p Purpose) bool {
	return len(c.Purposes) == 0 || slices.Contains(c.Purposes, p)
}

// TTLFor caps want at MaxTTL. Zero want is the class's default.
func (c SandboxClass) TTLFor(want time.Duration) time.Duration {
	if want <= 0 {
		want = c.DefaultTTL()
	}
	if ceiling := c.MaxTTL(); ceiling > 0 && want > ceiling {
		want = ceiling
	}
	return want
}

// maxSandboxName is short because the name is also a hostname and part of an
// ssh config Host pattern.
const maxSandboxName = 40

// maxClassName fits a Kubernetes label value (63), with 15 to spare for the
// warm pool's name.
const maxClassName = 40

func ValidClassName(s string) bool { return validName(s, maxClassName) }

// ValidSandboxName reports whether s can name a sandbox. It becomes a DNS
// label.
func ValidSandboxName(s string) bool { return validName(s, maxSandboxName) }

// SandboxState is where one sandbox is in its life.
type SandboxState string

const (
	// SandboxPending is not yet usable. If it stays, the row's Detail says why.
	SandboxPending SandboxState = "pending"
	SandboxReady   SandboxState = "ready"
	// SandboxSuspended has released its compute and kept its volume.
	SandboxSuspended SandboxState = "suspended"
	// SandboxExpired is an engineer's sandbox that ran out of lifetime and lost
	// its key, but keeps its volume. Resuming gives it a new key and lifetime.
	SandboxExpired SandboxState = "expired"
	// SandboxFailed means the driver gave up.
	SandboxFailed SandboxState = "failed"
	// SandboxTerminated is gone. The row stays so its cost still resolves.
	SandboxTerminated SandboxState = "terminated"
)

// Live reports whether this state holds resources, which a quota counts. A
// failed engineer sandbox keeps its volume too; store.Sandbox.Live adds that.
func (s SandboxState) Live() bool {
	return s == SandboxPending || s == SandboxReady || s == SandboxSuspended ||
		s == SandboxExpired
}

// Running reports whether compute is allocated. A suspended sandbox is live
// but not running.
func (s SandboxState) Running() bool {
	return s == SandboxPending || s == SandboxReady
}

// Final reports whether nothing more will happen to this sandbox on its own.
func (s SandboxState) Final() bool {
	return s == SandboxTerminated || s == SandboxFailed
}

// SandboxLimits is the part of Limits about sandboxes. They combine like every
// other limit.
type SandboxLimits struct {
	// MaxSandboxes counts live sandboxes. Zero means unlimited, as for every
	// ceiling here.
	MaxSandboxes         *int `json:"max_sandboxes,omitempty"`
	MaxSandboxTTLSeconds *int `json:"max_sandbox_ttl_seconds,omitempty"`
	// SandboxClasses nil means every class.
	SandboxClasses []string `json:"sandbox_classes"`
	// A class above MaxSandboxCPU or MaxSandboxMemory is refused, not shrunk.
	MaxSandboxCPU    *int `json:"max_sandbox_cpu_millis,omitempty"`
	MaxSandboxMemory *int `json:"max_sandbox_memory_mib,omitempty"`
	// AllowedRepos are forge paths a sandbox may check out: "bankb/core" for
	// one, "bankb" for all under it, or "*" for any. Nil inherits. Unlike the
	// other lists, if no level sets it nothing is allowed: one forge
	// credential can reach every tenant's repositories.
	AllowedRepos []string `json:"allowed_repos"`
}

// IsZero works like Limits.IsZero.
func (l SandboxLimits) IsZero() bool { return reflect.DeepEqual(l, SandboxLimits{}) }

// ResolvedSandbox is the sandbox part of Resolved.
type ResolvedSandbox struct {
	// The ceilings are zero for unlimited.
	MaxSandboxes         int      `json:"max_sandboxes"`
	MaxSandboxTTLSeconds int      `json:"max_sandbox_ttl_seconds"`
	SandboxClasses       []string `json:"sandbox_classes"`
	MaxSandboxCPU        int      `json:"max_sandbox_cpu_millis"`
	MaxSandboxMemory     int      `json:"max_sandbox_memory_mib"`
	// AllowedRepos nil means no repository, not every one.
	AllowedRepos []string `json:"allowed_repos"`
}

func (r ResolvedSandbox) allowsClass(name string) bool {
	return r.SandboxClasses == nil || slices.Contains(r.SandboxClasses, name)
}

// MaxTTL is zero for no limit.
func (r ResolvedSandbox) MaxTTL() time.Duration {
	return time.Duration(r.MaxSandboxTTLSeconds) * time.Second
}

// Admits says why this scope may not run class c, or nil. The message names
// the ceiling and what was asked for.
func (r ResolvedSandbox) Admits(c SandboxClass) error {
	if !r.allowsClass(c.Name) {
		return fmt.Errorf("the sandbox class %q is not allowed here; this scope may use %s",
			c.Name, strings.Join(r.SandboxClasses, ", "))
	}
	if r.MaxSandboxCPU > 0 && c.CPU > r.MaxSandboxCPU {
		return fmt.Errorf("the class %q asks for %s of CPU and this scope is capped at %s",
			c.Name, FormatCores(c.CPU), FormatCores(r.MaxSandboxCPU))
	}
	if r.MaxSandboxMemory > 0 && c.Memory > r.MaxSandboxMemory {
		return fmt.Errorf("the class %q asks for %s of memory and this scope is capped at %s",
			c.Name, FormatMiB(c.Memory), FormatMiB(r.MaxSandboxMemory))
	}
	return nil
}

// anyRepo in AllowedRepos allows every repository the forge credential reaches.
const anyRepo = "*"

// AdmitsRepo says why a sandbox here may not check out path, or nil.
func (r ResolvedSandbox) AdmitsRepo(path string) error {
	if r.AllowedRepos == nil {
		return errors.New("no repository is allowed for sandboxes here; an operator sets " +
			"'allowed_repos' on the organisation's guardrail")
	}
	for _, p := range r.AllowedRepos {
		if repoCovers(p, path) {
			return nil
		}
	}
	if len(r.AllowedRepos) == 0 {
		return errors.New("no repository is allowed for sandboxes in this scope")
	}
	return fmt.Errorf("%s is not allowed here; this scope may check out %s",
		path, strings.Join(r.AllowedRepos, ", "))
}

// ValidRepoPattern reports whether s can be an AllowedRepos entry.
func ValidRepoPattern(s string) bool {
	if s == anyRepo {
		return true
	}
	for p := range strings.SplitSeq(s, "/") {
		if p == "" || p == "." || p == ".." || strings.Trim(p, "abcdefghijklmnopqrstuvwxyz"+
			"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" {
			return false
		}
	}
	return true
}

// repoCovers ignores case, as forges do.
func repoCovers(pattern, path string) bool {
	if pattern == anyRepo {
		return true
	}
	pattern, path = strings.ToLower(pattern), strings.ToLower(path)
	return path == pattern || strings.HasPrefix(path, pattern+"/")
}

// narrowRepos: nil cur allows nothing, so a lower level cannot add to it.
func narrowRepos(cur, next []string) []string {
	if cur == nil || next == nil {
		return cur
	}
	return narrowList(cur, next, repoCovers)
}

func FormatCores(m int) string {
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%.1f", float64(m)/1000)
}

// FormatMiB uses Gi when it divides evenly.
func FormatMiB(m int) string {
	if m >= 1024 && m%1024 == 0 {
		return fmt.Sprintf("%dGi", m/1024)
	}
	return fmt.Sprintf("%dMi", m)
}

// narrow applies one level's sandbox limits. Only the organisation (top)
// grants repositories: one forge credential reaches every tenant's.
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
