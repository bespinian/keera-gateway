package catalog

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The sandbox catalogue works like the model catalogue. A class name is an API
// contract, typed on command lines and written into repositories, so the
// operator declares classes in a file and can change what is behind a name
// without anybody else editing anything.

// SandboxFile is the on-disk shape.
type SandboxFile struct {
	Sandboxes []Sandbox `yaml:"sandboxes"`
}

// Sandbox is one declared class.
//
// The resource fields are strings, so a file can say "4" and "16Gi" as
// Kubernetes files do. They are parsed and checked here, once.
//
// The JSON tags match the YAML tags because the control API accepts this struct
// directly, so a class typed into the panel means the same as one in the file.
type Sandbox struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Image       string `yaml:"image" json:"image"`
	// Isolation is standard, isolated or vm (see policy.Isolation). It
	// defaults to isolated.
	Isolation string `yaml:"isolation" json:"isolation"`
	// RuntimeClass overrides the deployment's own mapping for this class,
	// which makes the class specific to one cluster.
	RuntimeClass string `yaml:"runtime_class" json:"runtime_class"`
	// CPU is a core count or a millicore quantity: "4" or "4000m".
	CPU string `yaml:"cpu" json:"cpu"`
	// Memory and Disk are byte quantities: "16Gi", "512Mi". A bare number is
	// read as mebibytes.
	Memory     string   `yaml:"memory" json:"memory"`
	Disk       string   `yaml:"disk" json:"disk"`
	DefaultTTL string   `yaml:"default_ttl" json:"default_ttl"`
	MaxTTL     string   `yaml:"max_ttl" json:"max_ttl"`
	Warm       int      `yaml:"warm" json:"warm"`
	Egress     []string `yaml:"egress" json:"egress"`
	Purposes   []string `yaml:"purposes" json:"purposes"`
}

// Defaults for what an entry does not say. Each one guards against a mistake:
// a class with no lifetime or ceiling is a machine somebody must remember to
// stop.
const (
	defaultSandboxTTL       = 4 * time.Hour
	defaultSandboxMaxTTL    = 24 * time.Hour
	defaultSandboxCPUMillis = 2000
	defaultSandboxMemoryMiB = 4096
	// minSandboxTTL is the floor. A shorter sandbox could expire while its
	// image is still being pulled.
	minSandboxTTL = 5 * time.Minute
	// maxSandboxTTL is the ceiling: long enough for any task, but sandboxes
	// must not live for ever.
	maxSandboxTTL = 28 * 24 * time.Hour
	// maxWarm bounds a warm pool. Every warm sandbox holds the class's full
	// CPU and memory with nobody using it, so a typo here costs money.
	maxWarm = 32
)

// ParseSandboxes reads and validates a sandbox catalogue file.
func ParseSandboxes(raw []byte) ([]policy.SandboxClass, error) {
	var f SandboxFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse sandbox catalogue: %w", err)
	}
	if len(f.Sandboxes) == 0 {
		return nil, fmt.Errorf("catalogue declares no sandbox classes")
	}
	name := func(s Sandbox) string { return s.Name }
	return parseEntries(f.Sandboxes, "sandboxes", "name", name, func(s Sandbox) (policy.SandboxClass, error) {
		parsed, err := ParseSandbox(s)
		// The file owns what it declares, so only another apply may change it.
		parsed.Managed = true
		return parsed, err
	})
}

// ParseSandbox validates and expands a single declared class, so an entry on
// the command line means what the same entry in a file means.
func ParseSandbox(s Sandbox) (policy.SandboxClass, error) {
	switch {
	case s.Name == "":
		return policy.SandboxClass{}, fmt.Errorf("name is required")
	case !policy.ValidSandboxClass(s.Name):
		return policy.SandboxClass{}, fmt.Errorf("name %q must be lowercase letters, digits and "+
			"interior hyphens: it is typed on a command line and written into repositories' own "+
			"configuration, neither of which quotes it", s.Name)
	case strings.TrimSpace(s.Image) == "":
		return policy.SandboxClass{}, fmt.Errorf("image is required; it is what the sandbox " +
			"runs, and it should carry the toolchain already installed - a sandbox that " +
			"installs its own is one nobody waits for, and on an air-gapped site it is one " +
			"that never becomes ready")
	}

	c := policy.SandboxClass{
		Name:         s.Name,
		Description:  strings.TrimSpace(s.Description),
		Image:        strings.TrimSpace(s.Image),
		RuntimeClass: strings.TrimSpace(s.RuntimeClass),
		Warm:         s.Warm,
	}
	if err := parseIsolation(s, &c); err != nil {
		return policy.SandboxClass{}, err
	}
	if err := parseResources(s, &c); err != nil {
		return policy.SandboxClass{}, err
	}
	if err := parseLifetimes(s, &c); err != nil {
		return policy.SandboxClass{}, err
	}
	if c.Warm < 0 || c.Warm > maxWarm {
		return policy.SandboxClass{}, fmt.Errorf(
			"warm is %d; it is between 0 and %d, and every one of them holds this class's "+
				"whole CPU and memory allocation with nobody using it", c.Warm, maxWarm)
	}
	c.Egress = parseEgress(s.Egress)
	purposes, err := parsePurposes(s.Purposes)
	if err != nil {
		return policy.SandboxClass{}, err
	}
	c.Purposes = purposes
	return c, nil
}

