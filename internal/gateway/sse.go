package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
)

var (
	dataPrefix = []byte("data:")
	doneMarker = []byte("[DONE]")
	usageKey   = []byte(`"usage"`)
	deltaKey   = []byte(`"delta"`)
	finishKey  = []byte(`"finish_reason"`)
	errorKey   = []byte(`"error"`)
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
	// InputDetails says how much of the prompt the provider served from its
	// cache, which is charged at a lower price, and how much it wrote to it,
	// which may be charged at a higher one. See policy.Model.Cost.
	//
	// Both are part of InputTokens, not added to them. A self-hosted plane
	// sends no such object, which reads as no cache at all.
	InputDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
		// CacheWriteHourTokens is the part of CacheWriteTokens kept for an
		// hour, which costs more. Only Anthropic's own API reports it.
		CacheWriteHourTokens int `json:"-"`
	} `json:"prompt_tokens_details"`
}

// cached is how much of the prompt the provider did not have to read again.
func (u *tokenUsage) cached() int {
	if u == nil {
		return 0
	}
	return u.InputDetails.CachedTokens
}

// tokens is the record in the terms prices are given in.
func (u *tokenUsage) tokens() policy.Tokens {
	if u == nil {
		return policy.Tokens{}
	}
	return policy.Tokens{
		Input:          u.InputTokens,
		CachedInput:    u.InputDetails.CachedTokens,
		CacheWrite:     u.InputDetails.CacheWriteTokens,
		CacheWriteHour: u.InputDetails.CacheWriteHourTokens,
		Output:         u.OutputTokens,
	}
}

