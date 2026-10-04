package cli

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

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
		return err
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err == nil && !slices.Contains(flags, f.Name) {
			err = fmt.Errorf("keera %s does not take --%s (see: keera help %s)", name, f.Name, name)
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

// parse reads a subcommand's flags, before or after the positional
// arguments. Go's flag package stops at the first non-flag, so
// "keera guardrail set team t_1 --rpm 60" is reordered before parsing.
func parse(fs *flag.FlagSet, args []string) error {
	return fs.Parse(reorder(fs, args))
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
