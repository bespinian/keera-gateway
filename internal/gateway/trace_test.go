package gateway

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// stepNames is the steps of one request in the order they were recorded, which
// is what most of these assertions are actually about: the chart is read as a
// sequence, and a sequence that is out of order is worse than none.
func stepNames(spans []store.Span) []string {
	out := make([]string, 0, len(spans))
	for _, sp := range spans {
		out = append(out, sp.Name)
	}
	return out
}

func spanOf(t *testing.T, spans []store.Span, name string) store.Span {
	t.Helper()
	for _, sp := range spans {
		if sp.Name == name {
			return sp
		}
	}
	t.Fatalf("no %q step was recorded; the steps were %v", name, stepNames(spans))
	return store.Span{}
}

func TestServedRequestRecordsItsSteps(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}}`), nil, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ev := h.sink.last(t)
	want := []string{store.SpanAuth, store.SpanReceive, store.SpanAdmit, store.SpanPrepare,
		store.SpanUpstream, store.SpanRespond}
	if got := stepNames(ev.Spans); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if up := spanOf(t, ev.Spans, store.SpanUpstream); up.Of != "keera-code" {
		t.Errorf("the upstream step names %q; it has to name the model that was asked", up.Of)
	}
	if up := spanOf(t, ev.Spans, store.SpanUpstream); up.Note != "answered 200" {
		t.Errorf("upstream note = %q, want %q", up.Note, "answered 200")
	}
}

// The chart is read as a breakdown of the number beside it, so the steps have
// to start at the moment the latency does, run in order, and end inside it.
// Bars that overflowed the total, or started before it, would be a chart that
// invites its reader to distrust the row it is on.
func TestStepsAddUpToTheRequestTheyAreOn(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}}`), nil, nil)
	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)

	ev := h.sink.last(t)
	if len(ev.Spans) == 0 {
		t.Fatal("no steps were recorded")
	}
	if at := ev.Spans[0].At; at != 0 {
		t.Errorf("the first step begins %d us in; it has to begin where the latency does", at)
	}
	total := ev.Latency.Microseconds()
	var end int64
	for _, sp := range ev.Spans {
		if sp.At < end {
			t.Errorf("the %q step begins at %d us, before the one before it ended at %d",
				sp.Name, sp.At, end)
		}
		end = sp.At + sp.For
		if end > total {
			t.Errorf("the %q step ends at %d us, past the %d us the request took",
				sp.Name, end, total)
		}
	}
}

// A refused request is the one a chart is most often opened for, so the step it
// was stopped in is recorded rather than dropped, and it says what stopped it.
func TestRefusalRecordsTheStepItWasStoppedIn(t *testing.T) {
	h := newHarness(t, jsonBackend(`{}`), nil, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"nothing-serves-this","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	ev := h.sink.last(t)
	want := []string{store.SpanAuth, store.SpanReceive, store.SpanAdmit}
	if got := stepNames(ev.Spans); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v - a refused request went through the steps it "+
			"reached and no further", got, want)
	}
	if note := spanOf(t, ev.Spans, store.SpanAdmit).Note; note != "model_not_found" {
		t.Errorf("the admission step says %q; it has to say what stopped the request", note)
	}
}

