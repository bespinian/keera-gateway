package cli

// Colour only decorates words that already say the same thing, so output to a
// pipe, a file or a CI log gets no escapes. stdout and stderr are decided
// apart: `keera connect opencode > file` sends the configuration to a file and
// the steps to the terminal.

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// Only the basic colours, which every terminal theme supports.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// painter's zero value writes no escapes.
type painter bool

// style and styleErr paint stdout and stderr. Run sets them, so a test that
// calls a printer directly gets plain text.
var style, styleErr painter

var colorMode = "auto"

func (p painter) paint(code, s string) string {
	if !p || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (p painter) head(s string) string { return p.paint(ansiBold, s) }

// cmd is for something to type: a command, a flag, an address.
func (p painter) cmd(s string) string { return p.paint(ansiCyan, s) }

// ok, warn and bad are for verdicts and states.
func (p painter) ok(s string) string   { return p.paint(ansiGreen, s) }
func (p painter) warn(s string) string { return p.paint(ansiYellow, s) }
func (p painter) bad(s string) string  { return p.paint(ansiRed, s) }

func (p painter) muted(s string) string { return p.paint(ansiDim, s) }

func resolveStyle() {
	style = painter(wantsColor(colorMode, os.Stdout))
	styleErr = painter(wantsColor(colorMode, os.Stderr))
}

func wantsColor(mode string, f *os.File) bool {
	switch mode {
	case "never":
		return false
	case "always":
		return true
	}
	// https://no-color.org
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

// isTerminal treats any character device as a terminal, which is enough for
// deciding on colour.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func takeColorMode(args []string) ([]string, error) {
	args = slices.Clone(args)
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--no-color" || arg == "-no-color" {
			args[i] = "--color=never"
		}
	}
	return takeGlobal(args, "color", func(v string) error {
		switch v {
		case "auto", "always", "never":
			colorMode = v
			return nil
		}
		return badColor(v)
	})
}

func badColor(given string) error {
	return fmt.Errorf("--color takes auto, always or never, not %q", given)
}

// statusWord leaves unknown words plain.
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

// padTo measures only what s shows, not its escapes.
func padTo(s string, width int) string {
	if n := width - len([]rune(stripANSI(s))); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// stripANSI removes the escapes this package writes.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// A CSI sequence: digits and semicolons, then a letter. Anything
			// else is not ours and is kept.
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
