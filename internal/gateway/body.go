package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
)

// body is a shallow view of a JSON request body.
//
// The gateway touches a few top-level fields (model, stream, max_tokens),
// while most of a coding-agent request is a messages array that can run to
// hundreds of kilobytes. So only the top level is split, into spans of the
// original bytes, and rewriting a request costs the number of fields rather
// than the size of the conversation.
type body struct {
	fields map[string]json.RawMessage
	// order preserves the order fields arrived in, so a request forwarded
	// upstream reads the same as the one the client sent.
	order []string
}

var (
	errNotObject = errors.New("request body must be a JSON object")
	// errMalformed covers cases the walk below cannot reach in a document that
	// passed json.Valid. It turns a bug in the walk into a refusal, not a panic.
	errMalformed = errors.New("request body is malformed")
)

func parseBody(raw []byte) (*body, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("request body is empty")
	}
	// Validated once, up front, so the walk below can trust the document and
	// a bad body is refused with a message instead of forwarded.
	if !json.Valid(raw) {
		return nil, syntaxError(raw)
	}
	fields, order, err := splitObject(raw)
	if err != nil {
		return nil, err
	}
	return &body{fields: fields, order: order}, nil
}

// syntaxError re-reads an invalid body only to borrow encoding/json's message,
// which names the offset where it went wrong. It runs only on refusal.
func syntaxError(raw []byte) error {
	var probe json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	return errors.New("request body is not valid JSON")
}

// splitObject walks the top level of a JSON object, recording where each
// member's value begins and ends.
//
// The values are sub-slices of raw, not copies. Nothing edits them in place: a
// rewritten field is replaced.
//
// raw must already have passed json.Valid. This finds value boundaries by
// counting brackets and skipping strings; it is not a validator.
func splitObject(raw []byte) (map[string]json.RawMessage, []string, error) {
	i := skipSpace(raw, 0)
	if i == len(raw) || raw[i] != '{' {
		return nil, nil, errNotObject
	}
	// Enough for a chat request and the few fields a coding agent adds.
	const typicalFields = 16
	fields := make(map[string]json.RawMessage, typicalFields)
	order := make([]string, 0, typicalFields)

	i++ // the opening brace
	for {
		i = skipSpace(raw, i)
		if i == len(raw) {
			return nil, nil, errMalformed
		}
		switch raw[i] {
		case '}':
			return fields, order, nil
		case ',':
			i++
			continue
		case '"':
		default:
			return nil, nil, errMalformed
		}

		keyEnd := endOfString(raw, i)
		var key string
		if err := json.Unmarshal(raw[i:keyEnd], &key); err != nil {
			return nil, nil, err
		}
		i = skipSpace(raw, keyEnd)
		if i == len(raw) || raw[i] != ':' {
			return nil, nil, errMalformed
		}

		i = skipSpace(raw, i+1)
		valEnd := endOfValue(raw, i)
		// A repeated key keeps its last value, as decoding into a map would,
		// and is forwarded once.
		if _, seen := fields[key]; !seen {
			order = append(order, key)
		}
		fields[key] = raw[i:valEnd]
		i = valEnd
	}
}

// skipSpace returns the index of the first byte at or after i that is not JSON
// whitespace.
func skipSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// endOfString returns the index just past the string opening at i. The byte
// after a backslash is skipped, so an escaped quote does not end the string.
func endOfString(raw []byte, i int) int {
	for i++; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// endOfValue returns the index just past the JSON value beginning at i.
func endOfValue(raw []byte, i int) int {
	switch {
	case i == len(raw):
		return i
	case raw[i] == '"':
		return endOfString(raw, i)
	case raw[i] == '{', raw[i] == '[':
		// Brackets inside strings must not count, so strings are skipped whole.
		depth := 0
		for i < len(raw) {
			switch raw[i] {
			case '"':
				i = endOfString(raw, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	default:
		// A number, true, false or null ends where the next token can begin.
		for ; i < len(raw); i++ {
			switch raw[i] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return i
			}
		}
		return i
	}
}

// nullLiteral is treated as absent. Decoding null into a Go value succeeds and
// leaves zero, which would read as "the client sent 0".
var nullLiteral = []byte("null")

func (b *body) value(key string) (json.RawMessage, bool) {
	raw, ok := b.fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), nullLiteral) {
		return nil, false
	}
	return raw, true
}

func (b *body) str(key string) (string, bool) {
	raw, ok := b.value(key)
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func (b *body) boolean(key string) (bool, bool) {
	raw, ok := b.value(key)
	if !ok {
		return false, false
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return false, false
	}
	return v, true
}

// ceiling reads an output-token ceiling however the client spelled it: 4096,
// 4096.0, 4e3 and "4096" are the same ceiling. encoding/json decodes a Go int
// only from an integer literal, so an int would miss most of these.
//
// It is a float64 so that a value too large for an int can still be compared
// against the limit.
func (b *body) ceiling(key string) (float64, bool) {
	raw, ok := b.value(key)
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
			return 0, false
		}
	}
	// Only a string can carry these, and neither compares usefully.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func (b *body) set(key string, raw json.RawMessage) {
	if _, exists := b.fields[key]; !exists {
		b.order = append(b.order, key)
	}
	b.fields[key] = raw
}

