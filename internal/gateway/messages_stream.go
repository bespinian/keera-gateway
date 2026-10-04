package gateway

import (
	"encoding/json"
	"io"
	"strconv"
	"sync"
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

// pipe forwards a chat completion stream as a Messages stream.
//
// Events are written as they are made, by the goroutine reading upstream.
// Only the pings come from elsewhere, a timer, since they are needed exactly
// when that goroutine is waiting.
func (anthropicShape) pipe(dst io.Writer, flush func(), src io.Reader, alias string,
	_ bool, limit int64,
) (streamStats, error) {
	out := newPinger(dst, flush, anthropicPingInterval)
	st := &messagesStream{alias: alias, msgID: id.New("msg"), openIndex: -1, send: out.write}

	readErr := scanSSE(src, limit, st.chunk)
	// The message is closed even after an upstream failure: a client waiting
	// for message_stop would hang, which is worse than a short turn. A client
	// that is gone is sent nothing more.
	if !out.failed() {
		if err := st.finish(); err != nil && readErr == nil {
			readErr = err
		}
	}
	writeErr := out.stop()

	stats := streamStats{usage: st.usage, deltas: st.deltas, firstAt: out.firstAt}
	if writeErr != nil {
		return stats, writeErr
	}
	return stats, readErr
}

// pinger writes a stream's events, and a ping whenever nothing has been
// written for a whole interval.
type pinger struct {
	dst   io.Writer
	flush func()
	every time.Duration

	mu    sync.Mutex
	timer *time.Timer
	// last is when anything was last written, and firstAt when the first
	// event was: the wait a developer feels.
	last, firstAt time.Time
	// err is the first failed write. Nothing is written after it.
	err     error
	stopped bool
}

func newPinger(dst io.Writer, flush func(), every time.Duration) *pinger {
	p := &pinger{dst: dst, flush: flush, every: every, last: time.Now()}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.timer = time.AfterFunc(every, p.tick)
	return p
}

// write writes one event. It does not keep ev, so the caller may reuse it.
func (p *pinger) write(ev []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.last = time.Now()
	if p.firstAt.IsZero() {
		p.firstAt = p.last
	}
	if _, p.err = p.dst.Write(ev); p.err != nil {
		return p.err
	}
	p.flush()
	return nil
}

// tick pings if the stream has been quiet for the interval, and sets itself
// for when the next interval would end. Moving the timer here, rather than
// on every write, keeps it off the per-token path.
func (p *pinger) tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.err != nil {
		return
	}
	quiet := time.Since(p.last)
	if quiet >= p.every {
		if _, p.err = p.dst.Write(pingEvent); p.err != nil {
			return
		}
		p.flush()
		p.last, quiet = time.Now(), 0
	}
	p.timer.Reset(p.every - quiet)
}

func (p *pinger) failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err != nil
}

// stop ends the pings and returns the first failed write. Nothing is written
// once it returns.
func (p *pinger) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.timer.Stop()
	return p.err
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
	// openIndex is the content block currently open, or -1. Blocks open and
	// close in sequence, so a fragment for another block closes this one first.
	openIndex int
	openTool  int // upstream tool_call index behind the open block, if any
	isTool    bool

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
	if s.openIndex < 0 || s.isTool {
		if err := s.closeBlock(); err != nil {
			return err
		}
		if err := s.openText(); err != nil {
			return err
		}
	}
	return s.delta("text_delta", "text", text)
}

// toolCall emits a fragment of a tool call, opening its block first if needed.
func (s *messagesStream) toolCall(index int, tc oaiToolCallDelta) error {
	if err := s.start(); err != nil {
		return err
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
	b = strconv.AppendInt(b, int64(s.openIndex), 10)
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
	buf := make([]byte, 0, len(name)+len(data)+20)
	buf = append(buf, "event: "...)
	buf = append(buf, name...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, '\n', '\n')
	return s.send(buf)
}
