package gateway

import (
	"encoding/json"
	"errors"

	"github.com/bespinian/keera-gateway/internal/policy"
)

// textDoc is the text of one request body that filters and routers read: the
// strings a filter may rewrite, and how to put them back. Only prose counts.
// Tool-call arguments, images and audio are left out, since rewriting them
// could produce a request the plane rejects. That is a real limit of this
// guardrail.
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
	case policy.KindRerank:
		return extractRerank(b)
	default:
		return extractScalar(b, "input"), nil
	}
}

// extractChat is the text inside a messages array: string contents, and the
// text parts of multimodal ones. A part it cannot read is refused rather than
// skipped, as a model may read text no filter saw.
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
		if err := addChatContent(d, msg); err != nil {
			return nil, err
		}
		// Chat templates such as Qwen's show earlier reasoning to the model.
		if err := addText(d, msg, "reasoning_content"); err != nil {
			return nil, err
		}
		if err := addText(d, msg, "reasoning"); err != nil {
			return nil, err
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return d, nil
}

// addChatContent collects a message's content: a string, or an array of
// parts, where vLLM also reads a plain string as a text part.
func addChatContent(d *treeDoc, msg map[string]any) error {
	switch content := msg["content"].(type) {
	case nil:
	case string:
		d.addField(msg, "content")
	case []any:
		for i, e := range content {
			switch part := e.(type) {
			case string:
				d.add(part, func(r string) { content[i] = r })
			case map[string]any:
				if err := checkKeys(part); err != nil {
					return err
				}
				if err := addChatPart(d, part); err != nil {
					return err
				}
			default:
				return errors.New("each part of a message's 'content' must be a string or an object")
			}
		}
	default:
		return errors.New("a message's 'content' must be a string or an array")
	}
	return nil
}

// chatMediaParts carry no text a filter could read: images, audio, video and
// files pass through unchanged.
var chatMediaParts = map[string]bool{
	"image_url": true, "input_image": true, "image_embeds": true,
	"input_audio": true, "audio_url": true, "video_url": true, "file": true,
}

// addChatPart collects the text of one content part. OpenAI and vLLM read
// refusals and the Responses API's text parts in a chat too.
func addChatPart(d *treeDoc, part map[string]any) error {
	t, _ := part["type"].(string)
	switch {
	case t == "text" || t == "input_text" || t == "output_text" ||
		t == "" && part["text"] != nil:
		return addText(d, part, "text")
	case t == "refusal":
		return addText(d, part, "refusal")
	case t == "thinking":
		return addText(d, part, "thinking")
	case t == "search_result" || t == "document":
		addTextBlock(d, part)
		return nil
	case chatMediaParts[t]:
		return nil
	case t == "":
		return errors.New("a content part has no 'type'")
	default:
		return errors.New("a content part of type '" + t + "' cannot be read")
	}
}

// addText collects obj[key] when it is a string, and refuses any other value,
// which an upstream might still read as text.
func addText(d *treeDoc, obj map[string]any, key string) error {
	switch obj[key].(type) {
	case nil:
		return nil
	case string:
		d.addField(obj, key)
		return nil
	default:
		return errors.New("the '" + key + "' field must be a string")
	}
}

// extractRerank is the query and each document: a string, or the text of an
// object, as some rerank APIs take.
func extractRerank(b *body) (*treeDoc, error) {
	d := newTreeDoc()
	query, err := d.root(b, "query")
	if err != nil {
		return nil, err
	}
	switch s := query.(type) {
	case nil:
	case string:
		d.add(s, func(r string) { d.roots["query"] = r })
	default:
		return nil, errors.New("the 'query' field must be a string")
	}
	// llama.cpp also reads the documents from "texts", as Hugging Face's
	// text embeddings inference names them.
	for _, field := range []string{"documents", "texts"} {
		docs, err := d.root(b, field)
		if err != nil {
			return nil, err
		}
		if err := addDocuments(d, field, docs); err != nil {
			return nil, err
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return d, nil
}

// addDocuments collects the documents to rank: each a string, or an object
// with its text.
func addDocuments(d *treeDoc, field string, docs any) error {
	if docs == nil {
		return nil
	}
	arr, ok := docs.([]any)
	if !ok {
		return errors.New("the '" + field + "' field must be an array")
	}
	for i, e := range arr {
		switch doc := e.(type) {
		case string:
			d.add(doc, func(r string) { arr[i] = r })
		case map[string]any:
			if err := checkKeys(doc); err != nil {
				return err
			}
			if _, ok := doc["text"].(string); !ok {
				return errors.New("each object in '" + field + "' must have a string 'text'")
			}
			d.addField(doc, "text")
		case json.Number:
			// A number has no text to read.
		default:
			return errors.New("each entry in '" + field + "' must be a string or an object")
		}
	}
	return nil
}

// extractScalar is the text of a field that is one string or an array, such
// as a completion's prompt. An array of token ids has no text.
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
