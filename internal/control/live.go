package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The request log, read as it is written.
//
// Polling would cost every open tab a query per interval. Instead, the write
// path announces each batch of usage events on store.EventsChannel, and one
// listener per process wakes the streams of the organisations it names. The
// notice carries nothing else: each stream runs its own query, narrowed to
// what its reader may see.

const (
	// streamTick bounds how often one stream queries the database. Notices
	// can come several times a second; the reader would not notice, the
	// database would.
	streamTick = time.Second
	// streamKeepalive is how often an idle stream sends a comment, so a proxy
	// does not close it and make the browser reconnect every minute.
	streamKeepalive = 25 * time.Second
	// streamRows is the most rows one tick sends. Nobody can read more, and
	// the counts cover the whole window anyway.
	streamRows = 200
	// streamRecount is how often a stream counts its whole window again.
	// Between recounts it counts only new rows: recounting a busy week every
	// second, for every tab, would load the database more than the traffic it
	// shows. The recount drops rows that left the window.
	streamRecount = time.Minute
	// feedRetry is how long a dropped listener waits before reconnecting.
	feedRetry = 2 * time.Second
	// streamSettle is how long a stream keeps looking behind its newest row
	// for late commits. The usage recorder's write timeout is ten seconds.
	streamSettle = 30 * time.Second
)

// requestFeed fans one notification out to every stream in this process.
// One listener, not one per reader, because each holds a Postgres connection.
type requestFeed struct {
	mu sync.Mutex
	// subs holds each stream's organisation, or "" for all of them.
	subs map[chan struct{}]string
	// closing is closed on shutdown, to end responses that never end by
	// themselves.
	closing chan struct{}
	once    sync.Once
}

func newRequestFeed() *requestFeed {
	return &requestFeed{
		subs:    make(map[chan struct{}]string),
		closing: make(chan struct{}),
	}
}

// watch subscribes to one organisation's requests, or all for "". The caller
// must call the returned function to unsubscribe.
func (f *requestFeed) watch(orgID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.subs[ch] = orgID
	f.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
		})
	}
}

// Drain ends every open request stream.
//
// These responses only end when the reader closes the tab, so without Drain a
// graceful shutdown would wait out its whole grace period. The browser
// reconnects to the next process by itself.
func (s *Server) Drain() { s.feed.once.Do(func() { close(s.feed.closing) }) }

