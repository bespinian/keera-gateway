package cli

import (
	"flag"
	"fmt"
	"slices"
	"strings"
)

// verbFlags refuses a flag the verb does not read, going by its entry in
// help.go. A command whose verbs share one FlagSet would otherwise accept
// another verb's flag and quietly drop it.
func verbFlags(fs *flag.FlagSet, cmd, sub string) error {
	c, ok := find(cmd)
	if !ok {
		return nil
	}
	s, ok := c.sub(sub)
	if !ok {
		return nil
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err == nil && !slices.Contains(s.flags, f.Name) {
			err = fmt.Errorf("keera %s %s does not take --%s (see: keera %s %s --help)",
				cmd, s.name, f.Name, cmd, s.name)
		}
	})
	return err
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
