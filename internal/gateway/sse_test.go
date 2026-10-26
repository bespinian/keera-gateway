package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// sseStream builds a stream the way vLLM emits one.
func sseStream(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

const (
	deltaChunk = `{"id":"1","choices":[{"index":0,"delta":{"content":"x"}}]}`
	usageChunk = `{"id":"1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107}}`
	// stopChunk ends a whole answer. A translated stream without one failed.
	stopChunk = `{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
)

func TestPipeSSEDropsTheUsageChunkTheGatewayAskedFor(t *testing.T) {
	// The client never sent stream_options, so it does not expect a chunk
	// with empty choices. Some clients treat one as malformed.
	var out bytes.Buffer
	in := sseStream(deltaChunk, deltaChunk, usageChunk)

	stats, err := pipeSSE(&out, func() {}, strings.NewReader(in), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil {
		t.Fatal("the usage record was not captured; the request would be billed as free")
	}
	if stats.usage.InputTokens != 100 || stats.usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want 100/7", *stats.usage)
	}
	if strings.Contains(out.String(), `"usage"`) {
		t.Errorf("the injected usage chunk reached the client:\n%s", out.String())
	}
	if !strings.HasSuffix(out.String(), "data: [DONE]\n\n") {
		t.Errorf("the stream must still be terminated:\n%q", out.String())
	}
	if got := strings.Count(out.String(), "data: "); got != 3 {
		t.Errorf("forwarded %d data lines, want 2 deltas plus [DONE]", got)
	}
}

func TestPipeSSEKeepsTheUsageChunkTheClientAskedFor(t *testing.T) {
	var out bytes.Buffer
	in := sseStream(deltaChunk, usageChunk)

	stats, err := pipeSSE(&out, func() {}, strings.NewReader(in), "", false, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil {
		t.Fatal("usage was not captured")
	}
	if !strings.Contains(out.String(), `"usage"`) {
		t.Error("a client that set include_usage itself must still receive the chunk")
	}
}

func TestPipeSSEKeepsAChunkThatCarriesBothContentAndUsage(t *testing.T) {
	// With continuous_usage_stats a chunk has usage and choices. Dropping it
	// would lose content.
	both := `{"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	var out bytes.Buffer
	if _, err := pipeSSE(&out, func() {}, strings.NewReader(sseStream(both)), "", true, DefaultMaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"delta"`) {
		t.Errorf("a chunk carrying content was dropped:\n%s", out.String())
	}
}

func TestPipeSSECountsDeltasForACancelledStream(t *testing.T) {
	// The client hung up, so no usage chunk arrives. The request is charged
	// on the delta count instead.
	in := "data: " + deltaChunk + "\n\ndata: " + deltaChunk + "\n\ndata: " + deltaChunk + "\n\n"
	stats, err := pipeSSE(io.Discard, func() {}, strings.NewReader(in), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage != nil {
		t.Error("there was no usage record to find")
	}
	if stats.deltas != 3 {
		t.Errorf("deltas = %d, want 3", stats.deltas)
	}
}

// The row says when a passed-on stream was not a whole answer.
func TestPipeSSESaysWhenAStreamWasNoWholeAnswer(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{sseStream(deltaChunk, stopChunk, usageChunk), ""},
		{sseStream(deltaChunk), "stopped before"},
		{sseStream(deltaChunk, `{"error":{"message":"out of memory"}}`), "out of memory"},
		// A quoted key inside the text is not the key.
		{sseStream(`{"choices":[{"delta":{"content":"\"error\": x"}}]}`, stopChunk), ""},
	} {
		stats, err := pipeSSE(io.Discard, func() {}, strings.NewReader(tc.in), "", true,
			DefaultMaxResponseBytes)
		if err != nil {
			t.Fatalf("pipeSSE: %v", err)
		}
		switch {
		case tc.want == "" && stats.cut != nil:
			t.Errorf("%q: cut = %v, want a whole answer", tc.in, stats.cut)
		case tc.want != "" && (stats.cut == nil || !strings.Contains(stats.cut.Error(), tc.want)):
			t.Errorf("%q: cut = %v, want %q", tc.in, stats.cut, tc.want)
		}
	}
}

func TestPipeSSEHandlesCRLF(t *testing.T) {
	in := "data: " + deltaChunk + "\r\n\r\ndata: " + usageChunk + "\r\n\r\ndata: [DONE]\r\n\r\n"
	var out bytes.Buffer
	stats, err := pipeSSE(&out, func() {}, strings.NewReader(in), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil {
		t.Fatal("usage was not captured from a CRLF stream")
	}
	if strings.Contains(out.String(), `"usage"`) {
		t.Error("the injected usage chunk was not dropped from a CRLF stream")
	}
}

func TestPipeSSEForwardsATruncatedFinalEvent(t *testing.T) {
	// The upstream died mid-event. Whatever arrived is still the client's.
	in := "data: " + deltaChunk + "\n\ndata: {\"choices\":[{\"delta\":"
	var out bytes.Buffer
	if _, err := pipeSSE(&out, func() {}, strings.NewReader(in), "", true, DefaultMaxResponseBytes); err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if !strings.HasSuffix(out.String(), `"delta":`) {
		t.Errorf("the partial event was not forwarded:\n%q", out.String())
	}
}

func TestPipeSSEReportsAWriteFailure(t *testing.T) {
	// A client leaving mid-stream shows as a write error, which tells the
	// caller the request was cut short.
	want := errors.New("client gone")
	_, err := pipeSSE(errWriter{want}, func() {}, strings.NewReader(sseStream(deltaChunk)), "", true, DefaultMaxResponseBytes)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func TestPipeSSERecordsWhenTheFirstTokenReachedTheClient(t *testing.T) {
	stats, err := pipeSSE(io.Discard, func() {}, strings.NewReader(sseStream(deltaChunk)), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if stats.firstAt.IsZero() {
		t.Error("firstAt was not set; time-to-first-token would be unreportable")
	}
}

// afterPause reads r only once pause has passed.
type afterPause struct {
	r      io.Reader
	pause  time.Duration
	paused bool
}

func (a *afterPause) Read(p []byte) (int, error) {
	if !a.paused {
		time.Sleep(a.pause)
		a.paused = true
	}
	return a.r.Read(p)
}

// paused reads the first part at once, and each after it once pause has
// passed.
func paused(pause time.Duration, parts ...string) io.Reader {
	rs := []io.Reader{strings.NewReader(parts[0])}
	for _, p := range parts[1:] {
		rs = append(rs, &afterPause{r: strings.NewReader(p), pause: pause})
	}
	return io.MultiReader(rs...)
}

const pause = 50 * time.Millisecond

func TestPipeSSEDoesNotCountTheOpeningAsTheFirstToken(t *testing.T) {
	// A role chunk and a keep-alive can come long before the first token, so
	// they do not count.
	opening := ": keep-alive\n\n" +
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"
	start := time.Now()
	stats, err := pipeSSE(io.Discard, func() {}, paused(pause, opening, sseStream(deltaChunk)),
		"", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.firstAt.Sub(start); got < pause {
		t.Errorf("first token after %v, want at least %v: the opening was counted", got, pause)
	}
}

func TestPipeSSEWithoutTokensHasNoFirstToken(t *testing.T) {
	in := sseStream(`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	stats, err := pipeSSE(io.Discard, func() {}, strings.NewReader(in), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.firstAt.IsZero() {
		t.Error("a stream with no token has a first token")
	}
}

func TestGeneratedTellsTokensFromTheOpeningAndTheEnd(t *testing.T) {
	for _, tc := range []struct {
		chunk string
		want  bool
	}{
		{`{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{`{"choices":[{"delta":{"content":null}}]}`, false},
		{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		{`{"choices":[],"usage":{"prompt_tokens":1}}`, false},
		{`{"choices":[{"delta":{"content":"hi"}}]}`, true},
		{`{"choices":[{"delta":{"reasoning_content":"hm"}}]}`, true},
		{`{"choices":[{"delta":{"reasoning":"hm"}}]}`, true},
		{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1"}]}}]}`, true},
		{`{"choices":[{"text":"hi"}]}`, true},
		// A provider's own extension of another type does not hide the token.
		{`{"choices":[{"delta":{"content":"hi","refusal":{}}}],"usage":"?"}`, true},
	} {
		if got := generated([]byte(tc.chunk)); got != tc.want {
			t.Errorf("generated(%s) = %v, want %v", tc.chunk, got, tc.want)
		}
	}
}

func TestPipeSSEFlushesEveryEvent(t *testing.T) {
	// Without a flush per event the whole stream arrives at once.
	flushes := 0
	in := sseStream(deltaChunk, deltaChunk)
	if _, err := pipeSSE(io.Discard, func() { flushes++ }, strings.NewReader(in), "", true, DefaultMaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	if flushes != 3 {
		t.Errorf("flushed %d times, want one per forwarded event (2 deltas plus [DONE])", flushes)
	}
}

func TestUsageFromResponse(t *testing.T) {
	body := `{"id":"1","choices":[{"message":{"content":"hi"}}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`
	u := usageFromResponse([]byte(body))
	if u == nil {
		t.Fatal("usage was not found in a non-streamed response")
	}
	if u.InputTokens != 12 || u.OutputTokens != 3 {
		t.Errorf("usage = %+v, want 12/3", *u)
	}
	if usageFromResponse([]byte(`{"choices":[]}`)) != nil {
		t.Error("a response with no usage must report none")
	}
	if usageFromResponse([]byte(`not json`)) != nil {
		t.Error("an unparseable response must report none rather than panic")
	}
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

var _ io.Writer = errWriter{}

func TestPipeSSEEmitsLargeEventsWithoutWaiting(t *testing.T) {
	// A big tool call must not be held back waiting for a far blank line.
	big := `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"` +
		strings.Repeat("a", maxEventBytes) + `"}}]}}]}`
	var out bytes.Buffer
	if _, err := pipeSSE(&out, func() {}, strings.NewReader("data: "+big+"\n\n"), "", true, DefaultMaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("tool_calls")) {
		t.Error("a large tool-call event was not forwarded")
	}
}

func TestPipeSSEReadsAChunkLargerThanTheReadBuffer(t *testing.T) {
	// A large tool call arrives as one long line. Read as two, it would be
	// broken JSON and its usage would be lost.
	chunk := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 3*readBuffer) +
		`"}}],"usage":{"prompt_tokens":40000,"completion_tokens":11,"total_tokens":40011}}`
	var out bytes.Buffer
	stats, err := pipeSSE(&out, func() {}, strings.NewReader(sseStream(chunk)), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if stats.usage == nil {
		t.Fatal("the usage record in an oversized chunk was not read")
	}
	if stats.usage.OutputTokens != 11 {
		t.Errorf("output tokens = %d, want 11", stats.usage.OutputTokens)
	}
	if stats.deltas != 1 {
		t.Errorf("deltas = %d, want 1: an oversized line is one chunk, not several",
			stats.deltas)
	}
	if got, want := out.Len(), len(chunk); got < want {
		t.Errorf("forwarded %d bytes of a %d-byte chunk", got, want)
	}
	if !bytes.Contains(out.Bytes(), []byte(strings.Repeat("x", 3*readBuffer))) {
		t.Error("the content of an oversized chunk did not arrive intact")
	}
}

// The cached share of the prompt is nested in the usage chunk. Missing it
// would overprice a streamed request up to tenfold.
func TestPipeSSEReadsTheCachedShareOfThePrompt(t *testing.T) {
	const cachedUsageChunk = `{"id":"1","choices":[],"usage":{"prompt_tokens":100,` +
		`"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":90}}}`

	stats, err := pipeSSE(io.Discard, func() {},
		strings.NewReader(sseStream(deltaChunk, cachedUsageChunk)), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil {
		t.Fatal("the usage record was not captured")
	}
	// The prompt count is the whole prompt; the cached count is part of it.
	if stats.usage.InputTokens != 100 || stats.usage.cached() != 90 {
		t.Errorf("usage = %d in, %d cached; want 100 and 90",
			stats.usage.InputTokens, stats.usage.cached())
	}
}

// A self-hosted plane sends no cache object, which reads as nothing cached.
func TestPipeSSEReadsNoCachedShareAsNoneCached(t *testing.T) {
	stats, err := pipeSSE(io.Discard, func() {},
		strings.NewReader(sseStream(deltaChunk, usageChunk)), "", true, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil || stats.usage.cached() != 0 {
		t.Errorf("cached = %d, want 0", stats.usage.cached())
	}
}

func TestHasUsageSkipsANullUsage(t *testing.T) {
	// With include_usage, OpenAI sends "usage":null in every chunk, which
	// must not be decoded on every token.
	for payload, want := range map[string]bool{
		deltaChunk: false,
		usageChunk: true,
		`{"choices":[{"delta":{"content":"x"}}],"usage":null}`:   false,
		`{"choices":[{"delta":{"content":"x"}}],"usage" : null}`: false,
		`{"choices":[],"usage": {"prompt_tokens":1}}`:            true,
	} {
		if got := hasUsage([]byte(payload)); got != want {
			t.Errorf("hasUsage(%s) = %v, want %v", payload, got, want)
		}
	}
}

func TestPipeSSEStopsALineThatNeverEnds(t *testing.T) {
	// An upstream that sends no newline must not fill the gateway's memory.
	lines := newSSELines(strings.NewReader(strings.Repeat("x", 4*readBuffer)), 2*readBuffer)
	if _, err := lines.next(); !errors.Is(err, errEventTooLarge) {
		t.Errorf("next = %v, want errEventTooLarge", err)
	}
}

// KEERA_MAX_RESPONSE_BYTES also caps one event of every streamed shape.
func TestStreamsHonourTheResponseLimit(t *testing.T) {
	long := sseStream(`{"choices":[{"delta":{"content":"` + strings.Repeat("x", 4*readBuffer) + `"}}]}`)
	for name, sh := range map[string]shape{
		"openai": openAIShape{}, "messages": anthropicShape{}, "responses": responsesShape{},
	} {
		_, err := sh.pipe(io.Discard, func() {}, strings.NewReader(long), "keera-code", true, 2*readBuffer)
		if !errors.Is(err, errEventTooLarge) {
			t.Errorf("%s: pipe = %v, want errEventTooLarge", name, err)
		}
	}
}

type countingFlusher struct {
	http.ResponseWriter
	flushes int
}

func (c *countingFlusher) Flush() { c.flushes++ }

func pipeFlushes(t *testing.T, src io.Reader) int {
	t.Helper()
	w := &countingFlusher{ResponseWriter: httptest.NewRecorder()}
	f := &flushBeforeRead{src: src, dst: w, flusher: http.NewResponseController(w)}
	if _, err := pipeSSE(f, f.later, f, "", true, DefaultMaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	f.now()
	return w.flushes
}

func TestFlushBeforeReadJoinsEventsThatArriveTogether(t *testing.T) {
	in := sseStream(deltaChunk, deltaChunk, deltaChunk)
	if n := pipeFlushes(t, strings.NewReader(in)); n != 1 {
		t.Errorf("flushed %d times, want 1 for events that came in one read", n)
	}
}

func TestFlushBeforeReadFlushesBeforeWaiting(t *testing.T) {
	// One byte per read: each event must reach the client before the next
	// read, which could wait.
	in := sseStream(deltaChunk, deltaChunk)
	if n := pipeFlushes(t, iotest.OneByteReader(strings.NewReader(in))); n != 3 {
		t.Errorf("flushed %d times, want 3: one per event when each arrives alone", n)
	}
}

func TestAPingReachesTheClientWhileTheUpstreamWaits(t *testing.T) {
	// The ping is written while the read waits on the upstream, and must be
	// flushed without waiting for it.
	restore := anthropicPingInterval
	anthropicPingInterval = time.Millisecond
	t.Cleanup(func() { anthropicPingInterval = restore })

	w := &signalFlusher{ResponseWriter: httptest.NewRecorder(), flushed: make(chan struct{})}
	stalled := stallUntil(w.flushed)
	f := &flushBeforeRead{
		src:     io.MultiReader(stalled, strings.NewReader(sseStream(deltaChunk, stopChunk))),
		dst:     w,
		flusher: http.NewResponseController(w),
	}
	if _, err := (anthropicShape{}).pipe(f, f.later, f, "keera-code", true, DefaultMaxResponseBytes); err != nil {
		t.Fatal(err)
	}
}

// signalFlusher closes flushed on its first flush.
type signalFlusher struct {
	http.ResponseWriter
	once    sync.Once
	flushed chan struct{}
}

func (s *signalFlusher) Flush() { s.once.Do(func() { close(s.flushed) }) }

// stallUntil is an upstream that says nothing until done is closed.
type stallUntil chan struct{}

func (s stallUntil) Read([]byte) (int, error) {
	select {
	case <-s:
		return 0, io.EOF
	case <-time.After(2 * time.Second):
		return 0, errors.New("nothing was flushed while the upstream was quiet")
	}
}
