package gateway

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// Two client APIs are also a hosted provider's own: the Anthropic Messages API
// is Anthropic's, and the Responses API is OpenAI's. Translating a request into
// chat completions for that very provider loses what the translation has no
// place for - Anthropic's prompt caching and thinking, OpenAI's reasoning items
// - and on a coding agent's long session the lost cache is most of the bill.
//
// So when a request in one of those APIs goes to its own provider, it is
// forwarded in that API. It still passes every guardrail: the filters read and
// rewrite it in its own shape, and the system prompt and the output ceiling
// are written into it in that shape. Every other destination gets it
// translated, as before. The translation is made only when such a destination,
// or a router that reads the request, needs it: on a long session it costs far
// more than the rest of the gateway's work.

// dialect is a client API that one hosted provider serves as its own.
type dialect interface {
	// provider is the catalogue provider whose own API this is.
	provider() string
	// path is where that API is, under the provider's base URL.
	path() string
	// text is what filters read in a body of this API, and how to put it back.
	text(b *body) (textDoc, error)
	// addSystem puts the guardrail's system prompt ahead of the client's own.
	addSystem(b *body, prompt string) error
	// clamp holds the answer to the guardrail's ceiling.
	clamp(b *body, limit int)
	// needsNative says why a body can only go to the provider itself, or is
	// empty when it can be translated for any model.
	needsNative(b *body) string
	// auth presents the credential the way the provider's own API wants it.
	// It is given the client's headers, for the few it passes on.
	auth(client http.Header) func(h http.Header, credential string)
	// usage reads the token counts off a buffered answer.
	usage(raw []byte) *tokenUsage
	// stream reads a streamed answer as it passes through.
	stream(alias string) nativeStream
	// onKey makes a body fit to send on the deployment's key, or says why it
	// cannot be. See docs/billing.md.
	onKey(b *body) string
	// stripHostedTools takes out the tools the provider runs on its own
	// servers, and returns what it took out. See hosted.go.
	stripHostedTools(b *body) []string
	// opening is the first user message as the translation has it, for the
	// session key. Only the turns up to it are translated. The translation
	// drops what changes from turn to turn, such as cache markers, so a task
	// keeps one key.
	opening(b *body) (json.RawMessage, bool)
}

// nativeStream watches a provider's own event stream on its way to the client.
type nativeStream interface {
	// event reads one event's data, and returns it rewritten, or nil to send
	// it as it came.
	event(payload []byte) []byte
	// counts is the usage record, or nil if none arrived, and the number of
	// events that carried generated content. partial says the record was
	// completed from those events because the stream ended early.
	counts() (usage *tokenUsage, deltas int, partial bool)
	// failure is why a stream read to its end is still not a whole answer:
	// an error event, or no event that ends the answer. Nil otherwise.
	failure() error
}

// eventType is the type an event's data names. It is read from the JSON, not
// searched for in the bytes: the data can quote what the client sent, such as
// its metadata or a stop sequence, and a client could otherwise make the event
// that carries the usage record look like another.
func eventType(payload []byte) string {
	var ev struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &ev)
	return ev.Type
}

// speaksNative reports whether a destination is sent the client's own API.
func speaksNative(d dialect, m policy.Model) bool {
	return d != nil && m.Provider == d.provider()
}

// pipeNative forwards a provider's own event stream, flushing every event, and
// lets ns read and rewrite each one.
//
// Events are held whole, unlike in pipeSSE: the one that carries the usage
// record also carries the whole answer, and cutting it would lose the count.
// An event larger than limit ends the stream instead, so an upstream that never
// finishes one cannot fill the gateway's memory.
func pipeNative(dst io.Writer, flush func(), src io.Reader, ns nativeStream,
	limit int64,
) (streamStats, error) {
	var (
		stats streamStats
		event []byte // the raw event, every line of it
		head  []byte // its lines other than data, to rebuild it around new data
		data  []byte
	)
	emit := func() error {
		if len(event) == 0 {
			return nil
		}
		out := event
		if len(data) > 0 {
			if repl := ns.event(data); repl != nil {
				out = append(append(append(head, dataPrefix...), ' '), repl...)
				out = append(out, '\n', '\n')
			}
		}
		// The deltas are what the model wrote, reasoning included. Events
		// before them, such as message_start, only open the answer.
		if stats.firstAt.IsZero() {
			if _, n, _ := ns.counts(); n > 0 {
				stats.firstAt = time.Now()
			}
		}
		_, err := dst.Write(out)
		flush()
		event, head, data = event[:0], nil, data[:0]
		return err
	}
	err := eachLine(src, limit, func(line []byte) error {
		if int64(len(event)+len(line)) > limit {
			return errEventTooLarge
		}
		event = append(event, line...)
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			return emit()
		case bytes.HasPrefix(trimmed, dataPrefix):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(trimmed[len(dataPrefix):])...)
		default:
			head = append(head, trimmed...)
			head = append(head, '\n')
		}
		return nil
	}, emit)
	stats.usage, stats.deltas, stats.partial = ns.counts()
	stats.cut = ns.failure()
	return stats, err
}

