package hoststats

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/getnabbed/agent/internal/notice"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const gb = 1_000_000_000

var quiet = slog.New(slog.DiscardHandler)

// fakeDisks returns injected usage per path.
type fakeDisks struct {
	mu sync.Mutex
	u  map[string]Usage
}

func (f *fakeDisks) set(path string, free, total uint64) {
	f.mu.Lock()
	f.u[path] = Usage{Free: free, Total: total}
	f.mu.Unlock()
}

func (f *fakeDisks) usage(path string) (Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	u, ok := f.u[path]
	if !ok {
		return Usage{}, errors.New("no such folder")
	}

	return u, nil
}

type fakeRoots []*pb.LibraryRoot

func (r fakeRoots) Roots(context.Context) ([]*pb.LibraryRoot, error) { return r, nil }

type fakeTelemetry struct {
	mu   sync.Mutex
	sent []*pb.Telemetry
}

func (f *fakeTelemetry) SendTelemetry(key string, t *pb.Telemetry) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if key == TelemetryKey {
		f.sent = append(f.sent, t)
	}
}

func (f *fakeTelemetry) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.sent)
}

type fixture struct {
	st    *store.Store
	disks *fakeDisks
	rep   *notice.Reporter
	mon   *Monitor
}

func newFixture(t *testing.T, roots fakeRoots) *fixture {
	t.Helper()

	ctx := context.Background()

	st, err := store.Open(ctx, t.TempDir(), "agent-1", quiet)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	rep, err := notice.New(ctx, notice.Config{Store: st, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}

	disks := &fakeDisks{u: map[string]Usage{}}

	mon, err := New(ctx, Config{Store: st, DownloadsDir: "/dl", Roots: roots, Notifier: rep, Usage: disks.usage, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{st: st, disks: disks, rep: rep, mon: mon}
}

func (f *fixture) open(t *testing.T, subject string) bool {
	t.Helper()

	open, err := f.rep.Open(context.Background(), notice.CodeDiskLow, subject)
	if err != nil {
		t.Fatal(err)
	}

	return open
}

func TestDiskLowThresholdsAndHysteresis(t *testing.T) {
	steps := []struct {
		name        string
		free, total uint64
		want        bool
	}{
		{"plenty", 500 * gb, 1000 * gb, false},
		{"under 5%", 40 * gb, 1000 * gb, true},
		{"between 5% and 8% stays low", 60 * gb, 1000 * gb, true},
		{"above 8% and 15 GB clears", 90 * gb, 1000 * gb, false},
		{"between again stays clear", 60 * gb, 1000 * gb, false},
		{"under 10 GB on a small disk", 9 * gb, 100 * gb, true},
		{"above 8% but under 15 GB stays low", 14 * gb, 100 * gb, true},
		{"above 15 GB clears", 16 * gb, 100 * gb, false},
	}

	f := newFixture(t, nil)

	for _, s := range steps {
		f.disks.set("/dl", s.free, s.total)
		f.mon.Check(context.Background())

		if got := f.open(t, "/dl"); got != s.want {
			t.Fatalf("%s: got disk.low open = %v, want %v", s.name, got, s.want)
		}
	}
}

func TestDiskLowPerRoot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fakeRoots{{Id: "movies", Path: "/media/movies"}, {Id: "tv", Path: "/media/tv"}})

	f.disks.set("/dl", 500*gb, 1000*gb)
	f.disks.set("/media/movies", 1*gb, 1000*gb)
	f.disks.set("/media/tv", 500*gb, 1000*gb)

	hs := f.mon.Check(ctx)

	if !f.open(t, "movies") || f.open(t, "tv") || f.open(t, "/dl") {
		t.Fatalf("got movies/tv/downloads open = %v/%v/%v, want true/false/false", f.open(t, "movies"), f.open(t, "tv"), f.open(t, "/dl"))
	}

	if len(hs.GetDiskFree()) != 3 {
		t.Fatalf("got %d disk entries, want 3", len(hs.GetDiskFree()))
	}

	dl := hs.GetDiskFree()[0]
	if !dl.GetDownloads() || dl.GetRootId() != "" || dl.GetFreeBytes() != 500*gb || dl.GetTotalBytes() != 1000*gb {
		t.Fatalf("got downloads entry %v, want downloads set, no root id, 500/1000 GB", dl)
	}

	if mv := hs.GetDiskFree()[1]; mv.GetDownloads() || mv.GetRootId() != "movies" || mv.GetFreeBytes() != 1*gb {
		t.Fatalf("got movies entry %v, want root movies with 1 GB free", mv)
	}

	if hs.CpuPercent != nil || hs.MemUsedBytes != nil {
		t.Fatalf("got cpu or memory set, want them unset")
	}

	// The root goes away: its notice resolves.
	f.mon.cfg.Roots = fakeRoots{{Id: "tv", Path: "/media/tv"}}
	f.mon.Check(ctx)

	if f.open(t, "movies") {
		t.Fatalf("got disk.low open for a removed root, want resolved")
	}
}

func TestTelemetryFollowsTheSetting(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	f.disks.set("/dl", 500*gb, 1000*gb)

	tel := &fakeTelemetry{}
	f.mon.SetTelemetry(tel)

	f.mon.tick(ctx)

	if tel.count() != 0 {
		t.Fatalf("got %d HostStats with host_stats off, want 0", tel.count())
	}

	set := func(on bool) {
		t.Helper()

		if err := f.st.Update(ctx, func(tx *store.Tx) error { return f.mon.SetHostStats(ctx, tx, on) }); err != nil {
			t.Fatal(err)
		}
	}

	set(true)
	f.mon.tick(ctx)

	if tel.count() != 1 || len(tel.sent[0].GetHostStats().GetDiskFree()) != 1 {
		t.Fatalf("got %d HostStats with host_stats on, want 1 with the downloads folder", tel.count())
	}

	// The setting survives a restart.
	again, err := New(ctx, Config{Store: f.st, DownloadsDir: "/dl", Usage: f.disks.usage, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}

	if !again.HostStats() {
		t.Fatalf("got host_stats off after a restart, want on")
	}

	set(false)
	f.mon.tick(ctx)

	if tel.count() != 1 {
		t.Fatalf("got %d HostStats after turning host_stats off, want still 1", tel.count())
	}
}

func TestBytesString(t *testing.T) {
	for n, want := range map[uint64]string{999: "999 B", 1500: "1.5 kB", 9_500_000_000: "9.5 GB"} {
		if got := bytesString(n); got != want {
			t.Fatalf("got bytesString(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDiskUsageOfARealFolder(t *testing.T) {
	u, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if u.Total == 0 || u.Free > u.Total {
		t.Fatalf("got %+v, want a non-empty disk with free <= total", u)
	}
}
