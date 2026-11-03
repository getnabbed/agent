package download

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// newProgressManager returns a Manager with only what progress batching
// needs, and the sink its telemetry goes to.
func newProgressManager() (*Manager, *telemetrySink) {
	m := &Manager{progressEvery: engine.ProgressInterval}
	sink := &telemetrySink{}
	m.SetTelemetry(sink)

	return m, sink
}

// batches returns the downloads in each batch sent since the last call,
// failing if anything was sent under another key or of another kind.
func batches(t *testing.T, sink *telemetrySink) [][]*pb.DownloadProgress {
	t.Helper()

	var out [][]*pb.DownloadProgress

	for _, s := range sink.take() {
		b := s.t.GetDownloadProgressBatch()
		if s.key != ProgressTelemetryKey || b == nil {
			t.Fatalf("sent %v under key %q, want a DownloadProgressBatch under %q", s.t, s.key, ProgressTelemetryKey)
		}

		out = append(out, b.GetDownloads())
	}

	return out
}

// ids returns the download ids in one batch.
func ids(batch []*pb.DownloadProgress) []string {
	var out []string
	for _, p := range batch {
		out = append(out, p.GetDownloadId())
	}

	return out
}

// flushOnce flushes and returns the ids in the one batch that must result,
// or nil if nothing was sent.
func flushOnce(t *testing.T, m *Manager, sink *telemetrySink) []string {
	t.Helper()

	m.flushProgress()

	got := batches(t, sink)
	switch len(got) {
	case 0:
		return nil
	case 1:
		return ids(got[0])
	}

	t.Fatalf("one flush sent %d batches, want at most 1", len(got))

	return nil
}

func TestProgressBatchCarriesEveryDownload(t *testing.T) {
	m, sink := newProgressManager()

	m.progress("d2", engine.Progress{Bytes: 50, Total: 200, SpeedBps: 10})
	m.progress("d1", engine.Progress{Bytes: 5, Total: 10, SpeedBps: 1})
	m.progress("d1", engine.Progress{Bytes: 6, Total: 10, SpeedBps: 2})
	m.progress("d3", engine.Progress{Bytes: 7})

	m.flushProgress()

	got := batches(t, sink)
	if len(got) != 1 {
		t.Fatalf("sent %d batches, want 1", len(got))
	}

	want := []*pb.DownloadProgress{
		{DownloadId: "d1", Percent: 60, Bytes: 6, SpeedBps: 2, EtaS: 2},
		{DownloadId: "d2", Percent: 25, Bytes: 50, SpeedBps: 10, EtaS: 15},
		{DownloadId: "d3", Bytes: 7},
	}
	if !slices.EqualFunc(got[0], want, func(a, b *pb.DownloadProgress) bool { return proto.Equal(a, b) }) {
		t.Fatalf("batch = %v, want %v", got[0], want)
	}

	// With nothing new, the next batch still carries every download.
	if got := flushOnce(t, m, sink); !slices.Equal(got, []string{"d1", "d2", "d3"}) {
		t.Fatalf("next batch = %v, want [d1 d2 d3]", got)
	}
}

func TestProgressBatchSendsAnEndedRunOnceMore(t *testing.T) {
	m, sink := newProgressManager()

	m.progress("d1", engine.Progress{Bytes: 10, Total: 10})
	m.progress("d2", engine.Progress{Bytes: 1, Total: 10})
	m.batch.end("d1")

	if got := flushOnce(t, m, sink); !slices.Equal(got, []string{"d1", "d2"}) {
		t.Fatalf("batch after d1 ended = %v, want [d1 d2]", got)
	}

	if got := flushOnce(t, m, sink); !slices.Equal(got, []string{"d2"}) {
		t.Fatalf("batch after that = %v, want [d2]", got)
	}

	m.batch.end("d2")

	if got := flushOnce(t, m, sink); !slices.Equal(got, []string{"d2"}) {
		t.Fatalf("batch after d2 ended = %v, want [d2]", got)
	}

	if got := flushOnce(t, m, sink); got != nil {
		t.Fatalf("batch with nothing running = %v, want none sent", got)
	}
}

