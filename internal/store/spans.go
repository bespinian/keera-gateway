package store

import (
	"encoding/json"
	"slices"
	"strconv"
)

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

// spanCodes numbers the step names for storage. Only append to it: stored
// rows keep the number they were written with.
var spanCodes = []string{
	SpanAuth, SpanReceive, SpanAdmit, SpanRoute, SpanFilter,
	SpanPrepare, SpanUpstream, SpanWait, SpanStream, SpanRespond,
}

// spanDefaults is what a step's Of and Note usually are, from the request's
// row: the model that answered, and its status. Storage leaves out a field
// that matches.
func spanDefaults(name, alias string, status int) (of, note string) {
	switch name {
	case SpanUpstream:
		return alias, "answered " + strconv.Itoa(status)
	case SpanWait, SpanStream, SpanRespond:
		return alias, ""
	}
	return "", ""
}

// spansJSON encodes the steps for the json column of a row with this alias and
// status. No steps is NULL rather than an empty array, because the panel shows
// the two differently.
//
// The busiest table pays for every byte, so each step is an array,
// [code, gap, for, of, note]: the name's number in spanCodes, the time since
// the step before ended, and of and note as null or left off when they match
// spanDefaults. A marshalling error, which Span cannot produce, counts as no
// steps.
func spansJSON(spans []Span, alias string, status int) []byte {
	if len(spans) == 0 {
		return nil
	}
	if len(spans) > maxSpans {
		spans = spans[:maxSpans]
	}
	rows := make([][]any, len(spans))
	var end int64
	for i, sp := range spans {
		code := slices.Index(spanCodes, sp.Name)
		if code < 0 {
			return namedSpansJSON(spans)
		}
		of, note := spanDefaults(sp.Name, alias, status)
		row := []any{code, sp.At - end, sp.For, unlessEqual(sp.Of, of), unlessEqual(sp.Note, note)}
		for len(row) > 3 && row[len(row)-1] == nil {
			row = row[:len(row)-1]
		}
		rows[i] = row
		end = sp.At + sp.For
	}
	return marshalSpans(rows)
}

func unlessEqual(v, def string) any {
	if v == def {
		return nil
	}
	return v
}

// namedSpansJSON is the form before v0.13.0, [name, at, for, of, note]. It
// is still written for a step spanCodes does not know, so nothing is lost.
func namedSpansJSON(spans []Span) []byte {
	rows := make([][]any, len(spans))
	for i, sp := range spans {
		row := []any{sp.Name, sp.At, sp.For, sp.Of, sp.Note}
		for len(row) > 3 && row[len(row)-1] == "" {
			row = row[:len(row)-1]
		}
		rows[i] = row
	}
	return marshalSpans(rows)
}

func marshalSpans(rows [][]any) []byte {
	out, err := json.Marshal(rows)
	if err != nil {
		return nil
	}
	return out
}

// readSpans decodes the json column of a row with this alias and status, in
// whichever form it was written.
func readSpans(raw []byte, alias string, status int) ([]Span, error) {
	if raw == nil {
		return nil, nil
	}
	var rows [][]json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 || len(rows[0]) == 0 ||
		rows[0][0][0] == '"' {
		// Written before v0.13.0: each step names itself.
		var spans []Span
		err := json.Unmarshal(raw, &spans)
		return spans, err
	}
	spans := make([]Span, len(rows))
	var end int64
	for i, row := range rows {
		sp := &spans[i]
		var code int
		var gap int64
		var of, note *string
		fields := []any{&code, &gap, &sp.For, &of, &note}
		for j, f := range row[:min(len(row), len(fields))] {
			if err := json.Unmarshal(f, fields[j]); err != nil {
				return nil, err
			}
		}
		// An unknown number is a step a newer gateway wrote. It keeps its
		// time, without a name.
		if code >= 0 && code < len(spanCodes) {
			sp.Name = spanCodes[code]
		}
		sp.At = end + gap
		end = sp.At + sp.For
		sp.Of, sp.Note = spanDefaults(sp.Name, alias, status)
		if of != nil {
			sp.Of = *of
		}
		if note != nil {
			sp.Note = *note
		}
	}
	return spans, nil
}

// UnmarshalJSON reads one step in the object form the API sends, or as
// namedSpansJSON writes it.
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
