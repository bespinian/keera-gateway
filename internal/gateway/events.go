package gateway

import (
	"errors"
	"io"
	"sync"
	"time"
)

// The Messages and Responses surfaces both turn a chat completion stream into
// events where blocks or output items open and close around the deltas. This
// file holds what they share.

// eventStream re-emits a chat completion stream in another API's events.
type eventStream interface {
	chunk(payload []byte) error
	// finish closes the stream. cut is why the upstream ended early, or nil.
	finish(cut error) error
	// failure is why a fully read stream is still not a whole answer, or nil.
	failure() error
	counts() (usage *tokenUsage, deltas int, firstAt time.Time)
}

// pipeEvents forwards a chat completion stream as the events newStream makes.
// A ping is sent whenever the upstream is quiet for every; every <= 0 sends
// none. Pings come from a timer, since they are needed exactly when the
// reading goroutine is waiting.
func pipeEvents(dst io.Writer, flush func(), src io.Reader, limit int64, every time.Duration,
	newStream func(send func([]byte) error) eventStream,
) (streamStats, error) {
	out := newPinger(dst, flush, every)
	st := newStream(out.write)

	readErr := scanSSE(src, limit, st.chunk)
	if readErr == nil {
		readErr = st.failure()
	}
	// Close the stream even after an upstream failure, or the client hangs.
	// A client that is gone gets nothing more.
	if !out.failed() {
		if err := st.finish(readErr); err != nil && readErr == nil {
			readErr = err
		}
	}
	writeErr := out.stop()

	var stats streamStats
	stats.usage, stats.deltas, stats.firstAt = st.counts()
	if writeErr != nil {
		return stats, writeErr
	}
	return stats, readErr
}

func (c *chatChunks) counts() (*tokenUsage, int, time.Time) {
	return c.usage, c.deltas, c.firstAt
}

// streamFailure is an upstream stream that ended without a whole answer. Its
// message is safe to show the client.
type streamFailure struct{ msg string }

func (f streamFailure) Error() string { return f.msg }

// cutMessage is what the client is told of a stream cut short.
func cutMessage(cut error) string {
	if f, ok := errors.AsType[streamFailure](cut); ok {
		return f.msg
	}
	return "the connection to the model broke before the answer was finished"
}

// failure reports an error the upstream sent, or a stream that stopped without
// a finish reason, which every backend sends at the end of a whole answer.
func (c *chatChunks) failure() error { return endedFailure(c.upstreamErr, c.finishReason != "") }

func endedFailure(upstreamErr string, ended bool) error {
	switch {
	case upstreamErr != "":
		return streamFailure{"the model failed while answering: " + upstreamErr}
	case !ended:
		return streamFailure{"the model's answer stopped before it was finished"}
	}
	return nil
}

func frame(name string, data []byte) []byte {
	buf := make([]byte, 0, len(name)+len(data)+16)
	buf = append(buf, "event: "...)
	buf = append(buf, name...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	return append(buf, '\n', '\n')
}

var pingEvent = frame("ping", []byte(`{"type":"ping"}`))

// blocks tracks the one open block of a stream. Blocks open and close in
// sequence, so a fragment of another kind or tool call closes the open one.
type blocks struct {
	// open is the index of the open block, or -1.
	open   int
	isTool bool
	// tool is the upstream tool_call index behind an open tool block.
	tool int
}

// enter makes sure the open block is the one a fragment belongs to: text, or
// the tool call at index. If not, closeOpen closes it and open opens the next.
func (b *blocks) enter(isTool bool, index int, closeOpen, open func() error) error {
	if b.open >= 0 && b.isTool == isTool && (!isTool || b.tool == index) {
		return nil
	}
	if err := closeOpen(); err != nil {
		return err
	}
	return open()
}

// pinger writes a stream's events, and a ping whenever nothing was written for
// a whole interval.
type pinger struct {
	dst   io.Writer
	flush func()
	every time.Duration

	mu    sync.Mutex
	timer *time.Timer
	last  time.Time
	// err is the first failed write. Nothing is written after it.
	err     error
	stopped bool
}

func newPinger(dst io.Writer, flush func(), every time.Duration) *pinger {
	p := &pinger{dst: dst, flush: flush, every: every, last: time.Now()}
	if every <= 0 {
		return p
	}
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
	if _, p.err = p.dst.Write(ev); p.err != nil {
		return p.err
	}
	p.flush()
	return nil
}

// tick pings if the stream was quiet for the interval and resets the timer.
// Resetting here, not on every write, keeps it off the per-token path.
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

// stop ends the pings and returns the first failed write.
func (p *pinger) stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
	}
	return p.err
}
