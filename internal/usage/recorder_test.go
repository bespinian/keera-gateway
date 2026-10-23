package usage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The usage log is what a deployment bills from, so these tests ask what it
// does when it cannot write.
//
// A database that is down must not stop inference, so events are dropped
// rather than block. That is only fine while what is dropped is counted, and
// nothing is dropped unless the buffer is really full. An accepted event was
// already charged against a budget, so the shutdown drain still writes it.

// recorded is a Writer that keeps what it was given.
type recorded struct {
	mu     sync.Mutex
	events []store.Event
	// fail, while set, is returned instead of writing: a database that is
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
	// Each of these was already charged against a budget, so dropping it on the
	// way out would bill an organisation for requests its usage report does not
	// show.
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
	// The drain needs a context of its own: on the cancelled one every write
	// would fail at once, which is the same as not draining.
	//
	// The context is checked during the write, not after: Run cancels the drain
	// context on its way out, so a later check would pass on a broken recorder.
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
	// A small buffer and a wedged writer: a database that stopped answering.
	// Record must not block longer than BlockFor, as it runs on the way out of
	// a completion, and a stalled database would become stalled inference.
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
	// Generous: twenty calls that each waited indefinitely would not come back
	// at all.
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
	// BlockFor is a last resort: a recorder whose writer keeps up must not drop
	// anything.
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
	// A refused batch is gone, as there is no retry by design, so it must count
	// as dropped, not as written.
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
	// Otherwise a busy gateway's log would lag a flush interval per batch, and
	// the buffer would fill until it overflowed.
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
	// A quiet deployment's events must not wait in a half-full batch, or a
	// gateway serving one request an hour would have an empty usage report.
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
	// The ticker fires whether or not anything arrived. Writing empty batches
	// would be a query per interval per replica on an idle deployment.
	w := &recorded{}
	_, stop := run(t, w, Options{FlushInterval: time.Millisecond})
	time.Sleep(50 * time.Millisecond)
	stop()

	if got := w.batches(); got != 0 {
		t.Errorf("made %d round trips with nothing to write, want none", got)
	}
}

func TestTheDefaultsAreTheOnesTheCommentsPromise(t *testing.T) {
	// A deployment that tunes nothing gets these, and the buffer is the whole
	// margin between a slow database and lost billing data.
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
	set := Options{Buffer: 1, BatchSize: 2, FlushInterval: time.Second, BlockFor: time.Minute,
		WriteTimeout: time.Hour}
	was := set
	set.setDefaults()
	if set != was {
		t.Errorf("setDefaults changed %+v to %+v", was, set)
	}
}

func TestWaitReturnsOnlyOnceEverythingIsWritten(t *testing.T) {
	// The server calls Wait during shutdown to stay up until the log is
	// flushed. A Wait that returned early would let the process exit mid-drain.
	w := &recorded{}
	r, stop := run(t, w, Options{FlushInterval: time.Hour, BatchSize: 1000})
	for range 500 {
		r.Record(event("a"))
	}
	stop()

	// No polling: everything must be written by the time Wait returns.
	if got := len(w.seen()); got != 500 {
		t.Errorf("Wait returned with %d of 500 events written", got)
	}
}

// Anyone can send requests the gateway refuses, and each is recorded. Their
// events must not crowd out what an organisation pays for.
func TestABilledEventIsNotCrowdedOutByOthers(t *testing.T) {
	w := &recorded{block: make(chan struct{})}
	r, stop := run(t, w, Options{Buffer: 4, BatchSize: 1, BlockFor: time.Millisecond,
		FlushInterval: time.Hour})

	// The first is taken by the writer and held; the rest fill the buffer.
	for range 20 {
		r.Record(event("refused"))
	}
	r.Record(store.Event{OrgID: "org_1", Alias: "billed",
		Bills: []policy.BillLine{{CreditMicros: 1}}})
	close(w.block)
	stop()

	seen := w.seen()
	if len(seen) < 2 || seen[1].Alias != "billed" {
		t.Errorf("written = %v, want the billed event right after the one in flight", seen)
	}
}