// A streamed answer is two steps, because the two are read completely
// differently: waiting is the model deciding what to say, and streaming is it
// saying it.
func TestStreamedAnswerIsRecordedAsWaitingAndThenStreaming(t *testing.T) {
	h := newHarness(t, sseBackend(
		`{"choices":[{"delta":{"content":"he"}}]}`,
		`{"choices":[{"delta":{"content":"llo"}}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
	), nil, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The request is not over, and so is not recorded, until the stream is.
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	ev := h.sink.last(t)
	want := []string{store.SpanAuth, store.SpanReceive, store.SpanAdmit, store.SpanPrepare,
		store.SpanUpstream, store.SpanWait, store.SpanStream}
	if got := stepNames(ev.Spans); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if note := spanOf(t, ev.Spans, store.SpanStream).Note; note != "2 tokens" {
		t.Errorf("the streaming step says %q, want %q", note, "2 tokens")
	}
}

// The destinations a fallback router tried and gave up on are the whole reason
// such a request was slow, and they are invisible in every other reading of it:
// the row names the model that answered.
func TestFallbackRouterRecordsEveryDestinationItTried(t *testing.T) {
	h := chainHarness(t, chainRouter(), map[string]int{"small-served": 503})

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"ha","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ev := h.sink.last(t)
	var tried []string
	for _, sp := range ev.Spans {
		if sp.Name == store.SpanUpstream {
			tried = append(tried, sp.Of+" "+sp.Note)
		}
	}
	want := []string{"keera-small answered 503", "keera-large answered 200"}
	if !slices.Equal(tried, want) {
		t.Errorf("the destinations recorded were %v, want %v", tried, want)
	}
	// A fallback router decides nothing, so it has no step of its own: the
	// time it cost is the failed attempt above, drawn as itself.
	if slices.Contains(stepNames(ev.Spans), store.SpanRoute) {
		t.Errorf("a fallback router was drawn as a decision: %v", stepNames(ev.Spans))
	}
}

// A filter is a second model answering a second question, and on a request
// that felt slow it is as often the answer as the model is.
func TestFilterIsRecordedAsAStepOfItsOwn(t *testing.T) {
	h := filterHarness(t, redactor, nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ev := h.sink.last(t)
	sp := spanOf(t, ev.Spans, store.SpanFilter)
	if sp.Of != "redact" {
		t.Errorf("the filter step names %q, want %q", sp.Of, "redact")
	}
	if sp.Note == "" {
		t.Error("the filter step says nothing about what the filter did")
	}
	// It has to sit where it ran: after the request was admitted and before
	// anything was sent to a model.
	order := stepNames(ev.Spans)
	if slices.Index(order, store.SpanFilter) < slices.Index(order, store.SpanAdmit) ||
		slices.Index(order, store.SpanFilter) > slices.Index(order, store.SpanUpstream) {
		t.Errorf("the filter step is in the wrong place: %v", order)
	}
}

// An instruction router's own generation is time the request waited before it
// was sent anywhere, and the bar carries where it decided to send it.
func TestRouterDecisionIsRecordedWithWhereItSent(t *testing.T) {
	h := routerHarness(t, picks("keera-large"), autoRouter(), nil)

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ev := h.sink.last(t)
	sp := spanOf(t, ev.Spans, store.SpanRoute)
	if sp.Of != "auto" {
		t.Errorf("the routing step names %q, want %q", sp.Of, "auto")
	}
	if sp.Note != "chose keera-large" {
		t.Errorf("routing note = %q, want %q", sp.Note, "chose keera-large")
	}
	if !strings.Contains(strings.Join(stepNames(ev.Spans), " "), store.SpanRoute+" "+store.SpanPrepare) {
		t.Errorf("the decision was not recorded before the request was prepared: %v",
			stepNames(ev.Spans))
	}
}

// Authentication is time the client waited whether or not the key was in cache,
// so the clock starts before it rather than after.
func TestTheClockStartsBeforeTheKeyIsResolved(t *testing.T) {
	h := newHarness(t, jsonBackend(`{"choices":[{"message":{"content":"hi"}}]}`), nil, nil)
	h.post(t, "/v1/chat/completions",
		`{"model":"keera-code","messages":[{"role":"user","content":"hello"}]}`)

	ev := h.sink.last(t)
	if ev.Spans[0].Name != store.SpanAuth {
		t.Fatalf("the first step is %q, want %q", ev.Spans[0].Name, store.SpanAuth)
	}
	if ev.Latency <= 0 || ev.Latency > time.Minute {
		t.Errorf("latency = %s, which is not a time this request took", ev.Latency)
	}
}
