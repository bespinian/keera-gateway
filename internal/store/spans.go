package store

import "encoding/json"

// Spans are the steps one request went through and how long each took, kept
// as a flat list on the request's row. They add up to the row's latency.

// Span names. The panel gives each its own colour and description.
const (
	// SpanAuth is resolving the presented key, usually from the cache.
	SpanAuth = "auth"
	// SpanReceive is the request body arriving from the client.
	SpanReceive = "receive"
	// SpanAdmit is translating the request, finding the model, and checking
	// rate limits and budgets.
	SpanAdmit = "admit"
	// SpanRoute is a route step's decision, made by another model.
	SpanRoute = "route"
	// SpanFilter is one filter's pass over the request.
	SpanFilter = "filter"
	// SpanPrepare is the work after the hooks and before dispatch, such as the
	// system prompt and encoding.
	SpanPrepare = "prepare"
	// SpanUpstream is one destination being asked, from the dial to its
	// response line. Only a route step that tries several has more than one.
	SpanUpstream = "upstream"
	// SpanWait is a streamed answer between the response headers and the first
	// token.
	SpanWait = "wait"
	// SpanStream is the rest of a streamed answer.
	SpanStream = "stream"
	// SpanRespond is reading, translating and writing an answer that was not
	// streamed.
	SpanRespond = "respond"
)

// Span is one step of one request. Times are microseconds, not milliseconds,
// because the gateway's own steps would otherwise round to zero.
type Span struct {
	Name string `json:"name"`
	// Of is what the step was about, if not the request itself: the filter's
	// alias, the router's name, or the destination model.
	Of string `json:"of,omitempty"`
	// At is when the step began, from when the gateway took the request.
	At  int64 `json:"at_us"`
	For int64 `json:"for_us"`
	// Note is the outcome in a few words, such as "answered 503". It is always
	// the gateway's own wording, never the upstream's.
	Note string `json:"note,omitempty"`
}

// maxSpans bounds what one request may record, so a misconfiguration cannot
// bloat the busiest table.
const maxSpans = 64

// spansJSON encodes the steps for the json column. No steps is NULL rather
// than an empty array, because the panel shows the two differently.
//
// Each step is an array, [name, at, for, of, note], without trailing empty
// fields: a third of the size of an object on the busiest table. A marshalling
// error, which Span cannot produce, counts as no steps.
func spansJSON(spans []Span) []byte {
	if len(spans) == 0 {
		return nil
	}
	if len(spans) > maxSpans {
		spans = spans[:maxSpans]
	}
	rows := make([][]any, len(spans))
	for i, sp := range spans {
		row := []any{sp.Name, sp.At, sp.For, sp.Of, sp.Note}
		for len(row) > 3 && row[len(row)-1] == "" {
			row = row[:len(row)-1]
		}
		rows[i] = row
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return nil
	}
	return out
}

// UnmarshalJSON reads a step written by spansJSON, or the object form that
// rows from before v0.10.0 hold and the API sends.
func (sp *Span) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '[' {
		type plain Span
		return json.Unmarshal(b, (*plain)(sp))
	}
	var row []json.RawMessage
	if err := json.Unmarshal(b, &row); err != nil {
		return err
	}
	*sp = Span{}
	fields := []any{&sp.Name, &sp.At, &sp.For, &sp.Of, &sp.Note}
	for i, raw := range row[:min(len(row), len(fields))] {
		if err := json.Unmarshal(raw, fields[i]); err != nil {
			return err
		}
	}
	return nil
}
