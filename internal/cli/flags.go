package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// cmdRun is what every command starts with: the client, the flags, and the
// verb. A command declares its own flags on fs, then calls parse.
type cmdRun struct {
	c        *client
	fs       *flag.FlagSet
	cmd      string
	hasVerbs bool
	// verb is the verb as typed until parse, and its own name after.
	verb string
	args []string
	// rest is args without the verb.
	rest []string

	// The flags many commands share, declared when newCmdRun is asked to.
	org    string
	yes    bool
	asJSON bool
}

// newCmdRun takes the verb off args, when cmd has verbs, and declares the
// shared flags named: "org", "yes" and "json". "orgs" is --org on a report,
// which spans every organisation the caller can see unless told one.
func newCmdRun(cmd string, args []string, shared ...string) *cmdRun {
	entry, _ := find(cmd)
	r := &cmdRun{c: newClient(), cmd: cmd, hasVerbs: len(entry.subs) > 0, args: args, rest: args}
	if r.hasVerbs {
		r.verb, r.rest = split(args)
	}
	r.fs = flag.NewFlagSet(cmd, flag.ContinueOnError)
	for _, name := range shared {
		switch name {
		case "org":
			r.fs.StringVar(&r.org, "org", "", orgUsage)
		case "orgs":
			r.fs.StringVar(&r.org, "org", "", orgsUsage)
		case "yes":
			r.fs.BoolVar(&r.yes, "yes", false, yesUsage)
		case "json":
			r.fs.BoolVar(&r.asJSON, "json", false, jsonUsage)
		}
	}
	return r
}

// parse reads the verb and the flags. done means there is nothing left to
// run: help was asked for and printed, or err says what was wrong.
func (r *cmdRun) parse() (done bool, err error) {
	if want, ok := wantsHelp(r.args); ok {
		return true, printHelp(r.fs, r.cmd, want)
	}
	if r.hasVerbs {
		r.verb, err = parseVerb(r.fs, r.cmd, r.verb, r.rest)
	} else {
		err = parseCmd(r.fs, r.cmd, r.args)
	}
	return err != nil, err
}

// verbAsked is the verb args name, run or asked about in a request for help,
// by its own name. It is empty when args name none that cmd has.
func verbAsked(cmd string, args []string) string {
	name, _ := split(args)
	if want, ok := wantsHelp(args); ok {
		name = want
	}
	entry, _ := find(cmd)
	s, _ := entry.sub(name)
	return s.name
}

// parseVerb reads one verb's flags and arguments, going by its entry in
// help.go. It refuses a verb the command does not have, a flag the verb does
// not read and the wrong number of arguments, all before anything is sent.
//
// It returns the verb's own name, so a dispatcher needs one case per verb
// rather than one per spelling. No verb at all is the listing, where the
// command has one.
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

// parseCmd is parseVerb for a command with no verbs, which takes its flags
// and arguments directly.
func parseCmd(fs *flag.FlagSet, cmd string, args []string) error {
	c, ok := find(cmd)
	if !ok {
		return fmt.Errorf("unknown command: keera %s", cmd)
	}
	return parseChecked(fs, args, c.name, c.flags, c.args)
}

// parseChecked parses args and holds them to the flags and the argument
// spec help.go gives for "keera <name>". A shared FlagSet would otherwise
// accept another verb's flag and quietly drop it.
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

// argCount reads how many arguments an argument spec such as
// "<scope> [<id>]" allows. hi is -1 when there is no limit, as after
// "[-- <command>]". "[flags]" is not an argument.
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

// changesSomething reports whether any flag was given other than those that
// only pick the target or the output.
func changesSomething(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name != "json" && fl.Name != "org" && fl.Name != "yes" {
			set = true
		}
	})
	return set
}

// nothingToChange refuses a 'set' given no flag that changes anything. Sent
// anyway, it would write the entry back as it was, with an audit entry for it.
func nothingToChange(name string) error {
	return fmt.Errorf("nothing to change; pass a flag to change (see: keera help %s)", name)
}

// given reports whether the flag was on the command line, so an empty value
// can clear what is stored rather than be read as left out.
func given(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(fl *flag.Flag) { found = found || fl.Name == name })
	return found
}

// parse reads a subcommand's flags, before or after the positional
// arguments. Go's flag package stops at the first non-flag, so
// "keera guardrail set project project_1 --rpm 60" is reordered before parsing.
//
// A bad flag comes back as an error for Run to print, worded the way the rest
// of the CLI names flags, instead of the flag package printing the whole help.
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

		// Everything after "--" is positional by definition.
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
		// A non-boolean flag takes the next argument with it.
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

// opposites refuses two flags that undo each other.
func opposites(a, b string) error {
	return fmt.Errorf("--%s and --%s are opposites; pass one of them", a, b)
}
