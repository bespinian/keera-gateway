package cli

// The tables this command line prints, and how a painted cell stays in its
// column.
//
// text/tabwriter counts escape sequences as width, so a painted cell would
// push its column out. So tabwriter lays out the text without escapes, and
// the escapes are put back afterwards.
//
// A table is an io.Writer: printers write tab-separated cells and lines of
// prose to it, and Flush lays it all out.

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// tablePad is the space between two columns.
const tablePad = 2

type table struct {
	buf bytes.Buffer
	w   io.Writer
}

func newTable(w io.Writer) *table { return &table{w: w} }

func (t *table) Write(p []byte) (int, error) { return t.buf.Write(p) }

// header writes a table's heading row, painted. The names are uppercase, so
// the heading stands out without colour too.
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

// repaint puts the painted cells of a written line into the columns of the
// laid-out line.
//
// Layout only adds padding after a cell, so the two lines run in step. A line
// that does not is printed as laid out: plain, but in its column.
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
