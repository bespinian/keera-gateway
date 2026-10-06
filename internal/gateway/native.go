package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
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
// translated, as before. The translation is made only when a router or such a
// destination needs it: on a long session it costs far more than the rest of
// the gateway's work.

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
}

// openingOf is the first user message in msgs, encoded as firstUserMessage
// reads it out of a translated body.
func openingOf(msgs []oaiMessage) (json.RawMessage, bool) {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		var v any = m
		if m.Content != nil {
			v = m.Content
		}
		raw, err := json.Marshal(v)
		return raw, err == nil
	}
	return nil, false
}

// speaksNative reports whether a destination is sent the client's own API.
func speaksNative(d dialect, m policy.Model) bool {
	return d != nil && m.Provider == d.provider()
}

// relayNativeBuffered is relayBuffered for an answer in the client's own API:
// only the model name changes.
func (s *Server) relayNativeBuffered(c *call, resp *http.Response, answered time.Time) {
	raw, ok := s.readAnswer(c, resp, answered)
	if !ok {
		return
	}
	var usage *tokenUsage
	if resp.StatusCode < 300 {
		usage = c.surf.dialect.usage(raw)
		raw = renameIn(raw, "", c.alias)
	}
	if msg := bufferedError(raw, resp.StatusCode, resp.StatusCode); msg != "" {
		c.ev.Error = msg
	}
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(raw)
	c.ev.TTFT = time.Since(c.tr.start)
	c.tr.since(store.SpanRespond, c.alias, answered, "")
	if usage != nil {
		setUsage(&c.ev, usage)
	}
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
		if stats.firstAt.IsZero() {
			stats.firstAt = time.Now()
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
	roots  map[string]any
	values []string
	set    []func(string)
}

func newTreeDoc() *treeDoc { return &treeDoc{roots: map[string]any{}} }

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
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b.set(field, encoded)
	}
	return nil
}

// objects is the objects in a decoded array, skipping anything else.
func objects(v any) []map[string]any {
	arr, _ := v.([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if obj, ok := e.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}
