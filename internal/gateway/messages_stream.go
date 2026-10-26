package gateway

import (
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/id"
)

// The streaming half of the Messages surface. A chat completion stream is
// deltas to one implicit message; a Messages stream has explicit events where
// the message and its blocks open and close. So the translation tracks which
// block is open.

// anthropicPingInterval is how often a ping is sent while the upstream is
// silent, since clients abort a quiet stream, as before the first token of a
// big prompt. It is a variable for tests.
var anthropicPingInterval = 20 * time.Second

func (anthropicShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	_ bool, limit int64,
) (streamStats, error) {
	return pipeEvents(dst, flush, src, limit, anthropicPingInterval,
		func(send func([]byte) error) eventStream {
			return &messagesStream{alias: alias, msgID: id.New("msg"), open: -1, send: send}
		})
}

// messagesStream re-emits a chat completion stream as a Messages stream.
type messagesStream struct {
	alias string
	msgID string
	// send writes one event. It must not keep the slice, which is reused.
	send func([]byte) error
	buf  []byte
	// args holds the open tool call's arguments until its block closes.
	args []byte

	started   bool
	nextIndex int
	blocks
	chatChunks
}

func (s *messagesStream) chunk(payload []byte) error {
	return s.fold(payload, s.text, s.toolCall)
}

func (s *messagesStream) text(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(false, 0, s.closeBlock, s.openText); err != nil {
		return err
	}
	return s.delta("text_delta", "text", text)
}

func (s *messagesStream) toolCall(index int, tc oaiToolCallDelta) error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.enter(true, index, s.closeBlock, func() error {
		return s.openToolUse(index, tc.ID, tc.Function.Name)
	}); err != nil {
		return err
	}
	// The arguments are held until the call is whole and checked, since an
	// Anthropic SDK fails the whole answer on arguments that are not JSON.
	s.args = append(s.args, tc.Function.Arguments...)
	return nil
}

func (s *messagesStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	// Usage comes in the last chunk, so the counts are sent in message_delta.
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
			// The arguments follow at closeBlock, unless they are {}.
			"input": map[string]any{},
		},
	})
}

func (s *messagesStream) closeBlock() error {
	if s.open < 0 {
		return nil
	}
	if s.isTool {
		args := toolInput(string(s.args))
		s.args = s.args[:0]
		if string(args) != "{}" {
			if err := s.delta("input_json_delta", "partial_json", string(args)); err != nil {
				return err
			}
		}
	}
	index := s.open
	s.open, s.isTool = -1, false
	return s.emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// finish closes the message, or ends the stream with an error if the upstream
// failed, so a failure does not read as a finished turn.
func (s *messagesStream) finish(cut error) error {
	if cut != nil {
		// overloaded_error, since retrying is the remedy.
		return s.emit("error", map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "overloaded_error", "message": cutMessage(cut)},
		})
	}
	// Even an empty stream must be a whole message.
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

// delta sends a content_block_delta to the open block. It runs once per token,
// so it writes into a reused buffer instead of marshalling.
func (s *messagesStream) delta(deltaType, key, value string) error {
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

func (s *messagesStream) emit(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.send(frame(name, data))
}
