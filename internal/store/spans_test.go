package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

// roundTrip stores spans on a row of alias and status, checks they read back
// unchanged, and returns what was stored.
func roundTrip(t *testing.T, spans []Span, alias string, status int) string {
	t.Helper()
	raw := spansJSON(spans, alias, status)
	got, err := readSpans(raw, alias, status)
	if err != nil {
		t.Fatalf("readSpans(%s): %v", raw, err)
	}
	if !reflect.DeepEqual(got, spans) {
		t.Errorf("read back %+v\nwant %+v", got, spans)
	}
	return string(raw)
}

func TestSpansLeaveOutWhatTheRowSays(t *testing.T) {
	spans := []Span{
		{Name: SpanAuth, At: 0, For: 12},
		{Name: SpanReceive, At: 14, For: 220},
		{Name: SpanFilter, Of: "pii", At: 236, For: 350000, Note: "passed"},
		{Name: SpanUpstream, Of: "qwen", At: 350240, For: 120000, Note: "answered 200"},
		{Name: SpanWait, Of: "qwen", At: 470240, For: 800000},
		{Name: SpanStream, Of: "qwen", At: 1270245, For: 9000000, Note: "812 tokens"},
	}
	got := roundTrip(t, spans, "qwen", 200)
	want := `[[0,0,12],[1,2,220],[4,2,350000,"pii","passed"],[6,4,120000],` +
		`[7,0,800000],[8,5,9000000,null,"812 tokens"]]`
	if got != want {
		t.Errorf("stored %s\nwant   %s", got, want)
	}
}

func TestSpansKeepWhatDiffersFromTheRow(t *testing.T) {
	// A router's fallback: the first destination failed, the second answered,
	// and the gateway's own answer was a 502 after the stream broke.
	roundTrip(t, []Span{
		{Name: SpanRoute, Of: "auto/first", At: 0, For: 400000, Note: "chose b"},
		{Name: SpanUpstream, Of: "a", At: 400100, For: 30, Note: "unreachable"},
		{Name: SpanUpstream, Of: "b", At: 400200, For: 900, Note: "answered 200"},
		// A step whose usual Of and Note are not there, and one before its
		// predecessor ended.
		{Name: SpanUpstream, At: 300, For: 5},
		{Name: SpanStream, At: 1000, For: 7},
	}, "b", 502)
}

func TestAnUnknownStepKeepsItsName(t *testing.T) {
	spans := []Span{{Name: SpanAuth, For: 3}, {Name: "new", Of: "x", At: 4, For: 5}}
	if got := roundTrip(t, spans, "m", 200); got != `[["auth",0,3],["new",4,5,"x"]]` {
		t.Errorf("stored %s", got)
	}
}

func TestSpansWrittenBeforeV0_13_0StillRead(t *testing.T) {
	for _, old := range []string{
		`[["upstream",10,20,"m","answered 503"]]`,                                     // v0.10.0
		`[{"name":"upstream","of":"m","at_us":10,"for_us":20,"note":"answered 503"}]`, // before
	} {
		got, err := readSpans([]byte(old), "other", 200)
		if err != nil {
			t.Fatal(err)
		}
		want := []Span{{Name: SpanUpstream, Of: "m", At: 10, For: 20, Note: "answered 503"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("read %s as %+v, want %+v", old, got, want)
		}
	}
}

func TestSpansKeepTheirShapeInTheAPI(t *testing.T) {
	// The panel and the CLI read named fields; only storage is compact.
	out, err := json.Marshal(Span{Name: SpanWait, At: 5, For: 6})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"name":"wait","at_us":5,"for_us":6}` {
		t.Errorf("API sends %s", out)
	}
}

func TestSpansAreCappedAndNoneIsNull(t *testing.T) {
	if spansJSON(nil, "m", 200) != nil {
		t.Error("no steps was not NULL")
	}
	if got, _ := readSpans(nil, "m", 200); got != nil {
		t.Errorf("NULL read as %+v", got)
	}
	many := make([]Span, maxSpans+10)
	for i := range many {
		many[i].Name = SpanFilter
	}
	got, err := readSpans(spansJSON(many, "m", 200), "m", 200)
	if err != nil || len(got) != maxSpans {
		t.Errorf("stored %d steps, %v; want %d", len(got), err, maxSpans)
	}
}
