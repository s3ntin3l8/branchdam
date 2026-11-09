// Package clock is the one clock the server reads. In production it is the
// system clock; a demo instance can freeze it (config key clock.fixed) so every
// timestamp the server writes or shows, and every UUIDv7 it generates, is the
// same on every run.
//
// Two readings, and which one a caller wants depends on what it does with the
// result:
//
//   - Now / Unix: a point in time that is stored, compared with a stored time,
//     or shown. This is what SQLite's unixepoch() returns too (internal/db
//     registers it from here), so a row written in Go and one written by a
//     DEFAULT agree.
//   - Real: the system clock, always. For waits and measurements that must keep
//     advancing while Now is frozen: lock timeouts, connection deadlines,
//     request durations, throttle windows, nonces.
package clock

import (
	"encoding/binary"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// frozen is the instant Now returns, or nil for the system clock.
var frozen atomic.Pointer[time.Time]

// counter makes the frozen UUIDv7s distinct and ordered.
var counter atomic.Uint64

// Freeze fixes Now at t. Call it once at startup, before anything reads the
// clock; there is no way to move a frozen clock on, by design. Freezing again
// starts the frozen UUIDv7s over, so a second Freeze to the same instant
// re-issues values it already handed out (tests only).
func Freeze(t time.Time) {
	t = t.UTC()
	frozen.Store(&t)
	counter.Store(0)
}

// Unfreeze returns Now to the system clock (tests).
func Unfreeze() { frozen.Store(nil) }

// Frozen reports whether Now is fixed.
func Frozen() bool { return frozen.Load() != nil }

// Now is the current time: the frozen instant, or the system clock.
func Now() time.Time {
	if t := frozen.Load(); t != nil {
		return *t
	}
	return time.Now()
}

// Unix is Now in whole seconds, the unit of every timestamp column.
func Unix() int64 { return Now().Unix() }

// Real is the system clock, whatever Freeze did. Use it for deadlines, timeouts
// and elapsed time (time.Since(start) of a Real reading), never for a value
// that is stored or shown.
func Real() time.Time { return time.Now() }

// NewV7 is uuid.NewV7 on the clock: the system clock and random bits normally.
// While the clock is frozen the millisecond field is the frozen instant and the
// 74 random bits are a counter, so the n-th UUID of a run is the same on every
// run (a run's insert order is deterministic) and they still sort in order.
func NewV7() (uuid.UUID, error) {
	t := frozen.Load()
	if t == nil {
		return uuid.NewV7()
	}
	n := counter.Add(1) - 1
	var u uuid.UUID
	ms := uint64(t.UnixMilli())
	u[0], u[1], u[2], u[3], u[4], u[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	// rand_a (12 bits): the counter's high bits; rand_b (62 bits): its low bits.
	binary.BigEndian.PutUint16(u[6:8], 0x7000|uint16((n>>48)&0x0fff))
	binary.BigEndian.PutUint64(u[8:16], 0x8000000000000000|(n&0x3fffffffffffffff))
	return u, nil
}
