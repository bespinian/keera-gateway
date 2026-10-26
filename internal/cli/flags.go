package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// cmdRun is what every command starts with. A command declares its own flags
// on fs, then calls parse.
type cmdRun struct {
	c        *client
	fs       *flag.FlagSet
	cmd      string
	hasVerbs bool
	// verb is the verb as typed until parse, and its own name after.
	verb string
	args []string
	rest []string

	// org is --org, else the one 'keera org switch' chose, else empty.
	org string

	// Shared flags. help.go says which verbs take them.
	orgFlag string
	yes     bool
	asJSON  bool
}

// newCmdRun takes the verb off args and declares --org, --yes and --json.
func newCmdRun(cmd string, args []string) *cmdRun {
	entry, _ := find(cmd)
	r := &cmdRun{c: newClient(), cmd: cmd, hasVerbs: len(entry.subs) > 0, args: args, rest: args}
	r.org = r.c.org
	if r.hasVerbs {
		r.verb, r.rest = split(args)
	}
	r.fs = flag.NewFlagSet(cmd, flag.ContinueOnError)
	r.fs.StringVar(&r.orgFlag, "org", "", orgUsage)
	r.fs.BoolVar(&r.yes, "yes", false, yesUsage)
	r.fs.BoolVar(&r.asJSON, "json", false, jsonUsage)
	return r
}

// reword gives a flag another description on one verb, so that, for example,
// 'add' does not offer to clear what it creates.
func (r *cmdRun) reword(verb, name, usage string) {
	if verbAsked(r.cmd, r.args) == verb {
		r.fs.Lookup(name).Usage = usage
	}
}

// parse reports done when help was printed or err is set.
func (r *cmdRun) parse() (done bool, err error) {
	if want, ok := wantsHelp(r.args); ok {
		return true, printHelp(r.fs, r.cmd, want)
	}
	if r.hasVerbs {
		r.verb, err = parseVerb(r.fs, r.cmd, r.verb, r.rest)
	} else {
		err = parseCmd(r.fs, r.cmd, r.args)
	}
	if r.orgFlag != "" {
		r.org = r.orgFlag
	}
	return err != nil, err
}

// verbAsked returns the verb's own name, also in a request for help, or "".
func verbAsked(cmd string, args []string) string {
	name, _ := split(args)
	if want, ok := wantsHelp(args); ok {
		name = want
	}
	entry, _ := find(cmd)
	s, _ := entry.sub(name)
	return s.name
}

// parseVerb checks the verb, its flags and its argument count against
// help.go before anything is sent. It returns the verb's own name, so a
// dispatcher needs one case per verb, not per spelling. No verb is the
// listing.
func parseVerb(fs *flag.FlagSet, cmd, typed string, args []string) (string, error) {
	c, ok := find(cmd)
	if !ok {
		return "", fmt.Errorf("unknown command: keera %s", cmd)
	}
	if typed == "" {
		if _, ok := c.sub("list"); !ok {
			return "", unknownSub(cmd, "")
		}
		typed = "list"
	}
	s, ok := c.sub(typed)
	if !ok {
		return "", unknownSub(cmd, typed)
	}
	return s.name, parseChecked(fs, args, c.name+" "+s.name, s.flags, s.args)
}

// parseCmd is parseVerb for a command with no verbs.
func parseCmd(fs *flag.FlagSet, cmd string, args []string) error {
	c, ok := find(cmd)
	if !ok {
		return fmt.Errorf("unknown command: keera %s", cmd)
	}
	return parseChecked(fs, args, c.name, c.flags, c.args)
}

// parseChecked holds args to help.go's flags and argument spec. A shared
// FlagSet would otherwise accept another verb's flag and quietly drop it.
func parseChecked(fs *flag.FlagSet, args []string, name string, flags []string, spec string) error {
	if err := parse(fs, args); err != nil {
		return fmt.Errorf("%w (see: keera help %s)", err, name)
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err == nil && !slices.Contains(flags, f.Name) {
			err = fmt.Errorf("%s does not take --%s (see: keera help %s)", name, f.Name, name)
		}
	})
	if err != nil {
		return err
	}
	if lo, hi := argCount(spec); fs.NArg() < lo || (hi >= 0 && fs.NArg() > hi) {
		return fmt.Errorf("usage: keera %s (see: keera help %s)",
			strings.TrimSpace(name+" "+strings.ReplaceAll(spec, " [flags]", "")), name)
	}
	return nil
}

// argCount reads a spec such as "<scope> [<id>]". hi is -1 when there is no
// limit, as after "[-- <command>]".
func argCount(spec string) (lo, hi int) {
	for part := range strings.FieldsSeq(spec) {
		switch {
		case part == "[flags]":
		case strings.HasPrefix(part, "[--"):
			return lo, -1
		case strings.HasPrefix(part, "["):
			hi++
		case strings.HasPrefix(part, "<"):
			lo++
			hi++
		}
	}
	return lo, hi
}

// changesSomething ignores flags that only pick the target or the output.
func changesSomething(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "json", "org", "yes", "step":
		default:
			set = true
		}
	})
	return set
}

// nothingToChange refuses an empty 'set', which would only add an audit entry.
func nothingToChange(name string) error {
	return fmt.Errorf("nothing to change; pass a flag to change (see: keera help %s)", name)
}

// given lets an empty value clear what is stored rather than read as left out.
func given(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(fl *flag.Flag) { found = found || fl.Name == name })
	return found
}

// parse accepts flags after positional arguments, which Go's flag package
// does not, by reordering them first. A bad flag comes back as an error
// rather than the flag package printing the whole help.
func parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(reorder(fs, args))
	if err == nil {
		return nil
	}
	msg := strings.Replace(err.Error(), "flag provided but not defined: -", "unknown flag --", 1)
	msg = strings.Replace(msg, "flag needs an argument: -", "a value is missing after --", 1)
	return errors.New(strings.Replace(msg, " for flag -", " for --", 1))
}

func reorder(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after "--" is positional.
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.ContainsRune(name, '=') {
			continue // --flag=value carries its own value
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// durationFlag also takes days, such as 7d, which Go's duration does not.
type durationFlag time.Duration

func durationVar(fs *flag.FlagSet, p *time.Duration, name string, value time.Duration, usage string) {
	*p = value
	fs.Var((*durationFlag)(p), name, usage)
}

// String prints 7d rather than 168h0m0s. A zero or negative value means not
// given, and prints as 0.
func (d *durationFlag) String() string {
	if d == nil || *d <= 0 {
		return "0"
	}
	if v := time.Duration(*d); v%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", v/(24*time.Hour))
	}
	return time.Duration(*d).String()
}

func (d *durationFlag) Set(s string) error {
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = durationFlag(v)
	return nil
}

// parseDuration accepts 7d, 1d12h and 90m.
func parseDuration(s string) (time.Duration, error) {
	bad := errors.New("not a duration such as 7d, 8h or 30m")
	days, rest, found := strings.Cut(s, "d")
	if !found {
		v, err := time.ParseDuration(s)
		if err != nil {
			return 0, bad
		}
		return v, nil
	}
	n, err := strconv.Atoi(days)
	if err != nil || n < 0 {
		return 0, bad
	}
	v := time.Duration(n) * 24 * time.Hour
	if rest != "" {
		more, err := time.ParseDuration(rest)
		if err != nil || strings.HasPrefix(rest, "-") {
			return 0, bad
		}
		v += more
	}
	return v, nil
}

func opposites(a, b string) error {
	return fmt.Errorf("--%s and --%s are opposites; pass one of them", a, b)
}