func parseIsolation(s Sandbox, c *policy.SandboxClass) error {
	c.Isolation = policy.Isolation(strings.ToLower(strings.TrimSpace(s.Isolation)))
	if c.Isolation == "" {
		// Not "standard": a class written without thought for isolation should
		// still get a real boundary, and this tier needs nothing from the
		// hardware.
		c.Isolation = policy.IsolationIsolated
	}
	if !c.Isolation.Valid() {
		return fmt.Errorf("isolation %q is not one of %s", s.Isolation, joinIsolations())
	}
	return nil
}

func parseResources(s Sandbox, c *policy.SandboxClass) error {
	var err error
	if c.CPU, err = parseCPU(s.CPU, defaultSandboxCPUMillis); err != nil {
		return fmt.Errorf("cpu: %w", err)
	}
	if c.Memory, err = parseMiB(s.Memory, defaultSandboxMemoryMiB); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	// No disk by default: an agent sandbox lives for one task and needs no
	// state across a suspend.
	if c.Disk, err = parseMiB(s.Disk, 0); err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	return nil
}

func parseLifetimes(s Sandbox, c *policy.SandboxClass) error {
	var err error
	if c.DefaultTTL, err = parseTTL(s.DefaultTTL, defaultSandboxTTL); err != nil {
		return fmt.Errorf("default_ttl: %w", err)
	}
	if c.MaxTTL, err = parseTTL(s.MaxTTL, defaultSandboxMaxTTL); err != nil {
		return fmt.Errorf("max_ttl: %w", err)
	}
	if c.MaxTTL < c.DefaultTTL {
		return fmt.Errorf(
			"max_ttl (%s) is shorter than default_ttl (%s); every sandbox of this class would "+
				"be created already past its ceiling", c.MaxTTL, c.DefaultTTL)
	}
	return nil
}

// parseEgress lowercases the egress list and drops blanks and repeats.
func parseEgress(raw []string) []string {
	var out []string
	for _, e := range raw {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// parsePurposes checks the purposes list and drops blanks and repeats.
func parsePurposes(raw []string) ([]policy.Purpose, error) {
	var out []policy.Purpose
	for _, p := range raw {
		purpose := policy.Purpose(strings.ToLower(strings.TrimSpace(p)))
		if purpose == "" {
			continue
		}
		if !purpose.Valid() {
			return nil, fmt.Errorf("purposes names %q; it is 'engineer' or 'agent'", p)
		}
		if !slices.Contains(out, purpose) {
			out = append(out, purpose)
		}
	}
	return out, nil
}

func joinIsolations() string {
	return joinNames(policy.Isolations, func(i policy.Isolation) string { return string(i) })
}

// parseCPU reads a core count or a millicore quantity into millicores.
func parseCPU(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	if m, ok := strings.CutSuffix(raw, "m"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(m))
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q is not a positive millicore quantity such as 500m", raw)
		}
		return n, nil
	}
	cores, err := strconv.ParseFloat(raw, 64)
	if err != nil || cores <= 0 {
		return 0, fmt.Errorf("%q is not a core count such as 4, or a millicore quantity such "+
			"as 500m", raw)
	}
	return int(cores * 1000), nil
}

// parseMiB reads a byte quantity into mebibytes, with Kubernetes suffixes.
//
// A bare number is mebibytes, not bytes as in Kubernetes: "memory: 4096" meaning
// four kilobytes would be a class nothing can schedule.
func parseMiB(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	for _, unit := range []struct {
		suffix string
		mib    int
	}{{"Ti", 1 << 20}, {"Gi", 1 << 10}, {"Mi", 1}} {
		if v, ok := strings.CutSuffix(raw, unit.suffix); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("%q is not a positive quantity", raw)
			}
			return n * unit.mib, nil
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a quantity such as 16Gi, 512Mi, or a bare number of "+
			"mebibytes", raw)
	}
	return n, nil
}

// parseTTL reads a lifetime and holds it between minSandboxTTL and
// maxSandboxTTL.
func parseTTL(raw string, def time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 4h or 90m", raw)
	}
	switch {
	case d < minSandboxTTL:
		return 0, fmt.Errorf("%s is shorter than %s; a sandbox that expires while its image is "+
			"still being pulled reads as the platform being broken", d, minSandboxTTL)
	case d > maxSandboxTTL:
		return 0, fmt.Errorf("%s is longer than %s; a class whose sandboxes can outlive a month "+
			"is not an ephemeral sandbox, it is a virtual machine with extra steps", d, maxSandboxTTL)
	}
	return d, nil
}

// ApplySandboxes writes a declared sandbox catalogue to the database. As with
// models, classes the file does not mention are kept, and declared ones are
// marked as managed so only another apply may change them.
func ApplySandboxes(ctx context.Context, st *store.Store, path string) ([]policy.SandboxClass, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	classes, err := ParseSandboxes(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	declared := make([]string, 0, len(classes))
	for i := range classes {
		if err := st.UpsertSandboxClass(ctx, &classes[i]); err != nil {
			return nil, fmt.Errorf("apply %s: %w", classes[i].Name, err)
		}
		declared = append(declared, classes[i].Name)
	}
	if err := st.UnmanageSandboxClasses(ctx, declared); err != nil {
		return nil, fmt.Errorf("release sandbox classes the catalogue no longer declares: %w", err)
	}
	return classes, nil
}
