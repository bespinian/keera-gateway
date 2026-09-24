package gateway

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
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
)

func TestPipeSSEDropsTheUsageChunkTheGatewayAskedFor(t *testing.T) {
	// The client never sent stream_options, so it is not expecting a chunk with
	// an empty choices array. Some clients treat one as a malformed response.
	var out bytes.Buffer
	in := sseStream(deltaChunk, deltaChunk, usageChunk)

	stats, err := pipeSSE(&out, func() {}, strings.NewReader(in), true)
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

	stats, err := pipeSSE(&out, func() {}, strings.NewReader(in), false)
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
	// With continuous_usage_stats a chunk has usage *and* choices. Dropping one
	// of those would swallow generated content.
	both := `{"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	var out bytes.Buffer
	if _, err := pipeSSE(&out, func() {}, strings.NewReader(sseStream(both)), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"delta"`) {
		t.Errorf("a chunk carrying content was dropped:\n%s", out.String())
	}
}

func TestPipeSSECountsDeltasForACancelledStream(t *testing.T) {
	// No usage chunk ever arrives, because the client hung up. The delta count
	// is what the request is charged on instead of nothing at all.
	in := "data: " + deltaChunk + "\n\ndata: " + deltaChunk + "\n\ndata: " + deltaChunk + "\n\n"
	stats, err := pipeSSE(io.Discard, func() {}, strings.NewReader(in), true)
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

func TestPipeSSEHandlesCRLF(t *testing.T) {
	in := "data: " + deltaChunk + "\r\n\r\ndata: " + usageChunk + "\r\n\r\ndata: [DONE]\r\n\r\n"
	var out bytes.Buffer
	stats, err := pipeSSE(&out, func() {}, strings.NewReader(in), true)
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
	if _, err := pipeSSE(&out, func() {}, strings.NewReader(in), true); err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if !strings.HasSuffix(out.String(), `"delta":`) {
		t.Errorf("the partial event was not forwarded:\n%q", out.String())
	}
}

func TestPipeSSEReportsAWriteFailure(t *testing.T) {
	// A client disconnecting mid-stream surfaces as a write error, and the
	// caller needs it to know the request was cut short.
	want := errors.New("client gone")
	_, err := pipeSSE(errWriter{want}, func() {}, strings.NewReader(sseStream(deltaChunk)), true)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func TestPipeSSERecordsWhenTheFirstEventReachedTheClient(t *testing.T) {
	stats, err := pipeSSE(io.Discard, func() {}, strings.NewReader(sseStream(deltaChunk)), true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.firstAt.IsZero() {
		t.Error("firstAt was not set; time-to-first-token would be unreportable")
	}
}

func TestPipeSSEFlushesEveryEvent(t *testing.T) {
	// Without a flush per event the whole stream arrives at once, which is the
	// difference between an editor that types and one that pauses then dumps.
	flushes := 0
	in := sseStream(deltaChunk, deltaChunk)
	if _, err := pipeSSE(io.Discard, func() { flushes++ }, strings.NewReader(in), true); err != nil {
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
	// A tool call with a big argument object must not be held back waiting for
	// a blank line that is arbitrarily far away.
	big := `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"` +
		strings.Repeat("a", maxEventBytes) + `"}}]}}]}`
	var out bytes.Buffer
	if _, err := pipeSSE(&out, func() {}, strings.NewReader("data: "+big+"\n\n"), true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("tool_calls")) {
		t.Error("a large tool-call event was not forwarded")
	}
}

func TestPipeSSEReadsAChunkLargerThanTheReadBuffer(t *testing.T) {
	// A line longer than one read is a real thing on this stream: a tool call
	// with a large argument object arrives as one. Half of that line read as a
	// whole one would be truncated JSON, and the gateway would silently fail
	// to find the token counts it bills the request on.
	chunk := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 3*readBuffer) +
		`"}}],"usage":{"prompt_tokens":40000,"completion_tokens":11,"total_tokens":40011}}`
	var out bytes.Buffer
	stats, err := pipeSSE(&out, func() {}, strings.NewReader(sseStream(chunk)), true)
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
	// And the client still receives the whole thing.
	if got, want := out.Len(), len(chunk); got < want {
		t.Errorf("forwarded %d bytes of a %d-byte chunk", got, want)
	}
	if !bytes.Contains(out.Bytes(), []byte(strings.Repeat("x", 3*readBuffer))) {
		t.Error("the content of an oversized chunk did not arrive intact")
	}
}

// The cached share of the prompt rides in the same usage chunk, one level
// down. It is the difference between a streamed request priced the way the
// provider prices it and one priced at up to ten times that, so a stream that
// carried the number and a gateway that did not read it is the failure worth
// a test of its own.
func TestPipeSSEReadsTheCachedShareOfThePrompt(t *testing.T) {
	const cachedUsageChunk = `{"id":"1","choices":[],"usage":{"prompt_tokens":100,` +
		`"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":90}}}`

	stats, err := pipeSSE(io.Discard, func() {},
		strings.NewReader(sseStream(deltaChunk, cachedUsageChunk)), true)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil {
		t.Fatal("the usage record was not captured")
	}
	// The prompt count is the whole prompt, and the cached count is a share of
	// it rather than something to add to it.
	if stats.usage.InputTokens != 100 || stats.usage.cached() != 90 {
		t.Errorf("usage = %d in, %d cached; want 100 and 90",
			stats.usage.InputTokens, stats.usage.cached())
	}
}

// An inference plane of your own caches nothing and sends no such object,
// which has to read as nothing cached rather than as a missing number.
func TestPipeSSEReadsNoCachedShareAsNoneCached(t *testing.T) {
	stats, err := pipeSSE(io.Discard, func() {},
		strings.NewReader(sseStream(deltaChunk, usageChunk)), true)
	if err != nil {
		t.Fatalf("pipeSSE: %v", err)
	}
	if stats.usage == nil || stats.usage.cached() != 0 {
		t.Errorf("cached = %d, want 0", stats.usage.cached())
	}
}

func TestHasUsageSkipsANullUsage(t *testing.T) {
	// With include_usage, OpenAI sends "usage":null in every chunk. Decoding
	// each of them would put a JSON decode on every token.
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