// remove drops a field, and reports whether it was there.
func (b *body) remove(key string) bool {
	if _, exists := b.fields[key]; !exists {
		return false
	}
	delete(b.fields, key)
	b.order = slices.DeleteFunc(b.order, func(k string) bool { return k == key })
	return true
}

func (b *body) setString(key, val string) {
	raw, _ := json.Marshal(val)
	b.set(key, raw)
}

func (b *body) setInt(key string, val int) {
	b.set(key, json.RawMessage(strconv.Itoa(val)))
}

// prependSystem puts a system message at the head of a chat request's
// messages array, and reports false if there is no array to put it in.
//
// It splices bytes instead of decoding, so no structure is built for the
// source code inside the messages.
func (b *body) prependSystem(content string) bool {
	raw, ok := b.value("messages")
	if !ok {
		return false
	}
	arr := bytes.TrimSpace(raw)
	if len(arr) < 2 || arr[0] != '[' || arr[len(arr)-1] != ']' {
		return false
	}
	msg, err := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{"system", content})
	if err != nil {
		return false
	}
	rest := bytes.TrimSpace(arr[1 : len(arr)-1])
	out := make([]byte, 0, len(msg)+len(rest)+3)
	out = append(out, '[')
	out = append(out, msg...)
	if len(rest) > 0 {
		out = append(out, ',')
		out = append(out, rest...)
	}
	out = append(out, ']')
	b.set("messages", out)
	return true
}

// firstUserMessage returns the content of the first "user" message in a chat
// request, and reports whether there was one.
//
// It is the opening prompt: the same on the first call of a task and on its
// fortieth, which makes it usable as the task's identity.
//
// It steps over each array element without decoding it, and returns the
// content as its raw span. Elements that are not objects or have no role are
// skipped, not refused: this only labels a usage row, and must not fail a body
// the inference plane would accept.
func (b *body) firstUserMessage() (json.RawMessage, bool) {
	raw, ok := b.value("messages")
	if !ok {
		return nil, false
	}
	arr := bytes.TrimSpace(raw)
	if len(arr) < 2 || arr[0] != '[' {
		return nil, false
	}
	for i := 1; ; {
		i = skipSpace(arr, i)
		if i >= len(arr) || arr[i] == ']' {
			return nil, false
		}
		if arr[i] == ',' {
			i++
			continue
		}
		end := endOfValue(arr, i)
		if end <= i {
			return nil, false
		}
		elem := arr[i:end]
		i = end

		fields, _, err := splitObject(elem)
		if err != nil {
			continue
		}
		var role string
		if json.Unmarshal(fields["role"], &role) != nil || role != "user" {
			continue
		}
		// A message without content uses the whole element: all that matters
		// is that the same conversation hashes the same way.
		if content, ok := fields["content"]; ok {
			return content, true
		}
		return elem, true
	}
}

// ensureUsageInStream makes the upstream report token counts on a stream.
//
// vLLM and OpenAI only send usage on a stream when asked, and almost no client
// asks. The result says whether the gateway added the flag: if so, the extra
// usage-only chunk is removed on the way back, because a client that never
// asked for it may not cope with a chunk that has no choices.
func (b *body) ensureUsageInStream() (injected bool) {
	opts := map[string]json.RawMessage{}
	if raw, ok := b.fields["stream_options"]; ok {
		if err := json.Unmarshal(raw, &opts); err != nil {
			opts = map[string]json.RawMessage{}
		}
	}
	if inc, ok := opts["include_usage"]; ok {
		var v bool
		if json.Unmarshal(inc, &v) == nil && v {
			return false // the client asked; the chunk is theirs to receive
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	raw, err := json.Marshal(opts)
	if err != nil {
		return false
	}
	b.set("stream_options", raw)
	return true
}

// encode renders the body back to JSON. Values that were not touched are copied
// through verbatim.
func (b *body) encode() []byte {
	var buf bytes.Buffer
	buf.Grow(b.size() + 2)
	buf.WriteByte('{')
	for i, k := range b.order {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(b.fields[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func (b *body) size() int {
	n := 0
	for k, v := range b.fields {
		n += len(k) + len(v) + 4
	}
	return n
}
