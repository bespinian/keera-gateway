package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The request log, read as it is written.
//
// Polling would make every open tab a query per interval, whether or not
// anything happened. Instead, the write path announces each batch of usage
// events on store.EventsChannel. One listener per process picks that up and
// tells every open stream to look. The notice carries no data: each stream
// asks its own question, narrowed to what its reader may see.

const (
	// streamTick bounds how often one stream queries the database. Notices
	// can come several times a second; a reader cannot tell the difference,
	// the database can.
	streamTick = time.Second
	// streamKeepalive is how often an idle stream sends a comment, so a proxy
	// does not close it and make the browser reconnect every minute.
	streamKeepalive = 25 * time.Second
	// streamRows is the most rows one tick sends. Beyond that nobody can read
	// along, and the counts are computed over the whole window anyway.
	streamRows = 200
	// streamRecount is how often a stream counts its whole window again.
	// Between recounts it only counts the new rows, because counting a busy
	// week every second, for every open tab, would load the database more than
	// the traffic it shows. The recount drops rows that left the window.
	streamRecount = time.Minute
	// feedRetry is how long a dropped listener waits before reconnecting.
	feedRetry = 2 * time.Second
)

// requestFeed fans one notification out to every stream open in this process.
// There is one listener, not one per reader, because each listener holds a
// Postgres connection.
type requestFeed struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
	// closing is closed on shutdown. It is how a response that never ends by
	// itself is told to end.
	closing chan struct{}
	once    sync.Once
}

func newRequestFeed() *requestFeed {
	return &requestFeed{
		subs:    make(map[chan struct{}]struct{}),
		closing: make(chan struct{}),
	}
}

// watch subscribes. It returns the channel that ticks when the log grows and
// the function that unsubscribes, which the caller must call.
func (f *requestFeed) watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
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
// A graceful shutdown waits for open responses, and these only end when the
// reader closes the tab. Without Drain every shutdown would wait out its whole
// grace period. The browser reconnects to the next process by itself.
func (s *Server) Drain() { s.feed.once.Do(func() { close(s.feed.closing) }) }

