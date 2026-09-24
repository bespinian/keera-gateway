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
// translated, as before.

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
	// rename writes the alias over the backend's model name in a buffered
	// answer.
	rename(raw []byte, alias string) []byte
	// stream reads a streamed answer as it passes through.
	stream(alias string) nativeStream
	// stripHostedTools takes out the tools the provider runs on its own
	// servers, and returns what it took out. See hosted.go.
	stripHostedTools(b *body) []string
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

// speaksNative reports whether a destination is sent the client's own API.
func speaksNative(d dialect, m policy.Model) bool {
	return d != nil && m.Provider == d.provider()
}

// relayNativeBuffered is relayBuffered for an answer in the client's own API:
// only the model name changes.
func (s *Server) relayNativeBuffered(c *call, resp *http.Response, answered time.Time) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, s.opts.MaxResponseBytes))
	var usage *tokenUsage
	if err == nil && resp.StatusCode < 300 {
		usage = c.surf.dialect.usage(raw)
		raw = c.surf.dialect.rename(raw, c.alias)
	}
	if msg := bufferedError(err, raw, resp.StatusCode, resp.StatusCode); msg != "" {
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
	lines := newSSELines(src, limit)
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
	for {
		line, readErr := lines.next()
		if len(line) > 0 {
			if int64(len(event)+len(line)) > limit {
				stats.usage, stats.deltas, stats.partial = ns.counts()
				return stats, errEventTooLarge
			}
			event = append(event, line...)
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				if err := emit(); err != nil {
					stats.usage, stats.deltas, stats.partial = ns.counts()
					return stats, err
				}
			case bytes.HasPrefix(trimmed, dataPrefix):
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimSpace(trimmed[len(dataPrefix):])...)
			default:
				head = append(head, trimmed...)
				head = append(head, '\n')
			}
		}
		if readErr != nil {
			err := emit()
			stats.usage, stats.deltas, stats.partial = ns.counts()
			switch {
			case err != nil:
				return stats, err
			case errors.Is(readErr, io.EOF):
				return stats, nil
			default:
				return stats, readErr
			}
		}
	}
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
