package gateway

import (
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// The streaming half of the Messages surface, where the two APIs differ most.
//
// A chat completion stream is deltas against one implicit message. A Messages
// stream is explicit events: the message opens, content blocks open and close
// by index, and the message closes with a stop reason and token counts. So
// the translation is a state machine that tracks which block is open.

// anthropicPingInterval is how often a ping is sent while the upstream is
// silent. Clients abort a quiet stream, and the long quiet is before the first
// token, while a big prompt is read. The Messages API fills it with pings.
//
// A variable so a test need not wait twenty seconds.
var anthropicPingInterval = 20 * time.Second

// pipe forwards a chat completion stream as a Messages stream.
func (anthropicShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	_ bool, limit int64,
) (streamStats, error) {
	return pipeEvents(dst, flush, src, limit, anthropicPingInterval,
		func(send func([]byte) error) eventStream {
			return &messagesStream{alias: alias, msgID: id.New("msg"), blocks: blocks{open: -1}, send: send}
		})
}

// messagesStream is the state a chat completion stream has to be read against
// to be re-emitted as a Messages stream.
type messagesStream struct {
	alias string
	msgID string
	// send writes one event. It must not keep the slice, which is reused.
	send func([]byte) error
	// buf is reused for every token's event.
	buf []byte

	started   bool
	nextIndex int
	blocks
	chatChunks
}

// chunk folds one upstream chunk into the stream.
func (s *messagesStream) chunk(payload []byte) error {
	return s.fold(payload, s.text, s.toolCall)
}

// text emits a fragment of text, opening a text block first if needed.
func (s *messagesStream) text(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(false, 0, s.closeBlock, s.openText); err != nil {
		return err
	}
	return s.delta("text_delta", "text", text)
}

// toolCall emits a fragment of a tool call, opening its block first if needed.
func (s *messagesStream) toolCall(index int, tc oaiToolCallDelta) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(true, index, s.closeBlock, func() error {
		return s.openToolUse(index, tc.ID, tc.Function.Name)
	}); err != nil {
		return err
	}
	args := tc.Function.Arguments
	if args == "" {
		return nil
	}
	return s.delta("input_json_delta", "partial_json", args)
}

// start opens the message, once.
func (s *messagesStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	// The input token count is not known yet: usage comes in the last chunk.
	// It is zero here and sent for real in message_delta, which is where
	// clients read it.
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.msgID, "type": "message", "role": "assistant", "model": s.alias,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *messagesStream) openText() error {
	s.open, s.isTool = s.nextIndex, false
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.open,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
}

func (s *messagesStream) openToolUse(upstreamIndex int, callID, name string) error {
	s.open, s.isTool, s.tool = s.nextIndex, true, upstreamIndex
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.open,
		"content_block": map[string]any{
			"type": "tool_use", "id": toolUseID(callID), "name": name,
			// The arguments follow as input_json_delta fragments.
			"input": map[string]any{},
		},
	})
}

func (s *messagesStream) closeBlock() error {
	if s.open < 0 {
		return nil
	}
	index := s.open
	s.open, s.isTool = -1, false
	return s.emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// finish closes the message.
func (s *messagesStream) finish() error {
	// Even an empty stream must be a whole message, starting with
	// message_start.
	if err := s.start(); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}

	reason := stopReason(s.finishReason)
	usage := map[string]int{"input_tokens": 0, "output_tokens": 0}
	if s.usage != nil {
		usage["input_tokens"] = s.usage.InputTokens
		usage["output_tokens"] = s.usage.OutputTokens
	}
	if err := s.emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": reason, "stop_sequence": nil,
		},
		"usage": usage,
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}

// delta sends a content_block_delta event to the open block. It runs once per
// token, so it is written into a reused buffer rather than marshalled.
func (s *messagesStream) delta(deltaType, key, value string) error {
	s.deltas++
	b := append(s.buf[:0], "event: content_block_delta\ndata: "+
		`{"type":"content_block_delta","index":`...)
	b = strconv.AppendInt(b, int64(s.open), 10)
	b = append(b, `,"delta":{"type":"`...)
	b = append(b, deltaType...)
	b = append(b, `","`...)
	b = append(b, key...)
	b = append(b, `":`...)
	b = appendQuoted(b, value)
	b = append(b, "}}\n\n"...)
	s.buf = b
	return s.send(b)
}

// emit renders one event and hands it to the writer.
func (s *messagesStream) emit(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.send(frame(name, data))
}
