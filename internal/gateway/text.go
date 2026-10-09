package gateway

import (
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

// extractChat is the text inside a messages array: string contents, and the
// text parts of multimodal ones.
func extractChat(b *body) (*treeDoc, error) {
	if _, ok := b.value("messages"); !ok {
		return nil, errors.New("the 'messages' field is required")
	}
	d := newTreeDoc()
	msgs, err := d.root(b, "messages")
	if err != nil {
		return nil, err
	}
	arr, isArray := msgs.([]any)
	objs := d.objects(msgs)
	if msgs != nil && (!isArray || len(objs) != len(arr)) {
		return nil, errors.New("the 'messages' field must be an array of objects")
	}
	for _, msg := range objs {
		d.addField(msg, "content")
		for _, part := range d.objects(msg["content"]) {
			addTextBlock(d, part)
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return d, nil
}

// extractScalar is the text of a field that is either one string or an array:
// a completion's prompt, an embedding's input. Token ids are not text, so an
// array of them gives a filter nothing to read.
func extractScalar(b *body, field string) *treeDoc {
	d := newTreeDoc()
	v, _ := d.root(b, field)
	if s, ok := v.(string); ok {
		d.add(s, func(r string) { d.roots[field] = r })
	}
	arr, _ := v.([]any)
	for i, e := range arr {
		if s, ok := e.(string); ok {
			d.add(s, func(r string) { arr[i] = r })
		}
	}
	return d
}
