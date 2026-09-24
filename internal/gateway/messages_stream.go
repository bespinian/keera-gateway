package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

var pingEvent = []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")

// errStreamAbandoned stops the reader when the client has gone away. It is
// never returned to a caller.
var errStreamAbandoned = errors.New("gateway: client stopped reading the stream")

// pipe forwards a chat completion stream as a Messages stream.
//
// Reading and translating run in a goroutine that hands finished events over
// a channel. Only this goroutine writes to dst, so pings and events never
// write at the same time.
func (anthropicShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	_ bool,
) (streamStats, error) {
	var (
		stats    streamStats
		events   = make(chan []byte, 64)
		done     = make(chan struct{})
		finished = make(chan struct{})
		readErr  error
	)

	st := &messagesStream{alias: alias, msgID: id.New("msg"), openIndex: -1}
	st.send = func(b []byte) error {
		select {
		case events <- b:
			return nil
		case <-done:
			return errStreamAbandoned
		}
	}

	go func() {
		defer close(finished)
		defer close(events)
		readErr = scanSSE(src, st.chunk)
		if errors.Is(readErr, errStreamAbandoned) {
			return
		}
		// The message is closed even after an upstream failure: a client
		// waiting for message_stop would hang, which is worse than a short turn.
		if err := st.finish(); err != nil && readErr == nil {
			readErr = err
		}
	}()

	ticker := time.NewTicker(anthropicPingInterval)
	defer ticker.Stop()

	var writeErr error
loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			if stats.firstAt.IsZero() {
				stats.firstAt = time.Now()
			}
			if _, err := dst.Write(ev); err != nil {
				writeErr = err
				break loop
			}
			flush()
			ticker.Reset(anthropicPingInterval)
		case <-ticker.C:
			if _, err := dst.Write(pingEvent); err != nil {
				writeErr = err
				break loop
			}
			flush()
		}
	}
	close(done)
	<-finished

	stats.usage, stats.deltas = st.usage, st.deltas
	switch {
	case writeErr != nil:
		return stats, writeErr
	case errors.Is(readErr, errStreamAbandoned):
		return stats, nil
	default:
		return stats, readErr
	}
}

// scanSSE walks the data payloads of a server-sent event stream, joining an
// event's data lines the way the format says to and skipping the terminator.
func scanSSE(src io.Reader, fn func(payload []byte) error) error {
	lines := newSSELines(src, maxLineBytes)
	// data holds one event's payload and is reused for the next, to avoid an
	// allocation per token. fn must not keep what it is given.
	var data []byte

	complete := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := data
		data = data[:0]
		if bytes.Equal(bytes.TrimSpace(payload), doneMarker) {
			return nil
		}
		return fn(payload)
	}

	for {
		line, err := lines.next()
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				if cerr := complete(); cerr != nil {
					return cerr
				}
			case bytes.HasPrefix(trimmed, dataPrefix):
				chunk := bytes.TrimSpace(trimmed[len(dataPrefix):])
				if len(data)+len(chunk) > maxLineBytes {
					return errEventTooLarge
				}
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, chunk...)
			}
		}
		if err != nil {
			if cerr := complete(); cerr != nil {
				return cerr
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// oaiStreamChunk is one chunk of a chat completion stream.
type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string             `json:"content"`
			ToolCalls []oaiToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *tokenUsage `json:"usage"`
}

// oaiToolCallDelta is one fragment of a streamed tool call.
type oaiToolCallDelta struct {
	// Index is which of several parallel tool calls this fragment belongs to.
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// messagesStream is the state a chat completion stream has to be read against
// to be re-emitted as a Messages stream.
type messagesStream struct {
	alias string
	msgID string
	send  func([]byte) error

	started   bool
	nextIndex int
	// openIndex is the content block currently open, or -1. Blocks open and
	// close in sequence, so a fragment for another block closes this one first.
	openIndex int
	openTool  int // upstream tool_call index behind the open block, if any
	isTool    bool

	stopReason string
	usage      *tokenUsage
	deltas     int
}

// chunk folds one upstream chunk into the stream.
func (s *messagesStream) chunk(payload []byte) error {
	var c oaiStreamChunk
	if err := json.Unmarshal(payload, &c); err != nil {
		// An unreadable chunk is skipped: the rest of the turn is still worth
		// delivering.
		return nil
	}
	if c.Usage != nil {
		s.usage = c.Usage
	}
	if len(c.Choices) == 0 {
		// The usage-only chunk. Its numbers go into message_delta at the end.
		return nil
	}

	choice := c.Choices[0]
	if choice.FinishReason != "" {
		s.stopReason = stopReason(choice.FinishReason)
	}
	if text := choice.Delta.Content; text != "" {
		if err := s.text(text); err != nil {
			return err
		}
	}
	for _, tc := range choice.Delta.ToolCalls {
		if err := s.toolCall(tc); err != nil {
			return err
		}
	}
	return nil
}

// text emits a fragment of text, opening a text block first if needed.
func (s *messagesStream) text(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if s.openIndex < 0 || s.isTool {
		if err := s.closeBlock(); err != nil {
			return err
		}
		if err := s.openText(); err != nil {
			return err
		}
	}
	s.deltas++
	return s.emit("content_block_delta", textDeltaEvent{
		Type: "content_block_delta", Index: s.openIndex,
		Delta: textDelta{Type: "text_delta", Text: text},
	})
}

// toolCall emits a fragment of a tool call, opening its block first if needed.
func (s *messagesStream) toolCall(tc oaiToolCallDelta) error {
	if err := s.start(); err != nil {
		return err
	}
	index := 0
	if tc.Index != nil {
		index = *tc.Index
	}
	if s.openIndex < 0 || !s.isTool || s.openTool != index {
		if err := s.closeBlock(); err != nil {
			return err
		}
		if err := s.openToolUse(index, tc.ID, tc.Function.Name); err != nil {
			return err
		}
	}
	args := tc.Function.Arguments
	if args == "" {
		return nil
	}
	s.deltas++
	return s.emit("content_block_delta", toolDeltaEvent{
		Type: "content_block_delta", Index: s.openIndex,
		Delta: jsonDelta{Type: "input_json_delta", PartialJSON: args},
	})
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
	s.openIndex, s.isTool = s.nextIndex, false
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.openIndex,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
}

func (s *messagesStream) openToolUse(upstreamIndex int, callID, name string) error {
	s.openIndex, s.isTool, s.openTool = s.nextIndex, true, upstreamIndex
	s.nextIndex++
	return s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.openIndex,
		"content_block": map[string]any{
			"type": "tool_use", "id": toolUseID(callID), "name": name,
			// The arguments follow as input_json_delta fragments.
			"input": map[string]any{},
		},
	})
}

func (s *messagesStream) closeBlock() error {
	if s.openIndex < 0 {
		return nil
	}
	index := s.openIndex
	s.openIndex, s.isTool = -1, false
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

	reason := s.stopReason
	if reason == "" {
		reason = "end_turn"
	}
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

// The two per-token events are structs rather than maps, because they are
// encoded once per token per stream and a map is slower to marshal. They are
// separate types, not one with omitempty fields, because an empty fragment
// would make a broken event.
type textDeltaEvent struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta textDelta `json:"delta"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolDeltaEvent struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta jsonDelta `json:"delta"`
}

type jsonDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

// emit renders one event and hands it to the writer.
func (s *messagesStream) emit(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(name)+len(data)+20)
	buf = append(buf, "event: "...)
	buf = append(buf, name...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	return s.send(buf)
}
