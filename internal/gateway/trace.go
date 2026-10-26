package gateway

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// A trace is the timed steps of one request, kept on its usage row so the
// panel can show where the time went. It is not a distributed tracer:
// everything happens inside one handler.

// trace collects the steps of one request. start is the origin of every step
// and of the request's latency, so the steps add up.
type trace struct {
	start time.Time
	spans []store.Span

	// The step under way, if any, so a request stopped mid-step still shows
	// it.
	name, of string
	at       time.Time
}

// newTrace starts the clock before the key is resolved, since the client
// waits for that too.
func newTrace() *trace { return &trace{start: time.Now()} }

// open begins a step, ending whatever was already open.
func (t *trace) open(name, of string) {
	if t == nil {
		return
	}
	t.seal("")
	t.name, t.of, t.at = name, of, time.Now()
}

// seal ends the open step, if any, with a note on its result.
func (t *trace) seal(note string) {
	if t == nil || t.name == "" {
		return
	}
	name, of, at := t.name, t.of, t.at
	t.name, t.of = "", ""
	t.add(name, of, at, time.Since(at), note)
}

// since records a step that began at from and has just ended.
func (t *trace) since(name, of string, from time.Time, note string) {
	t.add(name, of, from, time.Since(from), note)
}

// add records a step measured elsewhere, so the chart and that step's log
// agree.
func (t *trace) add(name, of string, from time.Time, took time.Duration, note string) {
	if t == nil {
		return
	}
	t.spans = append(t.spans, store.Span{
		Name: name,
		Of:   of,
		At:   max(from.Sub(t.start).Microseconds(), 0),
		For:  max(took.Microseconds(), 0),
		Note: note,
	})
}

// steps is what was recorded, for the request's row. It seals any open step
// first, since the caller is done with the request.
func (t *trace) steps(note string) []store.Span {
	if t == nil {
		return nil
	}
	t.seal(note)
	return t.spans
}

// took records a step that just ended after running for took.
func (t *trace) took(name, of string, took time.Duration, note string) {
	t.add(name, of, time.Now().Add(-took), took, note)
}

// routeNote says where a route step sent the request, and whether it decided
// or fell back.
func routeNote(d routeDecision, ref *refusal) string {
	switch {
	case ref != nil:
		return "could not choose"
	case d.why != "":
		return "fell back to " + d.alias
	case d.alias != "":
		return "chose " + d.alias
	default:
		return ""
	}
}

// attemptNote says how asking one destination went.
func attemptNote(resp *http.Response, err error) string {
	if err != nil {
		return "unreachable"
	}
	if resp == nil {
		return ""
	}
	return "answered " + strconv.Itoa(resp.StatusCode)
}

// plural writes a count with its unit.
func plural(n int, unit string) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
