package control

import "sync/atomic"

// atomic64 is a nanosecond timestamp updated atomically. It has a type of its
// own so the copy loops in pipe read as "bytes moved now", not as arithmetic
// on a shared integer.
type atomic64 struct{ v atomic.Int64 }

func (a *atomic64) set(ns int64) { a.v.Store(ns) }
func (a *atomic64) get() int64   { return a.v.Load() }