func TestProgressBatchKeepsARunThatRestarted(t *testing.T) {
	m, sink := newProgressManager()

	// Paused, then resumed before the next batch went out.
	m.progress("d1", engine.Progress{Bytes: 1, Total: 10})
	m.batch.end("d1")
	m.progress("d1", engine.Progress{Bytes: 2, Total: 10})

	for range 2 {
		if got := flushOnce(t, m, sink); !slices.Equal(got, []string{"d1"}) {
			t.Fatalf("batch = %v, want [d1]", got)
		}
	}
}

func TestProgressBatchSendsNothingWhenIdle(t *testing.T) {
	m, sink := newProgressManager()

	if got := flushOnce(t, m, sink); got != nil {
		t.Fatalf("batch = %v, want none sent", got)
	}

	// A run that ended before reporting anything leaves nothing to send.
	m.batch.end("d1")

	if got := flushOnce(t, m, sink); got != nil {
		t.Fatalf("batch = %v, want none sent", got)
	}
}

func TestProgressBatchDropsEndedRunsWithoutTelemetry(t *testing.T) {
	m := &Manager{progressEvery: engine.ProgressInterval}

	m.progress("d1", engine.Progress{Bytes: 1, Total: 10})
	m.batch.end("d1")
	m.flushProgress()

	sink := &telemetrySink{}
	m.SetTelemetry(sink)

	if got := flushOnce(t, m, sink); got != nil {
		t.Fatalf("batch = %v, want none sent for a run that ended before telemetry was set", got)
	}
}

func TestProgressIsSentEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, sink := newProgressManager()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		go func() {
			m.sendProgress(ctx)
			close(done)
		}()

		// Step between ticks, so each sleep spans exactly one.
		time.Sleep(engine.ProgressInterval / 2)
		m.progress("d1", engine.Progress{Bytes: 1, Total: 10})

		for range 3 {
			time.Sleep(engine.ProgressInterval)
			synctest.Wait()

			if got := batches(t, sink); len(got) != 1 || !slices.Equal(ids(got[0]), []string{"d1"}) {
				t.Fatalf("one interval sent %v, want one batch of [d1]", got)
			}
		}

		m.batch.end("d1")
		time.Sleep(10 * engine.ProgressInterval)
		synctest.Wait()

		if got := batches(t, sink); len(got) != 1 {
			t.Fatalf("after d1 ended, sent %d batches, want its final one only", len(got))
		}

		cancel()
		<-done
	})
}

func TestEngineRunEndSendsItsProgressOnceMore(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(h *harness)
	}{
		{"done", func(h *harness) {
			h.eng.release <- nil
			h.waitState("d1", Done)
		}},
		{"failed", func(h *harness) {
			h.eng.release <- engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false, "dead torrent")
			h.waitState("d1", Failed)
		}},
		{"paused", func(h *harness) {
			if err := h.act(Pause, "d1"); err != nil {
				h.t.Fatal(err)
			}

			h.waitStopped("d1")
		}},
		{"cancelled", func(h *harness) {
			if err := h.act(Cancel, "d1"); err != nil {
				h.t.Fatal(err)
			}

			h.waitStopped("d1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 1)

			if err := h.start(torrentDownload("d1")); err != nil {
				t.Fatal(err)
			}

			h.waitStarted()

			for range 2 {
				if got := flushOnce(t, h.m, h.tel); !slices.Equal(got, []string{"d1"}) {
					t.Fatalf("batch while transferring = %v, want [d1]", got)
				}
			}

			tc.stop(h)

			if got := flushOnce(t, h.m, h.tel); !slices.Equal(got, []string{"d1"}) {
				t.Fatalf("batch after the run ended = %v, want [d1]", got)
			}

			if got := flushOnce(t, h.m, h.tel); got != nil {
				t.Fatalf("batch after that = %v, want none sent", got)
			}
		})
	}
}

// waitStopped waits until id has no runner.
func (h *harness) waitStopped(id string) {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		h.m.mu.Lock()
		_, running := h.m.running[id]
		h.m.mu.Unlock()

		if !running {
			return
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("download %s still has a runner", id)
		}

		time.Sleep(5 * time.Millisecond)
	}
}