// wake tells the streams of the organisations a payload names to look again.
// An empty payload wakes every stream.
//
// It never blocks. Each channel holds one tick, and a second would add
// nothing: a stream reads everything past its cursor in one look.
func (f *requestFeed) wake(payload string) {
	orgs, all := store.EventOrgs(payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch, org := range f.subs {
		if !all && org != "" && !slices.Contains(orgs, org) {
			continue
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run holds the process's listener on the usage-event channel until ctx ends.
//
// A lost listener only adds delay: streams read from an id cursor, so the next
// look picks up what was missed. That is why every attempt first wakes everyone.
func (s *Server) Run(ctx context.Context) {
	if s.st == nil {
		return
	}
	for ctx.Err() == nil {
		s.feed.wake("")
		if err := s.st.Listen(ctx, store.EventsChannel, s.feed.wake); err != nil &&
			ctx.Err() == nil {
			s.log.Warn("the usage-event listener dropped", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(feedRetry):
			}
		}
	}
}

// requestStream sends the rows GET /v1/requests returns, narrowed the same
// way, as they arrive. A member gets only their own requests.
//
// It takes the same query, plus `after`: the newest row the reader already
// has. Without it the stream starts at the end of the log.
func (s *Server) requestStream(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	rq, _, _, ok := s.requestQuery(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	rq.Limit = streamRows
	rq.After, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	ctx := r.Context()
	if rq.After <= 0 {
		latest, err := s.st.LatestEventID(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		rq.After = latest
	}
	names, err := s.groupNames(ctx, rq.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The window is not fixed here: a fixed end would stop the stream a second
	// later. Only the counts use it, recomputed against the clock each time.
	s.streamRequests(ctx, w, &rq, &names, q.Get("since"))
}

// streamRequests writes the event stream until the reader leaves or the
// server shuts down.
func (s *Server) streamRequests(ctx context.Context, w http.ResponseWriter,
	rq *store.RequestQuery, names *groupLabels, since string,
) {
	// From here on the body has started, so a failure cannot be a status code.
	// It ends the stream, and the browser reconnects.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// Nothing in between may buffer this response.
	h.Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// The listener's write timeout is for small JSON answers; this response
	// stays open as long as someone reads. A test recorder ignores this.
	_ = rc.SetWriteDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ticks, stop := s.feed.watch(rq.OrgID)
	defer stop()

	poll := time.NewTicker(streamTick)
	defer poll.Stop()
	alive := time.NewTicker(streamKeepalive)
	defer alive.Stop()

	// Look once before waiting: a stream reopened after a pause has a backlog.
	grown := true
	cur := newLiveCursor(rq.After)
	var tally outcomeTally
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.feed.closing:
			return
		case <-ticks:
			grown = true
		case <-alive.C:
			// A comment is a valid event that means nothing.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case <-poll.C:
			if !grown {
				continue
			}
			grown = false
			if !s.sendRequests(ctx, w, rc, rq, cur, names, &tally, since) {
				return
			}
		}
	}
}

// liveCursor is where a stream is in the log.
//
// A row gets its id on insert but is visible only once its batch commits.
// With several replicas, a row can appear behind the newest one already sent.
// So a stream reads from settled, behind which every batch has committed, and
// skips the rows it sent since.
type liveCursor struct {
	// settled is the id up to which nothing can still appear.
	settled int64
	// newest is the newest id sent; a reconnecting reader resumes from it.
	newest int64
	// sent is the ids sent past settled.
	sent []int64
	// seen is newest at each batch, oldest first. Every row up to one of these
	// ids has committed streamSettle later.
	seen []seenID
}

type seenID struct {
	id int64
	at time.Time
}

func newLiveCursor(after int64) *liveCursor {
	return &liveCursor{settled: after, newest: after}
}

func (c *liveCursor) query(rq store.RequestQuery) store.RequestQuery {
	rq.After, rq.Skip = c.settled, c.sent
	return rq
}

// settle moves settled up to the newest id seen streamSettle ago, and forgets
// what was sent behind it.
func (c *liveCursor) settle(now time.Time) {
	n := 0
	for n < len(c.seen) && now.Sub(c.seen[n].at) >= streamSettle {
		c.settled = max(c.settled, c.seen[n].id)
		n++
	}
	c.seen = slices.Delete(c.seen, 0, n)
	c.forget()
}

// took records a batch sent at now. Rows come newest first.
func (c *liveCursor) took(rows []store.Request, limit int, now time.Time) {
	for _, r := range rows {
		c.sent = append(c.sent, r.ID)
		c.newest = max(c.newest, r.ID)
	}
	c.seen = append(c.seen, seenID{c.newest, now})
	// A full batch means the reader fell behind. The older rows that did not
	// fit are not sent later: a tail that fell behind stays behind.
	if len(rows) >= limit {
		c.settled = max(c.settled, rows[len(rows)-1].ID)
		c.forget()
	}
}

func (c *liveCursor) forget() {
	c.sent = slices.DeleteFunc(c.sent, func(id int64) bool { return id <= c.settled })
}

type outcomeTally struct {
	// base counts the window up to upTo, a settled id, so no late row can
	// change it. at is when the whole window was last counted; a zero at
	// means nothing was counted yet.
	base store.RequestOutcomes
	upTo int64
	at   time.Time
}

// sendRequests writes the rows not sent yet. It reports whether the stream
// can go on.
func (s *Server) sendRequests(ctx context.Context, w http.ResponseWriter,
	rc *http.ResponseController, rq *store.RequestQuery, cur *liveCursor,
	names *groupLabels, tally *outcomeTally, since string) bool {
	now := time.Now()
	cur.settle(now)
	rows, err := s.st.Requests(ctx, cur.query(*rq))
	if err != nil {
		s.warnUnlessDone(ctx, "reading the live request log failed", "error", err)
		return false
	}
	if len(rows) == 0 {
		return true
	}
	cur.took(rows, rq.Limit, now)

	counts, err := s.countOutcomes(ctx, *rq, cur.settled, tally, since)
	if err != nil {
		s.warnUnlessDone(ctx, "counting the live request log failed", "error", err)
		return false
	}

	out := map[string]any{
		"data": rows, "outcomes": counts, "next_after": cur.newest,
	}
	// A brand-new key is the likeliest one to watch for, and its name is not in
	// the labels yet. So labels are re-read, and sent, only for an unknown id.
	if unnamed(rows, *names) {
		fresh, err := s.groupNames(ctx, rq.OrgID)
		if err == nil {
			*names = fresh
			fresh.addTo(out)
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		s.log.Warn("encoding a live request batch failed", "error", err)
		return false
	}
	// json.Marshal writes no newline outside a string, so this is one event.
	if _, err := fmt.Fprintf(w, "event: requests\ndata: %s\n\n", body); err != nil {
		return false
	}
	_ = rc.Flush()
	return true
}

// countOutcomes counts the stream's window. The part up to settled is kept in
// the tally and only grows. The rows past it are counted again each time,
// because a late commit can still add to them.
func (s *Server) countOutcomes(ctx context.Context, rq store.RequestQuery, settled int64,
	tally *outcomeTally, since string) (store.RequestOutcomes, error) {
	q := store.RequestQuery{
		OrgID: rq.OrgID, ReportScope: rq.ReportScope, Holder: rq.Holder,
		Status: rq.Status, StatusClass: rq.StatusClass, RequestID: rq.RequestID,
	}
	now := time.Now()
	switch {
	case tally.at.IsZero() || now.Sub(tally.at) >= streamRecount:
		// The window moves with the clock. The error cannot happen:
		// reportScope already checked this string.
		whole := q
		// A search by id has no window, as on the request log.
		if q.RequestID == "" {
			whole.From, _, _ = timeRange("", "", since)
		}
		whole.Before = settled + 1
		base, err := s.st.Outcomes(ctx, whole)
		if err != nil {
			return store.RequestOutcomes{}, err
		}
		*tally = outcomeTally{base: base, upTo: settled, at: now}
	case settled > tally.upTo:
		step := q
		step.After, step.Before = tally.upTo, settled+1
		more, err := s.st.Outcomes(ctx, step)
		if err != nil {
			return store.RequestOutcomes{}, err
		}
		tally.base.Add(more)
		tally.upTo = settled
	}
	recent := q
	recent.After = settled
	counts, err := s.st.Outcomes(ctx, recent)
	if err != nil {
		return store.RequestOutcomes{}, err
	}
	counts.Add(tally.base)
	return counts, nil
}

// unnamed reports whether any row has a key, project or person the labels
// cannot name.
func unnamed(rows []store.Request, names groupLabels) bool {
	for _, q := range rows {
		switch {
		case q.KeyID != "" && names.keys[q.KeyID] == "":
			return true
		case q.ProjectID != "" && names.projects[q.ProjectID] == "":
			return true
		case q.UserID != "" && names.users[q.UserID] == "":
			return true
		}
	}
	return false
}
