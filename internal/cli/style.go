package cli

// Colour, and when this command line uses it.
//
// Colour only decorates words that already say the same thing, so output to
// anything but a terminal (a pipe, a file, a CI log) gets no escapes at all.
//
// stdout and stderr are decided apart: `keera connect opencode > file` sends
// the configuration to a file and the steps around it to the terminal.

import (
	"fmt"
	"os"
	"strings"
)

// The escapes. Only the basic colours, which every terminal theme supports.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// painter paints a string, or hands it back untouched. The zero value is the
// safe one: no escapes at all.
type painter bool

// style and styleErr paint stdout and stderr. Run sets them, so a test that
// calls a printer directly gets plain text.
var style, styleErr painter

// colorMode is the global --color flag: auto, always or never.
var colorMode = "auto"

func (p painter) paint(code, s string) string {
	if !p || s == "" {
		return s
	}
	return code + s + ansiReset
}

// head is a column heading or a section title.
func (p painter) head(s string) string { return p.paint(ansiBold, s) }

// cmd is something to type: a command, a flag, an address.
func (p painter) cmd(s string) string { return p.paint(ansiCyan, s) }

// ok, warn and bad are the three verdicts anything in here can have - a
// check's, a sandbox's state, a key's.
func (p painter) ok(s string) string   { return p.paint(ansiGreen, s) }
func (p painter) warn(s string) string { return p.paint(ansiYellow, s) }
func (p painter) bad(s string) string  { return p.paint(ansiRed, s) }

// muted is for asides and placeholders.
func (p painter) muted(s string) string { return p.paint(ansiDim, s) }

// resolveStyle decides, once, what each stream gets.
func resolveStyle() {
	style = painter(wantsColor(colorMode, os.Stdout))
	styleErr = painter(wantsColor(colorMode, os.Stderr))
}

// wantsColor answers for one stream, honouring the usual environment
// variables.
func wantsColor(mode string, f *os.File) bool {
	switch mode {
	case "never":
		return false
	case "always":
		return true
	}
	// https://no-color.org: set to anything, and colour is off.
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	// For a CI console that is not detected as a terminal.
	if force := os.Getenv("CLICOLOR_FORCE"); force != "" && force != "0" {
		return true
	}
	return isTerminal(f)
}

// isTerminal treats a character device as a terminal, and a pipe, a file or
// a socket as not. That is enough for deciding on colour.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// takeColorMode reads --color off the arguments, as takeURL does --url.
func takeColorMode(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// Everything after it belongs to whatever this command runs.
			return append(out, args[i:]...), nil
		case arg == "--no-color" || arg == "-no-color":
			colorMode = "never"
		case arg == "--color" || arg == "-color":
			if i+1 >= len(args) {
				return nil, badColor("")
			}
			i++
			colorMode = args[i]
		case strings.HasPrefix(arg, "--color=") || strings.HasPrefix(arg, "-color="):
			colorMode = arg[strings.Index(arg, "=")+1:]
		default:
			out = append(out, arg)
			continue
		}
		switch colorMode {
		case "auto", "always", "never":
		default:
			return nil, badColor(colorMode)
		}
	}
	return out, nil
}

// badColor is what --color says to a word it does not have.
func badColor(given string) error {
	return fmt.Errorf("--color takes auto, always or never, not %q", given)
}

// statusWord paints a state word by what it means. Unknown words are left
// plain.
func statusWord(word string) string {
	switch word {
	case "ready", "active", "running", "enabled", "true", "yes", "ok":
		return style.ok(word)
	case "pending", "starting", "suspended", "warm":
		return style.warn(word)
	case "failed", "expired", "terminated", "revoked":
		return style.bad(word)
	case "disabled", "false", "no", "never", "unknown":
		return style.muted(word)
	}
	return word
}

// padTo pads s to width, measuring only what it shows, not its escapes.
func padTo(s string, width int) string {
	if n := width - len([]rune(stripANSI(s))); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// stripANSI removes the escapes this package writes, so widths can be
// measured on what is shown.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// A CSI sequence: digits and semicolons, then a final letter.
			// Anything else is not ours and is kept.
			j := i + 2
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] >= '@' && s[j] <= '~' {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
