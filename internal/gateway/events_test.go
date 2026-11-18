package gateway

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// A translated stream keeps text for its closing events. A backend that sends
// small deltas without end must not grow that past the limit.
func TestATranslatedStreamStopsHoldingTextAtTheLimit(t *testing.T) {
	const limit = 4 << 10
	text := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 100) + `"}}]}`
	args := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1",` +
		`"function":{"name":"write","arguments":"` + strings.Repeat("x", 100) + `"}}]}}]}`
	for _, tc := range []struct {
		name  string
		shape shape
		chunk string
		last  string
	}{
		{"responses text", responsesShape{}, text, "response.failed"},
		{"responses arguments", responsesShape{}, args, "response.failed"},
		{"messages arguments", anthropicShape{}, args, "error"},
	} {
		chunks := make([]string, 100)
		for i := range chunks {
			chunks[i] = tc.chunk
		}
		var buf bytes.Buffer
		_, err := tc.shape.pipe(&buf, func() {}, strings.NewReader(sseStream(chunks...)),
			"keera-code", true, limit)
		if !errors.Is(err, errAnswerTooLarge) {
			t.Errorf("%s: err = %v, want the answer too large", tc.name, err)
		}
		events := readEvents(t, buf.String())
		if last := events[len(events)-1]; last.name != tc.last {
			t.Errorf("%s: the stream ends with %q, want %q", tc.name, last.name, tc.last)
		}
	}
}

// The arguments of one tool call are held only until it closes, so many
// calls that are each small pass.
func TestManySmallToolCallsPassInAMessagesStream(t *testing.T) {
	const limit = 4 << 10
	chunks := make([]string, 0, 101)
	for i := range 100 {
		n := strconv.Itoa(i)
		chunks = append(chunks, `{"choices":[{"delta":{"tool_calls":[{"index":`+n+`,"id":"c`+n+`",`+
			`"function":{"name":"write","arguments":"{\"a\":\"`+strings.Repeat("x", 100)+`\"}"}}]}}]}`)
	}
	chunks = append(chunks, stopChunk)
	var buf bytes.Buffer
	if _, err := (anthropicShape{}).pipe(&buf, func() {}, strings.NewReader(sseStream(chunks...)),
		"keera-code", true, limit); err != nil {
		t.Errorf("err = %v, want each call held on its own", err)
	}
}
