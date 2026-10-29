package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// runScans runs m's scans until the test ends.
func runScans(t *testing.T, m *Manager) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = m.RunScans(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (m *Manager) rescan(rootID, scanID string) error {
	return m.store.Update(context.Background(), func(tx *store.Tx) error {
		return m.Rescan(context.Background(), tx, rootID, scanID)
	})
}

// reports waits for the last report of scanID and returns all its reports
// in the order they were queued.
func reports(t *testing.T, m *Manager, scanID string) []*pb.InventoryReport {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		_, events, _, err := m.store.PendingEvents(context.Background(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}

		var out []*pb.InventoryReport

		for _, ev := range events {
			if r := ev.GetInventoryReport(); r.GetScanId() == scanID {
				out = append(out, r)
			}
		}

		if len(out) > 0 && out[len(out)-1].GetLast() {
			return out
		}

		if time.Now().After(deadline) {
			t.Fatalf("got %d reports for %s and no last one", len(out), scanID)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func paths(rs []*pb.InventoryReport) []string {
	var out []string

	for _, r := range rs {
		for _, e := range r.GetEntries() {
			out = append(out, e.GetRelativePath())
		}
	}

	slices.Sort(out)

	return out
}

func TestScanReportsTheVideoFilesInARoot(t *testing.T) {
	allowed, outside := layout(t)
	m := newManager(t, allowed)

	movies := filepath.Join(allowed, "movies")
	write(t, filepath.Join(movies, "Arrival (2016)", "Arrival (2016).mkv"), "video")
	write(t, filepath.Join(movies, "Arrival (2016)", "Arrival (2016).srt"), "subs")
	write(t, filepath.Join(movies, "Heat (1995)", "Heat (1995).MP4"), "video")
	write(t, filepath.Join(movies, "Heat (1995)", "Sample", "heat.sample.mkv"), "s")
	write(t, filepath.Join(movies, ".trash", "Old.mkv"), "old")
	write(t, filepath.Join(movies, "Heat (1995)", ".Heat (1995).mkv"), "hidden")
	write(t, filepath.Join(movies, "Heat (1995)", "Heat (1995).mkv.nabbed-partial"), "partial")
	write(t, filepath.Join(outside, "secret.mkv"), "secret")
	write(t, filepath.Join(allowed, "tv", "Show.mkv"), "elsewhere inside")

	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.mkv"), filepath.Join(movies, "out.mkv")); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(filepath.Join(allowed, "tv", "Show.mkv"), filepath.Join(movies, "in.mkv")); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.put(root("movies", movies)); err != nil {
		t.Fatal(err)
	}

	runScans(t, m)

	if err := m.rescan("movies", "scan-1"); err != nil {
		t.Fatal(err)
	}

	rs := reports(t, m, "scan-1")

	want := []string{"Arrival (2016)/Arrival (2016).mkv", "Heat (1995)/Heat (1995).MP4"}
	if runtime.GOOS != "windows" {
		want = append(want, "in.mkv")
	}

	slices.Sort(want)

	if got := paths(rs); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	last := rs[len(rs)-1]
	if last.GetRootId() != "movies" || last.GetIncomplete() {
		t.Fatalf("got %v, want a complete report for movies", last)
	}

	e := rs[0].GetEntries()[0]

	fi, err := os.Stat(filepath.Join(movies, filepath.FromSlash(e.GetRelativePath())))
	if err != nil {
		t.Fatal(err)
	}

	if e.GetRootId() != "movies" || e.GetSize() != uint64(fi.Size()) || e.GetMtimeMs() != fi.ModTime().UnixMilli() {
		t.Fatalf("got %v, want size %d and mtime %d", e, fi.Size(), fi.ModTime().UnixMilli())
	}

	if pending, _ := m.pendingScans(context.Background()); len(pending) != 0 {
		t.Fatalf("got %v pending after the last report, want none", pending)
	}
}

func TestScanSkipsAnotherRootInsideIt(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	write(t, filepath.Join(allowed, "movies", "Heat (1995).mkv"), "video")
	write(t, filepath.Join(allowed, "tv", "Show", "Show - S01E01.mkv"), "video")

	if err := m.put(root("all", allowed)); err != nil {
		t.Fatal(err)
	}

	if err := m.put(root("tv", filepath.Join(allowed, "tv"))); err != nil {
		t.Fatal(err)
	}

	runScans(t, m)

	if err := m.rescan("all", "s-all"); err != nil {
		t.Fatal(err)
	}

	if got := paths(reports(t, m, "s-all")); !slices.Equal(got, []string{"movies/Heat (1995).mkv"}) {
		t.Fatalf("got %v, want the TV root left to its own scan", got)
	}

	if err := m.rescan("tv", "s-tv"); err != nil {
		t.Fatal(err)
	}

	if got := paths(reports(t, m, "s-tv")); !slices.Equal(got, []string{"Show/Show - S01E01.mkv"}) {
		t.Fatalf("got %v", got)
	}
}

func TestScanSplitsLargeInventories(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	movies := filepath.Join(allowed, "movies")
	for i := range 2*reportSize + 1 {
		write(t, filepath.Join(movies, fmt.Sprintf("m%04d.mkv", i)), "v")
	}

	if err := m.put(root("movies", movies)); err != nil {
		t.Fatal(err)
	}

	runScans(t, m)

	if err := m.rescan("movies", "big"); err != nil {
		t.Fatal(err)
	}

	rs := reports(t, m, "big")

	var sizes []int
	for i, r := range rs {
		sizes = append(sizes, len(r.GetEntries()))

		if r.GetLast() != (i == len(rs)-1) || r.GetRootId() != "movies" {
			t.Fatalf("got report %d = last %v root %q", i, r.GetLast(), r.GetRootId())
		}
	}

	if !slices.Equal(sizes, []int{reportSize, reportSize, 1}) {
		t.Fatalf("got report sizes %v", sizes)
	}
}

func TestEmptyRootSendsOneLastReport(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	if err := m.put(root("tv", filepath.Join(allowed, "tv"))); err != nil {
		t.Fatal(err)
	}

	runScans(t, m)

	if err := m.rescan("tv", "empty"); err != nil {
		t.Fatal(err)
	}

	rs := reports(t, m, "empty")
	if len(rs) != 1 || len(rs[0].GetEntries()) != 0 || rs[0].GetRootId() != "tv" || rs[0].GetIncomplete() {
		t.Fatalf("got %v, want one empty, complete last report naming the root", rs)
	}
}

func TestRescanRefusals(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	if err := m.rescan("nope", "s1"); !errors.Is(err, ErrUnknownRoot) {
		t.Fatalf("got %v, want ErrUnknownRoot", err)
	}

	tv := filepath.Join(allowed, "tv")
	if err := m.put(root("tv", tv)); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(tv); err != nil {
		t.Fatal(err)
	}

	if err := m.rescan("tv", "s2"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid for a root that is gone", err)
	}

	if pending, _ := m.pendingScans(context.Background()); len(pending) != 0 {
		t.Fatalf("got %v, want refused scans not recorded", pending)
	}
}

func TestInterruptedScanResumesOnStart(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	write(t, filepath.Join(allowed, "movies", "Heat (1995).mkv"), "video")

	if err := m.put(root("movies", filepath.Join(allowed, "movies"))); err != nil {
		t.Fatal(err)
	}

	// Requested while scans aren't running, as before a restart.
	if err := m.rescan("movies", "before"); err != nil {
		t.Fatal(err)
	}

	runScans(t, m)

	if got := paths(reports(t, m, "before")); !slices.Equal(got, []string{"Heat (1995).mkv"}) {
		t.Fatalf("got %v", got)
	}
}

func TestANewRescanReplacesTheOldOne(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	write(t, filepath.Join(allowed, "movies", "Heat (1995).mkv"), "video")

	if err := m.put(root("movies", filepath.Join(allowed, "movies"))); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"old", "new"} {
		if err := m.rescan("movies", id); err != nil {
			t.Fatal(err)
		}
	}

	runScans(t, m)
	reports(t, m, "new")

	_, events, _, err := m.store.PendingEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	for _, ev := range events {
		if ev.GetInventoryReport().GetScanId() == "old" {
			t.Fatalf("got a report for the replaced scan: %v", ev)
		}
	}
}

func TestRemovedRootIsNotScanned(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	tv := filepath.Join(allowed, "tv")
	if err := m.put(root("tv", tv)); err != nil {
		t.Fatal(err)
	}

	if err := m.rescan("tv", "doomed"); err != nil {
		t.Fatal(err)
	}

	if err := m.store.Update(context.Background(), func(tx *store.Tx) error { return m.Remove(context.Background(), tx, "tv") }); err != nil {
		t.Fatal(err)
	}

	if pending, _ := m.pendingScans(context.Background()); len(pending) != 0 {
		t.Fatalf("got %v, want the scan removed with its root", pending)
	}
}

func TestUnreadableFolderMakesTheScanIncomplete(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}

	allowed, _ := layout(t)
	m := newManager(t, allowed)

	movies := filepath.Join(allowed, "movies")
	write(t, filepath.Join(movies, "Heat (1995).mkv"), "video")
	write(t, filepath.Join(movies, "Locked", "Arrival.mkv"), "video")

	if err := m.put(root("movies", movies)); err != nil {
		t.Fatal(err)
	}

	locked := filepath.Join(movies, "Locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	runScans(t, m)

	if err := m.rescan("movies", "partial"); err != nil {
		t.Fatal(err)
	}

	rs := reports(t, m, "partial")
	if got := paths(rs); !slices.Equal(got, []string{"Heat (1995).mkv"}) || !rs[len(rs)-1].GetIncomplete() {
		t.Fatalf("got %v, incomplete %v; want the readable file and incomplete set", got, rs[len(rs)-1].GetIncomplete())
	}
}
