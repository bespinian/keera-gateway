package catalog

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// The sandbox catalogue works like the model catalogue: the file is a template,
// and each new organisation gets its own copy of every class in it.

// SandboxFile is the on-disk shape.
type SandboxFile struct {
	Sandboxes []Sandbox `yaml:"sandboxes"`
}

// Sandbox is one declared class.
//
// The resource fields are strings, so a file can say "4" and "16Gi" as
// Kubernetes files do. They are parsed and checked here, once. The control
// API takes the parsed policy.SandboxClass, the same shape it returns.
type Sandbox struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Image       string `yaml:"image"`
	// Isolation is standard, isolated or vm (see policy.Isolation). It
	// defaults to isolated.
	Isolation string `yaml:"isolation"`
	// CPU is a core count or a millicore quantity: "4" or "4000m".
	CPU string `yaml:"cpu"`
	// Memory and Disk are byte quantities: "16Gi", "512Mi". A bare number is
	// read as mebibytes.
	Memory     string   `yaml:"memory"`
	Disk       string   `yaml:"disk"`
	DefaultTTL string   `yaml:"default_ttl"`
	MaxTTL     string   `yaml:"max_ttl"`
	Warm       int      `yaml:"warm"`
	Purposes   []string `yaml:"purposes"`
}

// Defaults for what an entry does not say. Each one guards against a mistake:
// a class with no lifetime or ceiling is a machine somebody must remember to
// stop.
const (
	// DefaultSandboxTTL and DefaultSandboxMaxTTL also give a sandbox whose
	// class was deleted its lifetimes.
	DefaultSandboxTTL       = 4 * time.Hour
	DefaultSandboxMaxTTL    = 24 * time.Hour
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

func parseSandboxFile(raw []byte) ([]policy.SandboxClass, error) {
	var f SandboxFile
	if err := decodeStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("parse sandbox catalogue: %w", err)
	}
	if len(f.Sandboxes) == 0 {
		return nil, fmt.Errorf("catalogue declares no sandbox classes")
	}
	name := func(s Sandbox) string { return s.Name }
	return parseEntries(f.Sandboxes, "sandboxes", "name", name, ParseSandbox)
}

// ParseSandbox validates and expands a single declared class, so an entry on
// the command line means what the same entry in a file means.
func ParseSandbox(s Sandbox) (policy.SandboxClass, error) {
	switch {
	case s.Name == "":
		return policy.SandboxClass{}, fmt.Errorf("name is required")
	case !policy.ValidAlias(s.Name):
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
		Name:        s.Name,
		Description: strings.TrimSpace(s.Description),
		Image:       strings.TrimSpace(s.Image),
		Warm:        s.Warm,
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
	purposes, err := parsePurposes(s.Purposes)
	if err != nil {
		return policy.SandboxClass{}, err
	}
	c.Purposes = purposes
	return c, nil
}

// CheckSandboxClass holds a class sent to the control API to the rules a
// file's entry is held to, and fills in the same defaults for what is zero.
func CheckSandboxClass(c policy.SandboxClass) (policy.SandboxClass, error) {
	s := Sandbox{
		Name: c.Name, Description: c.Description, Image: c.Image,
		Isolation: string(c.Isolation), Warm: c.Warm,
	}
	// Zero is "not given", as an empty string is in a file.
	if c.CPU != 0 {
		s.CPU = strconv.Itoa(c.CPU) + "m"
	}
	if c.Memory != 0 {
		s.Memory = strconv.Itoa(c.Memory)
	}
	if c.Disk != 0 {
		s.Disk = strconv.Itoa(c.Disk)
	}
	if c.DefaultTTL != 0 {
		s.DefaultTTL = c.DefaultTTL.String()
	}
	if c.MaxTTL != 0 {
		s.MaxTTL = c.MaxTTL.String()
	}
	for _, p := range c.Purposes {
		s.Purposes = append(s.Purposes, string(p))
	}
	return ParseSandbox(s)
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
	if c.DefaultTTL, err = parseTTL(s.DefaultTTL, DefaultSandboxTTL); err != nil {
		return fmt.Errorf("default_ttl: %w", err)
	}
	if c.MaxTTL, err = parseTTL(s.MaxTTL, DefaultSandboxMaxTTL); err != nil {
		return fmt.Errorf("max_ttl: %w", err)
	}
	if c.MaxTTL < c.DefaultTTL {
		return fmt.Errorf(
			"max_ttl (%s) is shorter than default_ttl (%s); every sandbox of this class would "+
				"be created already past its ceiling", c.MaxTTL, c.DefaultTTL)
	}
	return nil
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

// LoadSandboxes reads and validates a sandbox catalogue file. Like the model
// file it is a template: each new organisation starts with a copy of every
// class it declares.
func LoadSandboxes(path string) ([]policy.SandboxClass, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	classes, err := parseSandboxFile(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return classes, nil
}
