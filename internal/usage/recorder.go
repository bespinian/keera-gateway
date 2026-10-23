// Package usage buffers the gateway's usage events and writes them in batches.
package usage

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/store"
)

// Options tunes the batching.
type Options struct {
	// Buffer is how many events may be queued before writers start blocking,
	// in each of the two queues: billed events and the rest.
	Buffer int
	// BatchSize is the largest batch sent in one round trip.
	BatchSize int
	// FlushInterval bounds how long an event waits for a batch to fill.
	FlushInterval time.Duration
	// BlockFor is how long a request waits for buffer space before its event
	// is dropped. Short, so a stalled database does not stall inference.
	BlockFor time.Duration
	// WriteTimeout bounds one batch write. Without it a connection that hangs
	// stops the only writer until TCP gives up, which can take minutes.
	WriteTimeout time.Duration
}

func (o *Options) setDefaults() {
	if o.Buffer <= 0 {
		o.Buffer = 8192
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 256
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 250 * time.Millisecond
	}
	if o.BlockFor <= 0 {
		o.BlockFor = 100 * time.Millisecond
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
}

// Writer is where a finished batch goes. It is an interface so the batching
// can be tested without a database.
type Writer interface {
	WriteEvents(ctx context.Context, events []store.Event) error
}

// Recorder is the write side of the usage log. Events are buffered and written
// in batches, so a completion does not wait for the database. If the database
// is away longer than the buffer holds, events are dropped, logged and counted
// in Dropped rather than blocking inference.
//
// Billed events have their own queue, written first, so the events of refused
// requests, which anyone can send, cannot crowd out what an organisation pays
// for.
type Recorder struct {
	st   Writer
	log  *slog.Logger
	opts Options

	ch      chan store.Event
	billed  chan store.Event
	dropped atomic.Int64
	// failing is set while the last write failed. The buffer will not drain
	// soon then, so a request does not wait for space that is not coming.
	failing atomic.Bool
	written atomic.Int64
	done    chan struct{}
	// batch collects events until it is full or flushed. Only Run touches it.
	batch []store.Event
}

// NewRecorder builds a recorder. Call Run to start it.
func NewRecorder(st Writer, opts Options, log *slog.Logger) *Recorder {
	opts.setDefaults()
	return &Recorder{
		st:     st,
		log:    log,
		opts:   opts,
		ch:     make(chan store.Event, opts.Buffer),
		billed: make(chan store.Event, opts.Buffer),
		done:   make(chan struct{}),
		batch:  make([]store.Event, 0, opts.BatchSize),
	}
}

// Record queues one event.
func (r *Recorder) Record(e store.Event) {
	ch := r.ch
	if len(e.Bills) > 0 {
		ch = r.billed
	}
	select {
	case ch <- e:
		return
	default:
	}
	if r.failing.Load() {
		r.drop()
		return
	}
	t := time.NewTimer(r.opts.BlockFor)
	defer t.Stop()
	select {
	case ch <- e:
	case <-t.C:
		r.drop()
	}
}

func (r *Recorder) drop() {
	if n := r.dropped.Add(1); n == 1 || n%1000 == 0 {
		r.log.Error("usage events are being dropped; billing data is incomplete",
			"dropped_total", n)
	}
}

// Dropped reports how many events were lost.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// Written reports how many events reached the database.
func (r *Recorder) Written() int64 { return r.written.Load() }

// Run writes batches until ctx is cancelled, then drains what is queued.
func (r *Recorder) Run(ctx context.Context) {
	defer close(r.done)
	t := time.NewTicker(r.opts.FlushInterval)
	defer t.Stop()

	for {
		select {
		case e := <-r.billed:
			r.add(ctx, e)
			continue
		default:
		}
		select {
		case e := <-r.billed:
			r.add(ctx, e)
		case e := <-r.ch:
			r.add(ctx, e)
		case <-t.C:
			r.flush(ctx)
		case <-ctx.Done():
			r.drain(ctx)
			return
		}
	}
}

// drain writes what is still queued. It gets its own context: the queued
// events were already charged against budgets and are owed to the usage log.
func (r *Recorder) drain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for {
		select {
		case e := <-r.billed:
			r.add(ctx, e)
		case e := <-r.ch:
			r.add(ctx, e)
		default:
			r.flush(ctx)
			return
		}
	}
}

func (r *Recorder) add(ctx context.Context, e store.Event) {
	r.batch = append(r.batch, e)
	if len(r.batch) >= r.opts.BatchSize {
		r.flush(ctx)
	}
}

func (r *Recorder) flush(ctx context.Context) {
	n := len(r.batch)
	if n == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.WriteTimeout)
	defer cancel()
	// A failed batch is not retried: the write may have landed before the
	// error, and a second copy would bill the requests twice.
	err := r.st.WriteEvents(ctx, r.batch)
	lost, partly := errors.AsType[*store.LostEvents](err)
	switch {
	case partly:
		// The rest were written, so the database is fine.
		r.log.Error("usage events were refused by the database", "error", err,
			"events", lost.N)
		r.dropped.Add(int64(lost.N))
		r.written.Add(int64(n - lost.N))
		r.failing.Store(false)
	case err != nil:
		r.log.Error("writing usage events failed", "error", err, "events", n)
		r.dropped.Add(int64(n))
		r.failing.Store(true)
	default:
		r.written.Add(int64(n))
		r.failing.Store(false)
	}
	r.batch = r.batch[:0]
}

// Wait blocks until Run has finished draining.
func (r *Recorder) Wait() { <-r.done }
