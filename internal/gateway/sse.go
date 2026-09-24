package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

var (
	dataPrefix = []byte("data:")
	doneMarker = []byte("[DONE]")
	usageKey   = []byte(`"usage"`)
	deltaKey   = []byte(`"delta"`)
)

// maxEventBytes bounds how much of one server-sent event is held before it is
// forwarded without waiting for its blank line. Anything this large cannot be
// the small usage-only chunk, so there is no reason to hold it.
const maxEventBytes = 1 << 20

// tokenUsage is the usage record of streamed and buffered responses. The tags
// are the OpenAI field names; everywhere else Keera says input and output.
type tokenUsage struct {
	InputTokens  int `json:"prompt_tokens"`
	OutputTokens int `json:"completion_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// InputDetails says how much of the prompt the provider served from its
	// cache, which is charged at a lower price. See policy.Model.Cost.
	//
	// Cached tokens are part of InputTokens, not added to them. A self-hosted
	// plane sends no such object, which reads as nothing cached.
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// cached is how much of the prompt the provider did not have to read again.
func (u *tokenUsage) cached() int {
	if u == nil {
		return 0
	}
	return u.InputDetails.CachedTokens
}

// streamStats is what a piped stream tells the gateway about what it carried.
type streamStats struct {
	// usage is the upstream's record, or nil if the stream ended before one
	// arrived, as it does when the client disconnects.
	usage *tokenUsage
	// deltas counts chunks with generated content. vLLM sends one per token,
	// so this estimates the output when no usage record came.
	deltas int
	// firstAt is when the first event reached the client: the wait a
	// developer actually feels.
	firstAt time.Time
	// partial says usage was pieced together from a stream that ended early,
	// so the row is an estimate.
	partial bool
}

// sseLines reads a server-sent event stream one line at a time.
//
// A token stream is one line per token, and ReadBytes would allocate for each.
// ReadSlice returns a view into the reader's buffer instead. This type adds
// what a view cannot do: hold a line longer than the buffer (such as a large
// tool call) until its newline arrives, so it is never read as two lines.
type sseLines struct {
	rd *bufio.Reader
	// part holds a line that spanned more than one read, until it is complete.
	part []byte
	// limit bounds part, so an upstream that never sends a newline cannot
	// fill the gateway's memory.
	limit int64
}

func newSSELines(src io.Reader, limit int64) *sseLines {
	return &sseLines{rd: bufio.NewReaderSize(src, readBuffer), limit: limit}
}

// maxLineBytes bounds one line of a stream whose caller sets no limit of its
// own. It matches the default cap on a buffered response.
const maxLineBytes = 64 << 20

// readBuffer is what one read holds. Any event smaller than this, which is
// every token of a completion, is read without copying.
const readBuffer = 32 << 10

// next returns the next line, terminator included, together with the error
// that ended the read. The slice it returns is only valid until the following
// call, exactly as ReadSlice's is.
func (l *sseLines) next() ([]byte, error) {
	for {
		frag, err := l.rd.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if int64(len(l.part)+len(frag)) > l.limit {
				l.part = l.part[:0]
				return nil, errEventTooLarge
			}
			l.part = append(l.part, frag...)
			continue
		}
		if len(l.part) == 0 {
			return frag, err
		}
		l.part = append(l.part, frag...)
		line := l.part
		l.part = l.part[:0]
		return line, err
	}
}

// pipeSSE forwards a server-sent event stream, flushing every event as it goes,
// while watching for the usage record.
//
// dropUsageEvent removes the trailing usage-only chunk from what reaches the
// client. It is set when the gateway, not the client, asked for that chunk.
func pipeSSE(dst io.Writer, flush func(), src io.Reader, dropUsageEvent bool) (streamStats, error) {
	p := &ssePipe{dst: dst, flush: flush, dropUsage: dropUsageEvent}
	lines := newSSELines(src, maxLineBytes)
	for {
		line, readErr := lines.next()
		if err := p.add(line); err != nil {
			return p.stats, err
		}
		if readErr != nil {
			// Forward whatever a truncated final event held, then report.
			if err := p.emit(); err != nil {
				return p.stats, err
			}
			if errors.Is(readErr, io.EOF) {
				return p.stats, nil
			}
			return p.stats, readErr
		}
	}
}

// ssePipe is the state of one piped stream: the event being gathered, and
// what has been learnt so far.
type ssePipe struct {
	dst       io.Writer
	flush     func()
	dropUsage bool

	stats     streamStats
	event     []byte
	usageOnly bool
}

// add takes one line into the current event, and forwards the event once it
// is complete or too large to hold.
func (p *ssePipe) add(line []byte) error {
	if len(line) == 0 {
		return nil
	}
	p.event = append(p.event, line...)
	trimmed := bytes.TrimRight(line, "\r\n")
	switch {
	case len(trimmed) == 0:
		// A blank line: the event is complete.
		return p.emit()
	case bytes.HasPrefix(trimmed, dataPrefix):
		payload := bytes.TrimSpace(trimmed[len(dataPrefix):])
		if !bytes.Equal(payload, doneMarker) {
			inspect(payload, &p.stats, &p.usageOnly)
		}
	}
	if len(p.event) > maxEventBytes {
		return p.emit()
	}
	return nil
}

// emit forwards the gathered event, unless it is the usage-only chunk the
// gateway asked for itself.
func (p *ssePipe) emit() error {
	if len(p.event) == 0 {
		return nil
	}
	if !p.usageOnly || !p.dropUsage {
		if p.stats.firstAt.IsZero() {
			p.stats.firstAt = time.Now()
		}
		if _, err := p.dst.Write(p.event); err != nil {
			return err
		}
		p.flush()
	}
	p.event = p.event[:0]
	p.usageOnly = false
	return nil
}

// inspect pulls what the gateway needs out of one chunk. A cheap byte search
// comes first, so ordinary content chunks never reach the JSON decoder.
func inspect(payload []byte, stats *streamStats, usageOnly *bool) {
	if bytes.Contains(payload, deltaKey) {
		stats.deltas++
	}
	if !hasUsage(payload) {
		return
	}
	var chunk struct {
		Usage   *tokenUsage       `json:"usage"`
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil {
		return
	}
	stats.usage = chunk.Usage
	*usageOnly = len(chunk.Choices) == 0
}

// hasUsage reports whether payload has a usage key with a value other than
// null. With include_usage, OpenAI sends "usage":null in every chunk, and
// only the last one needs decoding.
func hasUsage(payload []byte) bool {
	_, after, ok := bytes.Cut(payload, usageKey)
	if !ok {
		return false
	}
	rest := bytes.TrimLeft(after, " \t\r\n")
	rest = bytes.TrimLeft(bytes.TrimPrefix(rest, []byte(":")), " \t\r\n")
	return !bytes.HasPrefix(rest, []byte("null"))
}

// usageFromResponse pulls the usage record out of a complete, non-streamed
// response body.
func usageFromResponse(raw []byte) *tokenUsage {
	if !bytes.Contains(raw, usageKey) {
		return nil
	}
	var envelope struct {
		Usage *tokenUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	return envelope.Usage
}
