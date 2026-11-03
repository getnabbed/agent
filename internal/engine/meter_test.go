package engine

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// tick is the time of the i-th progress tick after t0.
func tick(i int) time.Time { return t0.Add(time.Duration(i) * ProgressInterval) }

// near reports whether got is within 0.1% of want.
func near(got, want int64) bool {
	d := float64(got - want)

	return d >= -0.001*float64(want) && d <= 0.001*float64(want)
}

func TestMeterSteadyRate(t *testing.T) {
	const rate = 1 << 20

	m := NewMeter(t0, 0)

	for i := 1; i <= 30; i++ {
		at := tick(i)
		bytes := int64(rate * at.Sub(t0).Seconds())

		if got := m.Add(at, bytes); !near(got, rate) {
			t.Fatalf("tick %d: speed = %d, want %d", i, got, rate)
		}
	}
}

func TestMeterSmoothsLumpyArrivals(t *testing.T) {
	// 1 MiB lands just after each whole second, so most ticks see nothing
	// and some see a whole MiB. Measured over one tick, the speed would
	// swing between 0 and 3 MiB/s.
	const chunk = 1 << 20

	m := NewMeter(t0, 0)

	for i := 1; i <= 60; i++ {
		at := tick(i)
		bytes := int64(at.Sub(t0.Add(100*time.Millisecond))/time.Second+1) * chunk
		got := m.Add(at, bytes)

		if at.Sub(t0) < SpeedWindow+time.Second {
			continue // warming up
		}

		if got < chunk*85/100 || got > chunk*140/100 {
			t.Fatalf("tick %d: speed = %d, want within 0.85 and 1.4 MiB/s", i, got)
		}
	}
}

func TestMeterBurstThenIdle(t *testing.T) {
	const rate = 10 << 20

	m := NewMeter(t0, 0)

	var bytes int64

	burst := 9 // ticks, about 3 s
	for i := 1; i <= burst; i++ {
		bytes = int64(rate * tick(i).Sub(t0).Seconds())
		m.Add(tick(i), bytes)
	}

	prev := m.Add(tick(burst+1), bytes)
	if prev < rate/2 {
		t.Fatalf("first idle tick: speed = %d, want it to fall gradually from %d", prev, rate)
	}

	for i := burst + 2; i <= burst+20; i++ {
		got := m.Add(tick(i), bytes)
		if got > prev {
			t.Fatalf("tick %d: speed rose to %d from %d while idle", i, got, prev)
		}

		prev = got

		idle := tick(i).Sub(tick(burst))
		if idle >= SpeedWindow+ProgressInterval && got != 0 {
			t.Fatalf("idle for %s: speed = %d, want 0", idle, got)
		}
	}
}

func TestMeterStartup(t *testing.T) {
	// The bytes already there when the run starts don't count.
	m := NewMeter(t0, 5<<30)

	if got := m.Add(t0, 5<<30+100); got != 0 {
		t.Fatalf("no time elapsed: speed = %d, want 0", got)
	}

	// Before a full window, the speed is over what history there is.
	if got := m.Add(tick(1), 5<<30+333_000); !near(got, 1_000_000) {
		t.Fatalf("after one tick: speed = %d, want 1000000", got)
	}

	if got := m.Add(tick(3), 5<<30+999_000); !near(got, 1_000_000) {
		t.Fatalf("after three ticks: speed = %d, want 1000000", got)
	}
}

func TestMeterBytesGoingBackwards(t *testing.T) {
	const rate = 1 << 20

	m := NewMeter(t0, 0)

	for i := 1; i <= 12; i++ {
		m.Add(tick(i), int64(rate*tick(i).Sub(t0).Seconds()))
	}

	// Pieces failing verification take 2 MiB back off.
	lost := int64(2 << 20)
	at := tick(13)

	got := m.Add(at, int64(rate*at.Sub(t0).Seconds())-lost)
	if got < 0 || got > rate {
		t.Fatalf("after the drop: speed = %d, want between 0 and %d", got, rate)
	}

	// Once bytes grow again the speed recovers, without reading the lost
	// bytes as a negative rate.
	for i := 14; i <= 30; i++ {
		at := tick(i)

		got := m.Add(at, int64(rate*at.Sub(t0).Seconds())-lost)
		if got < 0 {
			t.Fatalf("tick %d: speed = %d, want >= 0", i, got)
		}

		if i >= 13+int(SpeedWindow/ProgressInterval)+1 && !near(got, rate) {
			t.Fatalf("tick %d: speed = %d, want %d", i, got, rate)
		}
	}
}

func TestMeterTimeGoingBackwards(t *testing.T) {
	m := NewMeter(t0, 0)
	m.Add(tick(3), 1000)

	if got := m.Add(tick(1), 2000); got < 0 {
		t.Fatalf("speed = %d, want >= 0", got)
	}

	if got := m.Add(tick(6), 3000); got < 0 {
		t.Fatalf("speed = %d, want >= 0", got)
	}
}
