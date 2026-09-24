package gateway

import (
	"encoding/json"
	"errors"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// textDoc is the text of one request body that filters and routers read: the
// strings a filter may rewrite, and the way to put the rewrites back.
//
// Only prose counts: message contents, the text parts of a multimodal
// message, a completion prompt, an embedding input. Tool-call arguments,
// images and audio are not shown, because rewriting them could produce a
// request the inference plane rejects. That is a real limit of this guardrail.
type textDoc interface {
	texts() []string
	apply(b *body, replaced []string) error
}

func extractText(b *body, kind policy.Kind) (textDoc, error) {
	switch kind {
	case policy.KindChat:
		return extractChat(b)
	case policy.KindCompletion:
		return extractScalar(b, "prompt"), nil
	default:
		return extractScalar(b, "input"), nil
	}
}

// chatDoc is the text inside a messages array.
type chatDoc struct {
	msgs []map[string]json.RawMessage
	// parts holds the decoded content parts of the messages that carry an
	// array rather than a string, by message index.
	parts map[int][]map[string]json.RawMessage
	// refs points each collected string back at where it came from: a message
	// index, and a part index or -1 for a message whose content is a string.
	refs   []struct{ msg, part int }
	values []string
}

func extractChat(b *body) (*chatDoc, error) {
	raw, ok := b.value("messages")
	if !ok {
		return nil, errors.New("the 'messages' field is required")
	}
	d := &chatDoc{parts: map[int][]map[string]json.RawMessage{}}
	if err := json.Unmarshal(raw, &d.msgs); err != nil {
		return nil, errors.New("the 'messages' field must be an array of objects")
	}
	for i, msg := range d.msgs {
		content, ok := msg["content"]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(content, &s) == nil {
			d.add(i, -1, s)
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(content, &parts) != nil {
			continue // null, or a shape this gateway does not address
		}
		d.parts[i] = parts
		for j, part := range parts {
			var typ, text string
			if json.Unmarshal(part["type"], &typ) != nil || typ != "text" {
				continue
			}
			if json.Unmarshal(part["text"], &text) != nil {
				continue
			}
			d.add(i, j, text)
		}
	}
	return d, nil
}

func (d *chatDoc) add(msg, part int, s string) {
	d.refs = append(d.refs, struct{ msg, part int }{msg, part})
	d.values = append(d.values, s)
}

func (d *chatDoc) texts() []string { return d.values }

func (d *chatDoc) apply(b *body, replaced []string) error {
	if len(replaced) != len(d.refs) {
		return errors.New("the filtered request had a different number of segments")
	}
	for i, ref := range d.refs {
		encoded, err := json.Marshal(replaced[i])
		if err != nil {
			return err
		}
		if ref.part < 0 {
			d.msgs[ref.msg]["content"] = encoded
			continue
		}
		d.parts[ref.msg][ref.part]["text"] = encoded
	}
	for i, parts := range d.parts {
		encoded, err := json.Marshal(parts)
		if err != nil {
			return err
		}
		d.msgs[i]["content"] = encoded
	}
	encoded, err := json.Marshal(d.msgs)
	if err != nil {
		return err
	}
	b.set("messages", encoded)
	return nil
}

// scalarDoc is the text of a field that is either one string or an array of
// them: a completion's prompt, an embedding's input.
type scalarDoc struct {
	field  string
	array  bool
	values []string
}

func extractScalar(b *body, field string) *scalarDoc {
	d := &scalarDoc{field: field}
	raw, ok := b.value(field)
	if !ok {
		return d
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		d.values = []string{s}
		return d
	}
	if json.Unmarshal(raw, &d.values) == nil {
		d.array = true
		return d
	}
	// An array of token ids, not text. There is nothing for a filter to read.
	d.values = nil
	return d
}

func (d *scalarDoc) texts() []string { return d.values }

func (d *scalarDoc) apply(b *body, replaced []string) error {
	if len(replaced) != len(d.values) {
		return errors.New("the filtered request had a different number of segments")
	}
	if len(replaced) == 0 {
		return nil
	}
	var (
		encoded []byte
		err     error
	)
	if d.array {
		encoded, err = json.Marshal(replaced)
	} else {
		encoded, err = json.Marshal(replaced[0])
	}
	if err != nil {
		return err
	}
	b.set(d.field, encoded)
	return nil
}