// streamStats is what a piped stream tells the gateway about what it carried.
type streamStats struct {
	// usage is the upstream's record, or nil if the stream ended before one
	// arrived, as it does when the client disconnects.
	usage *tokenUsage
	// deltas counts chunks with generated content. vLLM sends one per token,
	// so this estimates the output when no usage record came.
	deltas int
	// firstAt is when the first token the model wrote reached the gateway's
	// client: text, reasoning or a tool call. Events that only open the
	// answer, such as one naming the role, come before any token and do not
	// count.
	firstAt time.Time
	// partial says usage was pieced together from a stream that ended early,
	// so the row is an estimate.
	partial bool
	// cut is why a stream that was read to its end is still not a whole
	// answer: the upstream sent an error, or stopped before it said why. A
	// passed-on stream is the client's to judge, so this is only recorded.
	cut error
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

// eachLine calls line with every line of a server-sent event stream, and end
// once the stream stops, so a last event cut off before its blank line still
// counts. The stream ending is not an error; a failed read is, after end.
func eachLine(src io.Reader, limit int64, line func([]byte) error, end func() error) error {
	lines := newSSELines(src, limit)
	for {
		l, readErr := lines.next()
		if len(l) > 0 {
			if err := line(l); err != nil {
				return err
			}
		}
		if readErr != nil {
			if err := end(); err != nil {
				return err
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// pipeSSE forwards a server-sent event stream, flushing every event as it goes,
// while watching for the usage record.
//
// dropUsageEvent removes the trailing usage-only chunk from what reaches the
// client. It is set when the gateway, not the client, asked for that chunk.
// alias, when set, is written as the model name of every chunk. limit bounds
// one line, so an upstream that never ends one cannot fill the gateway's memory.
func pipeSSE(dst io.Writer, flush func(), src io.Reader, alias string,
	dropUsageEvent bool, limit int64,
) (streamStats, error) {
	p := &ssePipe{dst: dst, flush: flush, dropUsage: dropUsageEvent}
	if alias != "" {
		p.name, _ = json.Marshal(alias)
	}
	err := eachLine(src, limit, p.add, p.emit)
	p.stats.cut = p.failure()
	return p.stats, err
}

// failure is why a stream read to its end is still not a whole answer, or
// nil, as chatChunks.failure decides for a translated one.
func (p *ssePipe) failure() error { return endedFailure(p.upstreamErr, p.finished) }

// ssePipe is the state of one piped stream: the event being gathered, and
// what has been learnt so far.
type ssePipe struct {
	dst   io.Writer
	flush func()
	// name is the alias, encoded once for every chunk it is written into.
	name      []byte
	dropUsage bool
	// renamed holds the chunk with the alias in it, reused for every chunk.
	renamed []byte

	stats     streamStats
	event     []byte
	usageOnly bool
	// generated says the event being gathered, or one before it, carries a
	// token. Once it is set, no chunk is decoded for it again.
	generated bool
	// finished says a chunk gave a finish reason, which every backend sends
	// at the end of a whole answer. upstreamErr is an error sent in place of
	// a chunk.
	finished    bool
	upstreamErr string
}

// add takes one line into the current event, and forwards the event once it
// is complete or too large to hold.
func (p *ssePipe) add(line []byte) error {
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
			p.generated = p.generated || generated(payload)
			p.finished = p.finished || hasValue(payload, finishKey)
			if hasValue(payload, errorKey) {
				p.upstreamErr = upstreamText(payload)
			}
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
		if p.generated && p.stats.firstAt.IsZero() {
			p.stats.firstAt = time.Now()
		}
		out := p.event
		if p.name != nil {
			if start, end, ok := modelValue(out); ok {
				p.renamed = appendRenamed(p.renamed[:0], out, p.name, start, end)
				out = p.renamed
			}
		}
		if _, err := p.dst.Write(out); err != nil {
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

// generated reports whether a chat or text completion chunk carries a token
// the model wrote. Reasoning counts: the model has started, even if the
// client shows only the answer. vLLM and llama.cpp name it reasoning_content,
// others reasoning.
func generated(payload []byte) bool {
	var chunk struct {
		Choices []struct {
			Text  string `json:"text"`
			Delta struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				Reasoning        string            `json:"reasoning"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	// A field of an unexpected type fails the decode but leaves the rest
	// read, so the error is not a verdict.
	_ = json.Unmarshal(payload, &chunk)
	for _, c := range chunk.Choices {
		d := c.Delta
		if c.Text != "" || d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" ||
			len(d.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// hasUsage reports whether payload has a usage key with a value other than
// null. With include_usage, OpenAI sends "usage":null in every chunk, and
// only the last one needs decoding.
func hasUsage(payload []byte) bool { return hasValue(payload, usageKey) }

// hasValue reports whether payload has key with a value other than null,
// without decoding it. Inside a string a quote is escaped, so the quoted key
// followed by a colon is only ever a key.
func hasValue(payload, key []byte) bool {
	_, after, ok := bytes.Cut(payload, key)
	if !ok {
		return false
	}
	rest := bytes.TrimLeft(after, " \t\r\n")
	if !bytes.HasPrefix(rest, []byte(":")) {
		return false
	}
	rest = bytes.TrimLeft(rest[1:], " \t\r\n")
	return !bytes.HasPrefix(rest, []byte("null"))
}

// flushBeforeRead holds back a stream's flushes until the gateway goes back to
// the upstream for more.
//
// A flush is a system call, and with one per token it was most of the CPU a
// stream cost. The pipes read through a bufio.Reader, which reads from here
// only once the events it holds are used up. So when one read brought several
// events, they go out in one write; when it brought one, it goes out at once,
// as before. A busy gateway falls behind, reads more per call and so flushes
// less, which is when it matters.
//
// A ping is written by a timer while the read waits, so the stream's writes
// and flushes all go through here under one lock, and a flush asked for
// during that wait happens at once.
type flushBeforeRead struct {
	src     io.Reader
	dst     io.Writer
	flusher *http.ResponseController

	mu      sync.Mutex
	pending bool
	waiting bool
}

func (f *flushBeforeRead) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dst.Write(p)
}

func (f *flushBeforeRead) Read(p []byte) (int, error) {
	f.mu.Lock()
	f.flush()
	f.waiting = true
	f.mu.Unlock()
	n, err := f.src.Read(p)
	f.mu.Lock()
	f.waiting = false
	f.mu.Unlock()
	return n, err
}

// later is the flush the pipes call after each event.
func (f *flushBeforeRead) later() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = true
	if f.waiting {
		f.flush()
	}
}

// now flushes what the pipes have written, if anything.
func (f *flushBeforeRead) now() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flush()
}

func (f *flushBeforeRead) flush() {
	if f.pending {
		_ = f.flusher.Flush()
		f.pending = false
	}
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

// scanSSE walks the data payloads of a server-sent event stream, joining an
// event's data lines the way the format says to and skipping the terminator.
// limit bounds one event's payload.
func scanSSE(src io.Reader, limit int64, fn func(payload []byte) error) error {
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

	return eachLine(src, limit, func(line []byte) error {
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			return complete()
		case bytes.HasPrefix(trimmed, dataPrefix):
			chunk := bytes.TrimSpace(trimmed[len(dataPrefix):])
			if int64(len(data)+len(chunk)) > limit {
				return errEventTooLarge
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
		return nil
	}, complete)
}

// oaiStreamChunk is one chunk of a chat completion stream.
type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string             `json:"content"`
			ReasoningContent string             `json:"reasoning_content"`
			Reasoning        string             `json:"reasoning"`
			ToolCalls        []oaiToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *tokenUsage `json:"usage"`
	// Error is what vLLM sends in place of a chunk when generation fails.
	Error json.RawMessage `json:"error"`
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

// chatChunks is what a stream translated into another API learns from a chat
// completion stream besides its content. Both the Messages and the Responses
// streams embed it.
type chatChunks struct {
	finishReason string
	usage        *tokenUsage
	// deltas counts the chunks the model wrote into, reasoning included,
	// as the chat pipe does: a client that hangs up is charged on it.
	deltas int
	// firstAt is when the first token arrived. A translation drops reasoning,
	// so this can be earlier than the first event the client is sent.
	firstAt time.Time
	// upstreamErr is the message of an error the upstream sent mid-stream.
	upstreamErr string
}

// fold reads one chunk: it keeps the usage record and the finish reason, and
// hands the text and each tool-call fragment on, in that order.
func (c *chatChunks) fold(payload []byte, text func(string) error,
	tool func(index int, tc oaiToolCallDelta) error,
) error {
	var chunk oaiStreamChunk
	if json.Unmarshal(payload, &chunk) != nil {
		// An unreadable chunk is skipped: the rest of the turn is still worth
		// delivering.
		return nil
	}
	if chunk.Usage != nil {
		c.usage = chunk.Usage
	}
	if len(chunk.Error) > 0 && !bytes.Equal(chunk.Error, []byte("null")) {
		c.upstreamErr = upstreamText(payload)
		return nil
	}
	if len(chunk.Choices) == 0 {
		// The usage-only chunk. Its numbers go into the closing event.
		return nil
	}
	choice := chunk.Choices[0]
	if d := choice.Delta; d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" ||
		len(d.ToolCalls) > 0 {
		if c.firstAt.IsZero() {
			c.firstAt = time.Now()
		}
		c.deltas++
	}
	if choice.FinishReason != "" {
		c.finishReason = choice.FinishReason
	}
	if t := choice.Delta.Content; t != "" {
		if err := text(t); err != nil {
			return err
		}
	}
	for _, tc := range choice.Delta.ToolCalls {
		index := 0
		if tc.Index != nil {
			index = *tc.Index
		}
		if err := tool(index, tc); err != nil {
			return err
		}
	}
	return nil
}
