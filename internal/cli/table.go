package cli

// text/tabwriter counts escape sequences as width, so a painted cell would
// push its column out. So a table lays out the text without escapes and puts
// them back afterwards.

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

const tablePad = 2

type table struct {
	buf bytes.Buffer
	w   io.Writer
}

func newTable(w io.Writer) *table { return &table{w: w} }

func (t *table) Write(p []byte) (int, error) { return t.buf.Write(p) }

// header is painted; the names are uppercase so it stands out without colour
// too.
func (t *table) header(cells string) {
	_, _ = fmt.Fprintln(t, style.head(cells))
}

func (t *table) Flush() error {
	written := t.buf.String()
	plain := stripANSI(written)

	var laid bytes.Buffer
	tw := tabwriter.NewWriter(&laid, 0, 0, tablePad, ' ', 0)
	if _, err := io.WriteString(tw, plain); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if plain == written {
		_, err := io.WriteString(t.w, laid.String())
		return err
	}

	// Stripping removes no newlines, so both sides have the same lines.
	src := strings.Split(written, "\n")
	out := strings.Split(laid.String(), "\n")
	if len(src) != len(out) {
		_, err := io.WriteString(t.w, laid.String())
		return err
	}
	var b strings.Builder
	for i, line := range src {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(repaint(line, out[i]))
	}
	_, err := io.WriteString(t.w, b.String())
	return err
}

// repaint puts the painted cells of a written line into the laid-out line.
// Layout only adds padding after a cell, so the two run in step. A line that
// does not is printed plain, as laid out.
func repaint(written, laid string) string {
	cells := strings.Split(written, "\t")
	if len(cells) == 1 {
		// Prose, not a row. A last cell is never padded, so it is unchanged.
		return written
	}
	var b strings.Builder
	pos := 0
	for i, cell := range cells {
		text := stripANSI(cell)
		if !strings.HasPrefix(laid[pos:], text) {
			return laid
		}
		b.WriteString(cell)
		pos += len(text)
		if i == len(cells)-1 {
			break
		}
		// The spaces after this cell are the padding plus any spaces the next
		// cell starts with.
		rest := laid[pos:]
		gap := len(rest) - len(strings.TrimLeft(rest, " "))
		next := stripANSI(cells[i+1])
		gap -= len(next) - len(strings.TrimLeft(next, " "))
		if gap < 0 {
			return laid
		}
		b.WriteString(laid[pos : pos+gap])
		pos += gap
	}
	b.WriteString(laid[pos:])
	return b.String()
}
