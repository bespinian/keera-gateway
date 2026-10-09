package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// The streaming half of the Responses surface.
//
// Like a Messages stream, a Responses stream is explicit events: the response
// opens, output items open and close by index, and the response closes with a
// status and the token counts. Clients read the finished items from the
// events that close them, so those carry the whole item.

// pipe forwards a chat completion stream as a Responses stream. The API has
// no keep-alive event, so unlike the Messages stream this one writes only
// when the upstream does.
func (responsesShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	_ bool, limit int64,
) (streamStats, error) {
	return pipeEvents(dst, flush, src, limit, 0, func(send func([]byte) error) eventStream {
		return &responsesStream{alias: alias, respID: id.New("resp"), createdAt: time.Now().Unix(),
			open: -1, send: send}
	})
}

// responsesStream is the state a chat completion stream has to be read against
// to be re-emitted as a Responses stream.
type responsesStream struct {
	alias     string
	respID    string
	createdAt int64
	send      func([]byte) error
	seq       int

	started bool
	// done is every finished output item, for the event that closes the
	// response.
	done []any
	// blocks is the output item under way.
	blocks
	itemID string
	callID string
	name   string
	text   strings.Builder

	chatChunks
}

// chunk folds one upstream chunk into the stream.
func (s *responsesStream) chunk(payload []byte) error {
	return s.fold(payload, s.textDelta, s.toolDelta)
}

func (s *responsesStream) textDelta(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(false, 0, s.closeItem, s.openMessage); err != nil {
		return err
	}
	s.deltas++
	s.text.WriteString(text)
	return s.emit("response.output_text.delta", map[string]any{
		"item_id": s.itemID, "output_index": s.open, "content_index": 0, "delta": text,
		"logprobs": []any{},
	})
}

func (s *responsesStream) toolDelta(index int, tc oaiToolCallDelta) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(true, index, s.closeItem, func() error {
		return s.openCall(index, tc.ID, tc.Function.Name)
	}); err != nil {
		return err
	}
	if tc.Function.Arguments == "" {
		return nil
	}
	s.deltas++
	s.text.WriteString(tc.Function.Arguments)
	return s.emit("response.function_call_arguments.delta", map[string]any{
		"item_id": s.itemID, "output_index": s.open, "delta": tc.Function.Arguments,
	})
}

// response is the response object as the events that open and close the
// stream carry it.
func (s *responsesStream) response(status string, incomplete any) responsesOut {
	output := s.done
	if output == nil {
		output = []any{}
	}
	return responsesOut{
		ID: s.respID, Object: "response", CreatedAt: s.createdAt, Status: status,
		IncompleteDetails: incomplete, Model: s.alias, Output: output,
		Usage: newResponsesUsage(s.usage),
	}
}

func (s *responsesStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := s.emit("response.created", map[string]any{
		"response": s.response("in_progress", nil),
	}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{
		"response": s.response("in_progress", nil),
	})
}

func (s *responsesStream) openMessage() error {
	s.open, s.isTool, s.itemID = len(s.done), false, id.New("msg")
	s.text.Reset()
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": s.open,
		"item": map[string]any{
			"type": "message", "id": s.itemID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{
		"item_id": s.itemID, "output_index": s.open, "content_index": 0, "part": outputText(""),
	})
}

func (s *responsesStream) openCall(upstreamIndex int, upstreamID, name string) error {
	s.open, s.isTool, s.tool, s.itemID = len(s.done), true, upstreamIndex, id.New("fc")
	s.callID, s.name = callID(upstreamID), name
	s.text.Reset()
	return s.emit("response.output_item.added", map[string]any{
		"output_index": s.open,
		"item":         callItem(s.itemID, "in_progress", s.callID, s.name, ""),
	})
}

// closeItem closes the open item, repeating it whole in the events that close
// it.
func (s *responsesStream) closeItem() error {
	if s.open < 0 {
		return nil
	}
	index, text := s.open, s.text.String()
	s.open = -1
	if s.isTool {
		item := callItem(s.itemID, "completed", s.callID, s.name, text)
		s.done = append(s.done, item)
		if err := s.emit("response.function_call_arguments.done", map[string]any{
			"item_id": s.itemID, "output_index": index, "arguments": text,
		}); err != nil {
			return err
		}
		return s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
	item := messageItem(s.itemID, "completed", text)
	s.done = append(s.done, item)
	if err := s.emit("response.output_text.done", map[string]any{
		"item_id": s.itemID, "output_index": index, "content_index": 0, "text": text,
		"logprobs": []any{},
	}); err != nil {
		return err
	}
	if err := s.emit("response.content_part.done", map[string]any{
		"item_id": s.itemID, "output_index": index, "content_index": 0, "part": outputText(text),
	}); err != nil {
		return err
	}
	return s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

// finish closes the response, with the usage record the last chunk carried.
func (s *responsesStream) finish() error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.closeItem(); err != nil {
		return err
	}
	status, incomplete := responseStatus(s.finishReason)
	name := "response.completed"
	if status == "incomplete" {
		name = "response.incomplete"
	}
	return s.emit(name, map[string]any{"response": s.response(status, incomplete)})
}

// emit renders one event, numbering it, and hands it to the writer.
func (s *responsesStream) emit(name string, payload map[string]any) error {
	payload["type"] = name
	payload["sequence_number"] = s.seq
	s.seq++
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.send(frame(name, data))
}
