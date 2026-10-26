package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// body is a shallow view of a JSON request body. Only the top level is split,
// into spans of the original bytes, since messages can be hundreds of kilobytes.
type body struct {
	fields map[string]json.RawMessage
	// order keeps the client's field order for the forwarded request.
	order []string
}

var (
	errNotObject = errors.New("request body must be a JSON object")
	// errMalformed should be unreachable after json.Valid. It turns a bug in
	// the walk into a refusal, not a panic.
	errMalformed = errors.New("request body is malformed")
)

func parseBody(raw []byte) (*body, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("request body is empty")
	}
	// Validated up front, so the walk below can trust the document.
	if !json.Valid(raw) {
		return nil, syntaxError(raw)
	}
	fields, order, err := splitObject(raw)
	if err != nil {
		return nil, err
	}
	if err := checkKeys(fields); err != nil {
		return nil, err
	}
	return &body{fields: fields, order: order}, nil
}

// readNames are the keys the gateway reads anywhere in a request.
var readNames = foldedNames(
	"_meta", "action", "arguments", "call_id", "content", "data", "definition", "description",
	"disable_parallel_tool_use", "file_data", "filename", "format", "id", "image_url", "input", "input_schema",
	"instructions", "max_completion_tokens", "max_output_tokens", "max_tokens",
	"media_type", "messages", "model", "name", "namespace", "output", "parallel_tool_calls",
	"parameters", "prompt", "refusal", "role", "schema", "source", "stop_sequences",
	"stream", "stream_options", "strict", "syntax", "system", "temperature", "text", "title",
	"tool_choice", "tool_use_id", "tools", "top_k", "top_p", "type", "url",
)

func foldedNames(names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[foldKey(n)] = n
	}
	return out
}

// foldKey folds case like encoding/json does when matching keys, so "CONTENT"
// and "ſystem" fold like "content" and "system".
func foldKey(k string) string {
	return strings.Map(func(r rune) rune { return unicode.ToUpper(unicode.ToLower(r)) }, k)
}

// checkKeys refuses a key the gateway reads when spelled with other case.
// Filters match keys exactly, but encoding/json and maybe the inference server
// do not, so a model could read text under "Content" that no filter saw.
func checkKeys[V any](obj map[string]V) error {
	for k := range obj {
		if n, ok := readNames[foldKey(k)]; ok && n != k {
			return fmt.Errorf("the key '%s' must be spelled '%s'", k, n)
		}
	}
	return nil
}

// syntaxError re-reads an invalid body to borrow encoding/json's message,
// which names the offset of the error.
func syntaxError(raw []byte) error {
	var probe json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	return errors.New("request body is not valid JSON")
}

// splitObject returns each top-level value as a sub-slice of raw. raw must
// have passed json.Valid, since this only counts brackets and skips strings.
func splitObject(raw []byte) (map[string]json.RawMessage, []string, error) {
	i := skipSpace(raw, 0)
	if i == len(raw) || raw[i] != '{' {
		return nil, nil, errNotObject
	}
	const typicalFields = 16
	fields := make(map[string]json.RawMessage, typicalFields)
	order := make([]string, 0, typicalFields)

	i++
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
		// A repeated key keeps its last value, like decoding into a map.
		if _, seen := fields[key]; !seen {
			order = append(order, key)
		}
		fields[key] = raw[i:valEnd]
		i = valEnd
	}
}

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

// endOfString returns the index just past the string opening at i. It jumps
// from quote to quote, since prompts are mostly string bytes.
func endOfString(raw []byte, i int) int {
	start := i + 1
	for i = start; i < len(raw); i++ {
		q := bytes.IndexByte(raw[i:], '"')
		if q < 0 {
			return len(raw)
		}
		i += q
		slashes := 0
		for j := i - 1; j >= start && raw[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			return i + 1
		}
	}
	return len(raw)
}

func endOfValue(raw []byte, i int) int {
	switch {
	case i == len(raw):
		return i
	case raw[i] == '"':
		return endOfString(raw, i)
	case raw[i] == '{', raw[i] == '[':
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
		for ; i < len(raw); i++ {
			switch raw[i] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return i
			}
		}
		return i
	}
}

// nullLiteral is treated as absent, since decoding null leaves a zero value
// that would read as "the client sent 0".
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

// ceiling reads an output-token ceiling however it is spelled: 4096, 4096.0,
// 4e3 or "4096". It is a float64 so all of these decode, and so a value too
// large for an int still compares against the limit.
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

// prependSystem puts a system message first in the messages array, splicing
// bytes so the messages are never decoded. It reports false if there is no
// array.
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

// firstUserMessage returns the raw content of the first user message. The
// opening prompt is the same on every call of a task, so it identifies the
// task. Malformed elements are skipped, since this only labels a usage row.
func (b *body) firstUserMessage() (json.RawMessage, bool) {
	raw, ok := b.value("messages")
	if !ok {
		return nil, false
	}
	var opening json.RawMessage
	eachElement(raw, func(elem []byte) bool {
		fields, _, err := splitObject(elem)
		if err != nil {
			return true
		}
		var role string
		if json.Unmarshal(fields["role"], &role) != nil || role != "user" {
			return true
		}
		// Without content, the whole element still hashes the same each time.
		opening = elem
		if content, ok := fields["content"]; ok {
			opening = content
		}
		return false
	})
	return opening, opening != nil
}

// eachElement calls fn with each value of a JSON array until fn returns false.
// Values are not decoded.
func eachElement(raw json.RawMessage, fn func(elem []byte) bool) {
	arr := bytes.TrimSpace(raw)
	if len(arr) < 2 || arr[0] != '[' {
		return
	}
	for i := 1; ; {
		i = skipSpace(arr, i)
		if i >= len(arr) || arr[i] == ']' {
			return
		}
		if arr[i] == ',' {
			i++
			continue
		}
		end := endOfValue(arr, i)
		if end <= i {
			return
		}
		if !fn(arr[i:end]) {
			return
		}
		i = end
	}
}

// ensureUsageInStream asks the upstream for token counts on a stream, which
// vLLM and OpenAI only send when asked. It reports whether the gateway added
// the flag; if so, the usage-only chunk is removed on the way back, since a
// client may not cope with a chunk without choices.
func (b *body) ensureUsageInStream() (injected bool) {
	var opts map[string]json.RawMessage
	if raw, ok := b.value("stream_options"); !ok || json.Unmarshal(raw, &opts) != nil || opts == nil {
		opts = map[string]json.RawMessage{}
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
