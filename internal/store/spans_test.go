package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSpansAreStoredAsCompactArrays(t *testing.T) {
	spans := []Span{
		{Name: SpanAuth, At: 0, For: 85},
		{Name: SpanUpstream, Of: "claude-sonnet", At: 1340, For: 812000, Note: "answered 200"},
		{Name: SpanFilter, At: 900, For: 400, Note: "refused"},
	}
	raw := spansJSON(spans)
	want := `[["auth",0,85],["upstream",1340,812000,"claude-sonnet","answered 200"],` +
		`["filter",900,400,"","refused"]]`
	if string(raw) != want {
		t.Errorf("stored %s, want %s", raw, want)
	}

	var got []Span
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, spans) {
		t.Errorf("read back %+v, want %+v", got, spans)
	}
}

func TestSpansWrittenBeforeV0_10_0StillRead(t *testing.T) {
	old := `[{"name":"upstream","of":"m","at_us":10,"for_us":20,"note":"answered 503"}]`
	var got []Span
	if err := json.Unmarshal([]byte(old), &got); err != nil {
		t.Fatal(err)
	}
	want := []Span{{Name: SpanUpstream, Of: "m", At: 10, For: 20, Note: "answered 503"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read %+v, want %+v", got, want)
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
	if spansJSON(nil) != nil {
		t.Error("no steps was not NULL")
	}
	many := make([]Span, maxSpans+10)
	for i := range many {
		many[i].Name = SpanFilter
	}
	if n := strings.Count(string(spansJSON(many)), `"filter"`); n != maxSpans {
		t.Errorf("stored %d steps, want %d", n, maxSpans)
	}
}
