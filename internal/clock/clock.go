// Package clock provides a wall clock corrected by an offset learned from the
// control plane, so signed timestamps stay inside the server's freshness
// window even when the host clock is wrong.
package clock

import (
	"sync/atomic"
	"time"
)

// Clock is a wall clock with an adjustable offset. The zero value is ready to
// use and behaves like time.Now.
type Clock struct {
	offset atomic.Int64 // nanoseconds to add to the local clock
	now    func() time.Time
}

// New returns a Clock backed by time.Now.
func New() *Clock { return &Clock{} }

// NewFixed returns a Clock backed by now, for tests.
func NewFixed(now func() time.Time) *Clock { return &Clock{now: now} }

// Local returns the uncorrected local time.
func (c *Clock) Local() time.Time {
	if c.now != nil {
		return c.now()
	}

	return time.Now()
}

// Now returns the local time corrected by the learned offset.
func (c *Clock) Now() time.Time {
	return c.Local().Add(time.Duration(c.offset.Load()))
}

// Offset returns the current correction.
func (c *Clock) Offset() time.Duration { return time.Duration(c.offset.Load()) }

// Observe records a server timestamp seen at local time localAt, and updates
// the offset to server - local. The newest observation wins; heartbeat round
// trips are short relative to the tolerances this guards.
func (c *Clock) Observe(server, localAt time.Time) time.Duration {
	d := server.Sub(localAt)
	c.offset.Store(int64(d))

	return d
}