// wake tells every stream that there is something new to look for.
//
// It never blocks. Each channel holds one tick, and a second would say the
// same: a stream reads everything past its own cursor in one look.
func (f *requestFeed) wake() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run holds the process's listener on the usage-event channel until ctx ends.
//
// A lost listener only adds delay: streams read from an id cursor, so the next
// look picks up whatever was missed. That is also why every attempt starts by
// waking everyone.
func (s *Server) Run(ctx context.Context) {
	if s.st == nil {
		return
	}
	for ctx.Err() == nil {
		s.feed.wake()
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

// requestStream is the request log as it is written: the rows GET
// /v1/requests returns, narrowed the same way, sent as they arrive.
//
// It takes the same query, plus `after`: the newest row the reader already
// has. Without it the stream starts at the end of the log, rather than
// replaying what the reader is already looking at.
//
// Administrator-only, like the request log.
func (s *Server) requestStream(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	if !p.CanAdminOrg(p.OrgID) {
		s.forbid(w, "only an administrator can read the request log")
		return
	}
	orgID, _, _, ok := s.reportScope(w, r, p)
	if !ok {
		return
	}
	sc, ok := s.entityScope(w, r, orgID)
	if !ok {
		return
	}

	q := r.URL.Query()
	rq := store.RequestQuery{
		OrgID:   orgID,
		Scope:   sc,
		Outcome: store.Outcome(q.Get("outcome")),
		Limit:   streamRows,
	}
	rq.Status, rq.StatusClass = parseStatus(q.Get("status"))
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
	names, err := s.groupNames(ctx, orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The window is not fixed here: a live row is inside every window, and a
	// fixed end would stop the stream a second later. Only the counts use it,
	// and they are recomputed against the clock each time.
	s.streamRequests(ctx, w, &rq, &names, orgID, q.Get("since"))
}

// streamRequests writes the event stream until the reader leaves or the
// server shuts down.
func (s *Server) streamRequests(ctx context.Context, w http.ResponseWriter,
	rq *store.RequestQuery, names *groupLabels, orgID, since string,
) {
	// From here on the body has started, so a failure cannot be a status code.
	// It ends the stream, and the browser reconnects.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// Nothing in between may buffer this response.
	h.Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// The listener's write timeout is for small JSON answers. This one stays
	// open as long as someone reads, so the timeout is lifted for it alone. A
	// test recorder does not support this, which is fine.
	_ = rc.SetWriteDeadline(time.Time{})
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	ticks, stop := s.feed.watch()
	defer stop()

	poll := time.NewTicker(streamTick)
	defer poll.Stop()
	alive := time.NewTicker(streamKeepalive)
	defer alive.Stop()

	// Look once before waiting. A stream reopened after a pause has a backlog,
	// which would otherwise wait for the next request to arrive.
	grown := true
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
			if !s.sendRequests(ctx, w, rc, rq, names, &tally, orgID, since) {
				return
			}
		}
	}
}

// outcomeTally is one stream's running outcome counts.
type outcomeTally struct {
	counts store.RequestOutcomes
	// upTo is the newest id the counts include, and at when the whole window
	// was last counted. A zero at means nothing was counted yet.
	upTo int64
	at   time.Time
}

// sendRequests writes the rows past the cursor, moving the cursor and
// refreshing the labels as needed. It reports whether the stream can go on.
func (s *Server) sendRequests(ctx context.Context, w http.ResponseWriter,
	rc *http.ResponseController, rq *store.RequestQuery, names *groupLabels,
	tally *outcomeTally, orgID, since string) bool {
	rows, err := s.st.Requests(ctx, *rq)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("reading the live request log failed", "error", err)
		}
		return false
	}
	if len(rows) == 0 {
		return true
	}
	// Newest first, so the first id is the new cursor. Older rows that did not
	// fit are not resent: a tail that fell behind stays behind, and the counts
	// are not built from these rows, so they stay right.
	rq.After = rows[0].ID

	if err := s.countOutcomes(ctx, rq, tally, since); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("counting the live request log failed", "error", err)
		}
		return false
	}

	out := map[string]any{
		"data": rows, "outcomes": tally.counts, "next_after": rq.After,
	}
	// A brand-new key is the one someone is most likely watching for, and its
	// name is not in the labels yet. So the labels are re-read, and sent, only
	// when a row has an id they cannot name.
	if unnamed(rows, *names) {
		fresh, err := s.groupNames(ctx, orgID)
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

// countOutcomes brings the tally up to the cursor. Both ends are bounded by
// id, so each row is counted once, even when a batch sends only some of them.
func (s *Server) countOutcomes(ctx context.Context, rq *store.RequestQuery,
	tally *outcomeTally, since string) error {
	q := store.RequestQuery{
		OrgID: rq.OrgID, Scope: rq.Scope,
		Status: rq.Status, StatusClass: rq.StatusClass,
		Before: rq.After + 1,
	}
	now := time.Now()
	if tally.at.IsZero() || now.Sub(tally.at) >= streamRecount {
		// The window moves with the clock, so a stream left open overnight
		// counts the last 24 hours. Its end is the cursor, not the clock. The
		// error cannot happen: reportScope already checked this string.
		q.From, _, _ = timeRange("", "", since)
		counts, err := s.st.Outcomes(ctx, q)
		if err != nil {
			return err
		}
		*tally = outcomeTally{counts: counts, upTo: rq.After, at: now}
		return nil
	}
	q.After = tally.upTo
	counts, err := s.st.Outcomes(ctx, q)
	if err != nil {
		return err
	}
	tally.counts.Add(counts)
	tally.upTo = rq.After
	return nil
}

// unnamed reports whether any row names a key, team or person the labels cannot
// put a name to.
func unnamed(rows []store.Request, names groupLabels) bool {
	for _, q := range rows {
		switch {
		case q.KeyID != "" && names.keys[q.KeyID] == "":
			return true
		case q.TeamID != "" && names.teams[q.TeamID] == "":
			return true
		case q.UserID != "" && names.users[q.UserID] == "":
			return true
		}
	}
	return false
}
