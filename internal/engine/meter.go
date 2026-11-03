package engine

import (
	"slices"
	"time"
)

// ProgressInterval is how often engines report Progress while downloading.
const ProgressInterval = 333 * time.Millisecond

// SpeedWindow is how much recent history a Meter measures speed over. It is
// several progress intervals long, so frequent reports don't make the speed
// jumpy.
const SpeedWindow = 3 * time.Second

// Meter measures transfer speed as the bytes moved over about the last
// SpeedWindow of samples. Until it has that much history it measures over
// what it has. Use NewMeter; a Meter is not safe for concurrent use.
type Meter struct {
	samples []sample // oldest first, never empty
}

type sample struct {
	at    time.Time
	bytes int64
}

// NewMeter returns a Meter whose first sample is bytes at time at, such as
// the bytes already on disk when a run starts.
func NewMeter(at time.Time, bytes int64) *Meter {
	return &Meter{samples: []sample{{at: at, bytes: bytes}}}
}

// Add records that bytes had been transferred by time at and returns the
// speed in bytes per second. The speed is never negative: when bytes go
// down, as when torrent pieces fail verification, the history drops by the
// same amount, so the drop reads as no progress rather than as negative
// speed.
func (m *Meter) Add(at time.Time, bytes int64) int64 {
	last := m.samples[len(m.samples)-1]
	if at.Before(last.at) {
		at = last.at
	}

	if drop := last.bytes - bytes; drop > 0 {
		for i := range m.samples {
			m.samples[i].bytes -= drop
		}
	}

	m.samples = append(m.samples, sample{at: at, bytes: bytes})

	// Drop everything older than the newest sample that is at least
	// SpeedWindow old.
	n := 0
	for n+1 < len(m.samples) && at.Sub(m.samples[n+1].at) >= SpeedWindow {
		n++
	}

	m.samples = slices.Delete(m.samples, 0, n)

	first := m.samples[0]
	elapsed := at.Sub(first.at)
	if elapsed <= 0 {
		return 0
	}

	return max(int64(float64(bytes-first.bytes)/elapsed.Seconds()), 0)
}
