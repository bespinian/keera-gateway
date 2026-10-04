package gateway

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"unicode/utf8"
)

// jsonText is text kept as a JSON string literal, quotes included, as the
// client wrote it.
//
// A JSON string means the same in every API, so text can cross from one to
// another without being unescaped and escaped again. In a coding agent's
// request that text - file contents, tool output - is nearly all of it.
//
// nil is no text at all; `""` is empty text.
type jsonText []byte

// emptyText is empty text, for a field that must be present.
var emptyText = jsonText(`""`)

// textOf turns one JSON value into text. null is empty text, as decoding null
// into a string would give. Anything else that is not a string is an error.
//
// A literal that is not valid UTF-8, or that escapes a UTF-16 surrogate, is
// decoded and written again, which replaces what it cannot carry with U+FFFD.
// Forwarded as it is, either could be refused by the backend.
func textOf(raw []byte) (jsonText, error) {
	raw = bytes.TrimSpace(raw)
	switch {
	case bytes.Equal(raw, nullLiteral):
		return emptyText, nil
	case len(raw) < 2 || raw[0] != '"':
		return nil, errNotText
	case utf8.Valid(raw) && !escapesSurrogate(raw):
		return jsonText(raw), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return quoteText(s), nil
}

var errNotText = errors.New("not a string")

// escapesSurrogate reports whether a literal has a \uD800 to \uDFFF escape. It
// also matches a few escapes just below that range, which only costs them the
// slower path.
func escapesSurrogate(raw []byte) bool {
	return bytes.Contains(raw, []byte(`\ud`)) || bytes.Contains(raw, []byte(`\uD`))
}

// quoteText makes text from a Go string.
func quoteText(s string) jsonText {
	// The error only reports invalid UTF-8, which is replaced either way.
	t, _ := jsontext.AppendQuote(make([]byte, 0, len(s)+2), s)
	return t
}

// UnmarshalJSON lets a struct field hold text without decoding it. The bytes
// are copied, because the decoder reuses its buffer.
func (t *jsonText) UnmarshalJSON(raw []byte) error {
	text, err := textOf(raw)
	if err != nil {
		return err
	}
	*t = bytes.Clone(text)
	return nil
}

// isEmpty reports whether there is no text, or only empty text.
func (t jsonText) isEmpty() bool { return len(t) <= 2 }

// joinTexts joins texts with a blank line between them, leaving out empty
// ones. The inside of a literal is a valid piece of any literal, so the
// pieces are put together without decoding them.
func joinTexts(texts []jsonText) jsonText {
	var n, kept int
	var last jsonText
	for _, t := range texts {
		if !t.isEmpty() {
			n += len(t)
			kept++
			last = t
		}
	}
	switch kept {
	case 0:
		return nil
	case 1:
		return last
	}
	out := make(jsonText, 0, n+4*kept)
	out = append(out, '"')
	for _, t := range texts {
		if t.isEmpty() {
			continue
		}
		if len(out) > 1 {
			out = append(out, `\n\n`...)
		}
		out = append(out, t[1:len(t)-1]...)
	}
	return append(out, '"')
}

// appendQuoted appends s as a JSON string.
func appendQuoted(dst []byte, s string) []byte {
	dst, _ = jsontext.AppendQuote(dst, s)
	return dst
}
