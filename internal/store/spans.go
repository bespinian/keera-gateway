package store

import "encoding/json"

// Spans are the steps one request went through and how long each took. They
// are kept on the request's own row as a flat list, and add up to the latency
// the row reports. This is not a distributed trace: nothing crosses a process
// boundary, nothing is sampled, and there is no collector.

// Span names. They are a fixed set because the panel gives each one its own
// colour and description.
const (
	// SpanAuth is resolving the presented key. Usually a cache hit; a slow one
	// means the control plane is slow.
	SpanAuth = "auth"
	// SpanReceive is the request body arriving from the client. It is waiting,
	// not work this process added.
	SpanReceive = "receive"
	// SpanAdmit is everything between having the body and acting on it:
	// translating the client's format, finding the model, and checking rate
	// limits and budgets.
	SpanAdmit = "admit"
	// SpanRoute is a router's decision: a generation on another model that the
	// request waited for.
	SpanRoute = "route"
	// SpanFilter is one filter's pass over the request: again a generation on
	// another model, one per filter.
	SpanFilter = "filter"
	// SpanPrepare is the gateway's work after the hooks and before dispatch:
	// the system prompt, the output ceiling, the encode.
	SpanPrepare = "prepare"
	// SpanUpstream is one destination being asked, from the dial to its
	// response line. Only a fallback router has more than one.
	SpanUpstream = "upstream"
	// SpanWait is a streamed answer between the response headers and the first
	// token: the model thinking.
	SpanWait = "wait"
	// SpanStream is the rest of a streamed answer, for as long as tokens keep
	// arriving.
	SpanStream = "stream"
	// SpanRespond is the same stretch for an answer that was not streamed:
	// reading, translating and writing the whole body. Its parts cannot be told
	// apart until the body has arrived.
	SpanRespond = "respond"
)

// Span is one step of one request. Times are microseconds, unlike the
// milliseconds used elsewhere, because the gateway's own steps take tens of
// microseconds and would otherwise all round to zero.
type Span struct {
	// Name is which step this is, from the names above.
	Name string `json:"name"`
	// Of is what the step was about, if not the request itself: the filter's
	// alias, the router's name, or the destination model.
	Of string `json:"of,omitempty"`
	// At is when the step began, measured from when the gateway took the
	// request. It is stored because the gaps between steps matter.
	At int64 `json:"at_us"`
	// For is how long it ran.
	For int64 `json:"for_us"`
	// Note is the outcome in a few words, such as "answered 503". It is always
	// the gateway's own wording, never the client's or the upstream's, so no
	// stack trace ends up stored on every row.
	Note string `json:"note,omitempty"`
}

// maxSpans bounds what one request may record. Configuration, not the client,
// decides the number of steps; this only stops a misconfiguration from
// bloating the busiest table.
const maxSpans = 64

// spansJSON encodes the steps for the jsonb column. No steps is NULL rather
// than an empty array, because the panel shows the two differently.
//
// A marshalling error is treated as no steps. Span cannot produce one, and a
// served request is not worth failing to record over it.
func spansJSON(spans []Span) []byte {
	if len(spans) == 0 {
		return nil
	}
	if len(spans) > maxSpans {
		spans = spans[:maxSpans]
	}
	out, err := json.Marshal(spans)
	if err != nil {
		return nil
	}
	return out
}
