package usage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// The usage log is what a deployment bills from, so the question these ask is
// not "does it write rows" but "what does it do when it cannot".
//
// The trade this package makes is stated in its doc comment: a database that
// has gone away must never become inference that has gone away, so events are
// dropped rather than allowed to block. That is only a defensible trade while
// two things hold - that what is dropped is counted, and that nothing is
// dropped for any reason other than the buffer genuinely being full. The
// shutdown drain is the second half of it: an event that has been accepted has
// already been charged against a budget, so it is owed to the log.

// recorded is a Writer that keeps what it was given.
type recorded struct {
	mu     sync.Mutex
	events []store.Event
	// fail, while set, is returned instead of writing - a database that is
	// unreachable rather than slow.
	fail error
	// block, while non-nil, holds a write until it is closed.
	block chan struct{}
	calls int
}

func (r *recorded) WriteEvents(_ context.Context, events []store.Event) error {
	r.mu.Lock()
	block, fail := r.block, r.fail
	r.calls++
	r.mu.Unlock()

	if block != nil {
		<-block
	}
	if fail != nil {
		return fail
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	return nil
}

func (r *recorded) seen() []store.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.Event(nil), r.events...)
}

func (r *recorded) batches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// run starts a recorder and returns it with the function that stops it and
// waits for the drain to finish.
func run(t *testing.T, w Writer, opts Options) (*Recorder, func()) {
	t.Helper()
	r := NewRecorder(w, opts, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	stop := sync.OnceFunc(func() {
		cancel()
		r.Wait()
	})
	t.Cleanup(stop)
	return r, stop
}

func event(id string) store.Event {
	return store.Event{OrgID: "org_1", Alias: id, InputTokens: 1}
}

func TestEverythingAcceptedBeforeShutdownIsStillWritten(t *testing.T) {
	// This is the one that matters. Each of these has already been charged
	// against a budget by the time it gets here, so dropping it on the way out
	// would bill an organisation for requests its usage report does not show -
	// a discrepancy nobody would find, because the two numbers are produced by
	// different code paths and only ever compared by a customer.
	w := &recorded{}
	// A flush interval longer than the test, so the only thing that can write
	// these is the drain itself.
	r, stop := run(t, w, Options{FlushInterval: time.Hour, BatchSize: 1000})

	for i := range 200 {
		r.Record(event(string(rune('a' + i%26))))
	}
	stop()

	if got := len(w.seen()); got != 200 {
		t.Errorf("wrote %d events, want all 200 that were accepted", got)
	}
	if got := r.Written(); got != 200 {
		t.Errorf("Written = %d, want 200", got)
	}
	if got := r.Dropped(); got != 0 {
		t.Errorf("Dropped = %d, want none: nothing here was refused", got)
	}
}

func TestTheDrainKeepsWritingAfterTheRequestContextIsGone(t *testing.T) {
	// The drain runs on a context of its own, detached from the cancelled one.
	// Reusing the cancelled context would make every write on the way out fail
	// instantly, which is the same bug as not draining at all and shows up in
	// the logs as a database error rather than as a lost row.
	//
	// The state of the context is taken during the write and not afterwards:
	// Run cancels the drain context on its way out, so by the time Wait has
	// returned every context it used is cancelled and the check would pass on
	// a recorder that had got this wrong.
	w := &contextWriter{}
	r, stop := run(t, w, Options{FlushInterval: time.Hour})
	r.Record(event("a"))
	stop()

	wrote, deadline, err := w.atWrite()
	if !wrote {
		t.Fatal("the drain never wrote anything")
	}
	if err != nil {
		t.Errorf("the drain wrote on an already-cancelled context: %v", err)
	}
	if !deadline {
		t.Error("the drain has no deadline; a database that hangs would hang shutdown")
	}
}

// contextWriter reports the state of the context a write was made on, as it was
// at the moment of the write.
type contextWriter struct {
	mu       sync.Mutex
	err      error
	deadline bool
	wrote    bool
}

func (c *contextWriter) WriteEvents(ctx context.Context, _ []store.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = ctx.Err()
	_, c.deadline = ctx.Deadline()
	c.wrote = true
	return nil
}

func (c *contextWriter) atWrite() (wrote, deadline bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wrote, c.deadline, c.err
}

func TestAFullBufferDropsAndCountsRatherThanBlockingInference(t *testing.T) {
	// The buffer is small and the writer is wedged, so this is a database that
	// has stopped answering. What must not happen is Record blocking for longer
	// than BlockFor: it is called on the way out of a completion, so a stalled
	// database would otherwise become stalled inference.
	w := &recorded{block: make(chan struct{})}
	defer close(w.block)

	r, _ := run(t, w, Options{
		Buffer: 2, BatchSize: 1, FlushInterval: time.Millisecond,
		BlockFor: 20 * time.Millisecond,
	})

	// Let the loop pick one up and wedge on it, so the buffer is what fills.
	r.Record(event("a"))
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	for range 20 {
		r.Record(event("b"))
	}
	elapsed := time.Since(start)

	if r.Dropped() == 0 {
		t.Error("a wedged database and a full buffer dropped nothing; Record blocked instead")
	}
	// Generous, because this is about the order of magnitude: twenty calls that
	// each waited their turn indefinitely would not come back at all.
	if limit := 20 * 20 * time.Millisecond * 3; elapsed > limit {
		t.Errorf("twenty records took %v, want well under %v", elapsed, limit)
	}
}

// hangWriter is a connection that stopped answering: a write only ends when
// its context does.
type hangWriter struct{}

func (hangWriter) WriteEvents(ctx context.Context, _ []store.Event) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAHungWriteIsGivenUpAfterWriteTimeout(t *testing.T) {
	// Without a bound, one hung connection stops the only writer until TCP
	// gives up, and every request after that waits for buffer space.
	r, _ := run(t, hangWriter{}, Options{
		BatchSize: 1, FlushInterval: time.Millisecond, WriteTimeout: 20 * time.Millisecond,
	})
	r.Record(event("a"))

	deadline := time.Now().Add(2 * time.Second)
	for r.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.Dropped(); got != 1 {
		t.Errorf("Dropped = %d, want the 1 event whose write hung", got)
	}
}

func TestAFullBufferDropsAtOnceWhileWritesAreFailing(t *testing.T) {
	// The buffer will not drain soon while the database refuses writes, so
	// waiting BlockFor for space would only add latency to every request.
	w := &recorded{fail: errors.New("connection refused")}
	r, _ := run(t, w, Options{
		Buffer: 1, BatchSize: 1, FlushInterval: time.Millisecond, BlockFor: time.Minute,
	})

	r.Record(event("a"))
	deadline := time.Now().Add(2 * time.Second)
	for r.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// Wedge the writer on the next event, so the one after fills the buffer.
	block := make(chan struct{})
	defer close(block)
	w.mu.Lock()
	w.block = block
	w.mu.Unlock()
	r.Record(event("b"))
	for w.batches() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.Record(event("c"))

	start := time.Now()
	r.Record(event("d"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Record waited %v for space while writes were failing, want no wait", elapsed)
	}
	if got := r.Dropped(); got != 2 {
		t.Errorf("Dropped = %d, want the failed batch and the event that found no room", got)
	}
}

func TestAnEventIsNeverDroppedWhileThereIsRoomForIt(t *testing.T) {
	// The other side of the drop policy. BlockFor is a last resort, not a
	// timeout on the common path: a recorder with a buffer and a writer that
	// keeps up must not be shedding anything at all.
	w := &recorded{}
	r, stop := run(t, w, Options{Buffer: 4096, BatchSize: 64, FlushInterval: time.Millisecond})

	for range 2000 {
		r.Record(event("a"))
	}
	stop()

	if got := r.Dropped(); got != 0 {
		t.Errorf("Dropped = %d, want none", got)
	}
	if got := len(w.seen()); got != 2000 {
		t.Errorf("wrote %d events, want 2000", got)
	}
}

func TestAFailedWriteIsCountedAsLostAndNotAsWritten(t *testing.T) {
	// A batch the database refused is gone: there is no retry queue, by design.
	// So it has to land in the number that says billing data is incomplete,
	// rather than in the one that says it is not.
	w := &recorded{fail: errors.New("connection refused")}
	r, stop := run(t, w, Options{FlushInterval: time.Millisecond, BatchSize: 10})

	for range 30 {
		r.Record(event("a"))
	}
	stop()

	if got := r.Written(); got != 0 {
		t.Errorf("Written = %d, want 0: every write failed", got)
	}
	if got := r.Dropped(); got != 30 {
		t.Errorf("Dropped = %d, want 30: a refused batch is lost data", got)
	}
}

func TestAFullBatchIsWrittenWithoutWaitingForTheTimer(t *testing.T) {
	// Otherwise a busy gateway's usage log would lag by a flush interval per
	// batch however fast the events arrive, and the buffer would take the
	// difference until it overflowed.
	w := &recorded{}
	r, stop := run(t, w, Options{BatchSize: 10, FlushInterval: time.Hour})

	for range 50 {
		r.Record(event("a"))
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(w.seen()) < 50 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(w.seen()); got != 50 {
		t.Fatalf("wrote %d events without the timer firing, want 50", got)
	}
	// Five batches of ten, not fifty writes of one: the batching is the reason
	// this package exists.
	if got := w.batches(); got != 5 {
		t.Errorf("made %d round trips for 50 events at a batch size of 10, want 5", got)
	}
	stop()
}

func TestAPartialBatchIsWrittenWhenTheTimerFires(t *testing.T) {
	// The other half of the same trade: a quiet deployment's events must not
	// sit in a half-full batch until enough arrive to fill it, or a gateway
	// serving one request an hour would have an empty usage report.
	w := &recorded{}
	r, stop := run(t, w, Options{BatchSize: 1000, FlushInterval: 5 * time.Millisecond})
	r.Record(event("a"))

	deadline := time.Now().Add(2 * time.Second)
	for len(w.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(w.seen()); got != 1 {
		t.Errorf("wrote %d events, want the one that was waiting", got)
	}
	stop()
}

func TestAnIdleRecorderMakesNoRoundTrips(t *testing.T) {
	// The ticker fires whether or not anything arrived. A flush that wrote an
	// empty batch every interval would be a query per interval per replica,
	// forever, on a deployment doing nothing.
	w := &recorded{}
	_, stop := run(t, w, Options{FlushInterval: time.Millisecond})
	time.Sleep(50 * time.Millisecond)
	stop()

	if got := w.batches(); got != 0 {
		t.Errorf("made %d round trips with nothing to write, want none", got)
	}
}

func TestTheDefaultsAreTheOnesTheCommentsPromise(t *testing.T) {
	// They are what a deployment that tunes nothing gets, and the buffer in
	// particular is the whole margin between a slow database and lost billing
	// data.
	var o Options
	o.setDefaults()

	if o.Buffer != 8192 {
		t.Errorf("Buffer = %d, want 8192", o.Buffer)
	}
	if o.BatchSize != 256 {
		t.Errorf("BatchSize = %d, want 256", o.BatchSize)
	}
	if o.FlushInterval != 250*time.Millisecond {
		t.Errorf("FlushInterval = %v, want 250ms", o.FlushInterval)
	}
	if o.BlockFor != 100*time.Millisecond {
		t.Errorf("BlockFor = %v, want 100ms", o.BlockFor)
	}
	if o.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %v, want 10s", o.WriteTimeout)
	}
	// A recorder built with explicit values keeps them.
	set := Options{Buffer: 1, BatchSize: 2, FlushInterval: time.Second, BlockFor: time.Minute,
		WriteTimeout: time.Hour}
	was := set
	set.setDefaults()
	if set != was {
		t.Errorf("setDefaults changed %+v to %+v", was, set)
	}
}

func TestWaitReturnsOnlyOnceEverythingIsWritten(t *testing.T) {
	// serve calls Wait during shutdown to hold the process open until the log
	// is flushed. A Wait that returned early would make the drain pointless,
	// because the process would exit through the middle of it.
	w := &recorded{}
	r, stop := run(t, w, Options{FlushInterval: time.Hour, BatchSize: 1000})
	for range 500 {
		r.Record(event("a"))
	}
	stop()

	// No polling: if Wait means anything, everything is already written by the
	// time it has returned.
	if got := len(w.seen()); got != 500 {
		t.Errorf("Wait returned with %d of 500 events written", got)
	}
}
