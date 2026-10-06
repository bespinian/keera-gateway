package control

import (
	"slices"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// The feed is what turns one database notification into every open reader's
// cue to look. Its whole contract is in three properties, and each of them is a
// bug somebody would otherwise find in production: everyone is told, a burst is
// one cue rather than a queue of them, and a reader who has gone is not kept.

func TestFeedWakesEveryOpenStream(t *testing.T) {
	f := newRequestFeed()
	first, stopFirst := f.watch()
	second, stopSecond := f.watch()
	defer stopFirst()
	defer stopSecond()

	f.wake()
	for i, ch := range []<-chan struct{}{first, second} {
		select {
		case <-ch:
		default:
			t.Errorf("stream %d was not woken; a reader nobody tells is a reader polling", i)
		}
	}
}

func TestFeedCollapsesABurstIntoOneCue(t *testing.T) {
	// A busy deployment writes several batches a second. Each cue costs the
	// stream that receives it a query, and the second one would ask exactly the
	// question the first already answers - the cursor it reads from picks up
	// everything since the last look however many batches that was.
	f := newRequestFeed()
	ch, stop := f.watch()
	defer stop()

	for range 50 {
		f.wake()
	}
	if len(ch) != 1 {
		t.Errorf("%d cues queued after 50 notifications, want 1", len(ch))
	}
	<-ch
	select {
	case <-ch:
		t.Error("a second cue was waiting; the burst was queued rather than collapsed")
	default:
	}
}

func TestFeedForgetsAStreamThatHasGone(t *testing.T) {
	// wake sends to every subscriber it holds. One left behind by a reader who
	// closed the tab is a channel nobody will ever receive from, and the map
	// entry that keeps it is the leak.
	f := newRequestFeed()
	_, stop := f.watch()
	if got := len(f.subs); got != 1 {
		t.Fatalf("%d subscribers after one watch, want 1", got)
	}
	stop()
	if got := len(f.subs); got != 0 {
		t.Errorf("%d subscribers after unsubscribing, want none", got)
	}
	// Twice, because a handler that both returns and is cancelled must not be
	// able to delete a later reader's subscription.
	stop()
	f.wake()
}

func TestDrainEndsEveryStreamAndIsSafeTwice(t *testing.T) {
	// A graceful shutdown waits for in-flight responses, and one of these
	// finishes when its reader closes the tab. Without this the listener sits
	// out its whole grace period every time it is asked to stop.
	s := newServer()
	select {
	case <-s.feed.closing:
		t.Fatal("streams were told to close before anything asked them to")
	default:
	}

	s.Drain()
	select {
	case <-s.feed.closing:
	default:
		t.Error("Drain did not end the open streams")
	}
	// Shutdown can be reached more than once - a signal, then a listener error.
	// Closing a closed channel panics, so this is the test that it cannot.
	s.Drain()
}

// The cursor has to catch a row that is committed after a newer one, which is
// what happens when several replicas write the log.

func rowsOf(ids ...int64) []store.Request {
	rows := make([]store.Request, len(ids))
	for i, id := range ids {
		rows[i] = store.Request{ID: id}
	}
	return rows
}

func TestCursorReadsAgainForARowCommittedLate(t *testing.T) {
	now := time.Now()
	c := newLiveCursor(100)

	// Rows 103 and 102 are in; 101 is still in a batch that has not committed.
	c.took(rowsOf(103, 102), streamRows, now)
	q := c.query(store.RequestQuery{})
	if q.After != 100 {
		t.Errorf("After = %d, want 100: row 101 can still appear", q.After)
	}
	if !slices.Equal(slices.Sorted(slices.Values(q.Skip)), []int64{102, 103}) {
		t.Errorf("Skip = %v, want the rows already sent", q.Skip)
	}
	if c.newest != 103 {
		t.Errorf("newest = %d, want 103", c.newest)
	}
}

func TestCursorSettlesOnceEveryBatchHasCommitted(t *testing.T) {
	now := time.Now()
	c := newLiveCursor(100)
	c.took(rowsOf(103, 102), streamRows, now)
	c.took(rowsOf(105), streamRows, now.Add(time.Second))

	c.settle(now.Add(streamSettle))
	q := c.query(store.RequestQuery{})
	if q.After != 103 {
		t.Errorf("After = %d, want 103: everything up to it has committed by now", q.After)
	}
	if !slices.Equal(q.Skip, []int64{105}) {
		t.Errorf("Skip = %v, want only what was sent past the settled id", q.Skip)
	}
}

func TestCursorLetsAFullBatchsBacklogGo(t *testing.T) {
	// A reader that fell behind is sent the newest rows, not the backlog.
	now := time.Now()
	c := newLiveCursor(0)
	c.took(rowsOf(300, 299, 298), 3, now)
	if q := c.query(store.RequestQuery{}); q.After != 298 || !slices.Equal(q.Skip, []int64{300, 299}) {
		t.Errorf("After = %d, Skip = %v; want 298 and the two newer rows", q.After, q.Skip)
	}
}
