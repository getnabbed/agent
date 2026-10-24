package source

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/engine/usenet"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// run starts the manager's reporter for the test.
func (f *fixture) run(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = f.m.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// healthEvents waits until the outbox holds want SourceHealth events and
// returns them.
func (f *fixture) healthEvents(t *testing.T, want int) []*pb.SourceHealth {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		_, evs, _, err := f.store.PendingEvents(context.Background(), 0, 100)
		if err != nil {
			t.Fatal(err)
		}

		var out []*pb.SourceHealth
		for _, ev := range evs {
			if h := ev.GetSourceHealth(); h != nil {
				out = append(out, h)
			}
		}

		if len(out) >= want || time.Now().After(deadline) {
			if len(out) != want {
				t.Fatalf("got %d source health events %v, want %d", len(out), out, want)
			}

			return out
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestAppliedSourceIsCheckedAndReported(t *testing.T) {
	f := newFixture(t)
	f.run(t)

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil {
		t.Fatal(err)
	}

	evs := f.healthEvents(t, 1)
	if evs[0].GetSourceId() != "src-1" || !evs[0].GetHealthy() || evs[0].GetDetail() != "" {
		t.Fatalf("got %v, want src-1 healthy", evs[0])
	}

	// An unchanged PutSource keeps the pool, so there is no new check and
	// no new event.
	if err := f.put(f.source(t, "src-1", validConfig())); err == nil {
		time.Sleep(50 * time.Millisecond)
		f.healthEvents(t, 1)
	}
}

func TestHealthChangesAreReportedOnce(t *testing.T) {
	f := newFixture(t)
	f.engine.setCheckErr(usenet.ErrAuth)
	f.run(t)

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil {
		t.Fatal(err)
	}

	evs := f.healthEvents(t, 1)
	if evs[0].GetHealthy() || !strings.Contains(evs[0].GetDetail(), "username or password") {
		t.Fatalf("got %v, want unhealthy with the reason", evs[0])
	}

	// The same failure again is not a change.
	_ = f.m.Test(context.Background(), "src-1")
	time.Sleep(50 * time.Millisecond)
	f.healthEvents(t, 1)

	// Recovery is.
	f.engine.setCheckErr(nil)

	if err := f.m.Test(context.Background(), "src-1"); err != nil {
		t.Fatal(err)
	}

	if evs := f.healthEvents(t, 2); !evs[1].GetHealthy() {
		t.Fatalf("got %v, want the recovery reported", evs[1])
	}
}

func TestTestSourceAnswers(t *testing.T) {
	f := newFixture(t)

	if err := f.m.Test(context.Background(), "nope"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("got %v for an unknown source", err)
	}

	off := f.source(t, "src-2", validConfig())
	off.Enabled = false

	if err := f.put(off); err != nil {
		t.Fatal(err)
	}

	if err := f.m.Test(context.Background(), "src-2"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("got %v for a disabled source", err)
	}

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil {
		t.Fatal(err)
	}

	f.engine.setCheckErr(usenet.ErrAuth)

	err := f.m.Test(context.Background(), "src-1")
	if err == nil || !strings.Contains(err.Error(), "username or password") {
		t.Fatalf("got %v, want the reason the test failed", err)
	}
}

func TestLocalProvidersAreNotReported(t *testing.T) {
	f := newFixture(t)
	f.run(t)

	// A provider from NABBED_NNTP lives in the same engine.
	f.engine.mu.Lock()
	f.engine.providers["local:0"] = usenet.Provider{}
	f.engine.mu.Unlock()

	_ = f.engine.Check(context.Background(), "local:0")

	time.Sleep(50 * time.Millisecond)
	f.healthEvents(t, 0)
}

func (f *fakeEngine) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.checks)
}

func TestFailingSourceIsCheckedAgainUntilItRecovers(t *testing.T) {
	f := newFixture(t)
	f.m.recheck.base, f.m.recheck.max, f.m.recheck.tick = 10*time.Millisecond, 40*time.Millisecond, 5*time.Millisecond
	f.engine.setCheckErr(usenet.ErrAuth)
	f.run(t)

	if err := f.put(f.source(t, "src-1", validConfig())); err != nil {
		t.Fatal(err)
	}

	f.healthEvents(t, 1)

	deadline := time.Now().Add(5 * time.Second)
	for f.engine.checkCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if n := f.engine.checkCount(); n < 3 {
		t.Fatalf("got %d checks of a failing source, want it checked again", n)
	}

	// Still one event: failing again is not news.
	f.healthEvents(t, 1)

	f.engine.setCheckErr(nil)

	if evs := f.healthEvents(t, 2); !evs[1].GetHealthy() {
		t.Fatalf("got %v, want recovery reported by a background check", evs[1])
	}

	// Healthy sources are not polled.
	n := f.engine.checkCount()
	time.Sleep(100 * time.Millisecond)

	if got := f.engine.checkCount(); got != n {
		t.Fatalf("got %d more checks of a healthy source, want none", got-n)
	}
}