// errEventTooLarge ends a stream whose one event outgrew what the gateway holds.
var errEventTooLarge = errors.New("the upstream sent one event larger than the gateway holds")

// renameIn writes the alias as the model name of one object in a document:
// the document itself when key is empty, or the object under key.
//
// The model name is the backend's, and the client was promised the alias.
// A document that cannot be read is returned as it came.
func renameIn(raw []byte, key, alias string) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	name, _ := json.Marshal(alias)
	if key == "" {
		if _, ok := doc["model"]; !ok {
			return raw
		}
		doc["model"] = name
	} else {
		var inner map[string]json.RawMessage
		if json.Unmarshal(doc[key], &inner) != nil {
			return raw
		}
		if _, ok := inner["model"]; !ok {
			return raw
		}
		inner["model"] = name
		encoded, err := json.Marshal(inner)
		if err != nil {
			return raw
		}
		doc[key] = encoded
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// modelKey is the start of the model field in an OpenAI answer or chunk.
var modelKey = []byte(`"model":`)

// renameModel writes the alias as the value of the first model field in an
// OpenAI answer or chunk, and leaves every other byte as it came.
//
// It works on the bytes, because a stream has one chunk per token and each
// would otherwise be decoded and encoded again. That is safe: inside a JSON
// string a quote is escaped, so the first unescaped `"model":` is a key, and
// in these documents the only one is the answer's own.
func renameModel(raw []byte, alias string) []byte {
	start, end, ok := modelValue(raw)
	if !ok {
		return raw
	}
	name, _ := json.Marshal(alias)
	out := make([]byte, 0, len(raw)-(end-start)+len(name))
	return appendRenamed(out, raw, name, start, end)
}

// appendRenamed appends raw to dst with name, an encoded JSON string, as the
// model. A stream calls it once per chunk with the same name and buffer, so
// renaming a token costs no encoding and no allocation.
func appendRenamed(dst, raw, name []byte, start, end int) []byte {
	dst = append(dst, raw[:start]...)
	dst = append(dst, name...)
	return append(dst, raw[end:]...)
}

// modelValue finds the model's string value in raw, quotes included, as
// raw[start:end].
func modelValue(raw []byte) (start, end int, ok bool) {
	i := bytes.Index(raw, modelKey)
	if i < 0 {
		return 0, 0, false
	}
	start = i + len(modelKey)
	for start < len(raw) && (raw[start] == ' ' || raw[start] == '\t') {
		start++
	}
	if start >= len(raw) || raw[start] != '"' {
		return 0, 0, false
	}
	end = start + 1
	for end < len(raw) && raw[end] != '"' {
		if raw[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(raw) {
		return 0, 0, false
	}
	return start, end + 1, true
}

// ------------------------------------------------------------------ text

// treeDoc is the text of a request whose strings sit at more than one depth,
// as they do in the Messages and Responses APIs. It decodes only the fields
// that hold text, and writes back only those.
type treeDoc struct {
	roots map[string]any
	// raws is each root as it came, for apply to edit.
	raws   map[string][]byte
	values []string
	set    []func(string)
	// err is the first misspelled key the walk met. See checkKeys.
	err error
}

func newTreeDoc() *treeDoc {
	return &treeDoc{roots: map[string]any{}, raws: map[string][]byte{}}
}

// root decodes one field, keeping numbers as they were written. A missing or
// null field is nil.
func (d *treeDoc) root(b *body, field string) (any, error) {
	raw, ok := b.value(field)
	if !ok {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("the '" + field + "' field could not be read")
	}
	d.roots[field] = v
	d.raws[field] = raw
	return v, nil
}

// add collects one string and the way to replace it.
func (d *treeDoc) add(s string, set func(string)) {
	d.values = append(d.values, s)
	d.set = append(d.set, set)
}

// addField collects obj[key] when it is a string.
func (d *treeDoc) addField(obj map[string]any, key string) {
	if s, ok := obj[key].(string); ok {
		d.add(s, func(v string) { obj[key] = v })
	}
}

func (d *treeDoc) texts() []string { return d.values }

func (d *treeDoc) apply(b *body, replaced []string) error {
	if len(replaced) != len(d.set) {
		return errors.New("the filtered request had a different number of segments")
	}
	for i, set := range d.set {
		set(replaced[i])
	}
	for field, v := range d.roots {
		encoded, err := patchStrings(d.raws[field], v)
		if err != nil {
			return err
		}
		b.set(field, encoded)
	}
	return nil
}

// patchStrings writes into raw the strings that differ in v, the value raw
// decodes to after a filter's rewrite, and leaves every other byte as it came.
//
// Encoding v again would sort every object's keys, the model's own tool
// inputs in earlier turns included. Those turns would then reach the provider
// changed: its prompt cache would miss from the first of them, and Anthropic
// may refuse or drop a thinking block next to an edited turn.
//
// A raw it cannot map, such as one with a key twice, is encoded again whole,
// which also keeps one copy of the key: the one the filters read.
func patchStrings(raw []byte, v any) ([]byte, error) {
	var edits []stringEdit
	root, err := spansOf(jsontext.NewDecoder(bytes.NewReader(raw)))
	if err != nil || !changedStrings(v, root, &edits) {
		return json.Marshal(v)
	}
	slices.SortFunc(edits, func(a, b stringEdit) int { return a.start - b.start })
	out := make([]byte, 0, len(raw))
	last := 0
	for _, e := range edits {
		out = append(out, raw[last:e.start]...)
		out = appendQuoted(out, e.value)
		last = e.end
	}
	return append(out, raw[last:]...), nil
}

// rawNode is where one value sits in the bytes it was decoded from.
type rawNode struct {
	kind jsontext.Kind
	// start and end bound a scalar, quotes included. str is a string's value.
	start, end int
	str        string
	members    map[string]*rawNode
	elems      []*rawNode
}

// stringEdit is one string to write over raw[start:end].
type stringEdit struct {
	start, end int
	value      string
}

// spansOf maps the next value dec reads. The decoder refuses a key that comes
// twice in one object, which is what sends such a raw to be encoded whole.
func spansOf(dec *jsontext.Decoder) (*rawNode, error) {
	switch dec.PeekKind() {
	case '{', '[':
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		n := &rawNode{kind: tok.Kind(), members: map[string]*rawNode{}}
		for k := dec.PeekKind(); k != '}' && k != ']'; k = dec.PeekKind() {
			var name string
			if n.kind == '{' {
				tok, err := dec.ReadToken()
				if err != nil {
					return nil, err
				}
				name = tok.String()
			}
			child, err := spansOf(dec)
			if err != nil {
				return nil, err
			}
			if n.kind == '{' {
				n.members[name] = child
			} else {
				n.elems = append(n.elems, child)
			}
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return n, nil
	}
	val, err := dec.ReadValue()
	if err != nil {
		return nil, err
	}
	end := int(dec.InputOffset())
	n := &rawNode{kind: val.Kind(), start: end - len(val), end: end}
	if n.kind == '"' {
		s, err := jsontext.AppendUnquote(nil, val)
		if err != nil {
			return nil, err
		}
		n.str = string(s)
	}
	return n, nil
}

// changedStrings collects the strings in v that differ from those at n. It
// returns false when v is not shaped like n, which a filter never causes.
func changedStrings(v any, n *rawNode, edits *[]stringEdit) bool {
	switch v := v.(type) {
	case string:
		if n.kind != '"' {
			return false
		}
		if v != n.str {
			*edits = append(*edits, stringEdit{n.start, n.end, v})
		}
	case map[string]any:
		if n.kind != '{' || len(n.members) != len(v) {
			return false
		}
		for k, e := range v {
			if c, ok := n.members[k]; !ok || !changedStrings(e, c, edits) {
				return false
			}
		}
	case []any:
		if n.kind != '[' || len(n.elems) != len(v) {
			return false
		}
		for i, e := range v {
			if !changedStrings(e, n.elems[i], edits) {
				return false
			}
		}
	}
	return true
}

// objects is the objects in a decoded array, skipping anything else. It
// checks their keys, as each holds text the filters read.
func (d *treeDoc) objects(v any) []map[string]any {
	arr, _ := v.([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if obj, ok := e.(map[string]any); ok {
			if err := checkKeys(obj); err != nil && d.err == nil {
				d.err = err
			}
			out = append(out, obj)
		}
	}
	return out
}
