package notice

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()

	st, err := store.Open(t.Context(), dir, "agent-1", nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func newReporter(t *testing.T, st *store.Store, clk *fakeClock, limit int) *Reporter {
	t.Helper()

	r, err := New(t.Context(), Config{Store: st, Now: clk.now, Limit: limit})
	if err != nil {
		t.Fatalf("new reporter: %v", err)
	}

	return r
}

// drain returns the notices queued so far and acknowledges them.
func drain(t *testing.T, st *store.Store) []*pb.AgentNotice {
	t.Helper()

	ids, evs, _, err := st.PendingEvents(t.Context(), 0, 1000)
	if err != nil {
		t.Fatalf("pending events: %v", err)
	}

	if _, err := st.AckEvents(t.Context(), ids); err != nil {
		t.Fatalf("ack events: %v", err)
	}

	var out []*pb.AgentNotice
	for _, ev := range evs {
		if n := ev.GetAgentNotice(); n != nil {
			out = append(out, n)
		}
	}

	return out
}

func TestRaiseOnlyOnChangeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}

	st := openStore(t, dir)
	r := newReporter(t, st, clk, 0)

	attrs := map[string]string{"free_bytes": "1"}
	for range 3 {
		if err := r.Raise(ctx, CodeDiskLow, "root-1", pb.Severity_SEVERITY_WARNING, "low on space", attrs); err != nil {
			t.Fatalf("raise: %v", err)
		}
	}

	got := drain(t, st)
	if len(got) != 1 {
		t.Fatalf("got %d notices after 3 raises, want 1", len(got))
	}

	n := got[0]
	if n.GetCode() != CodeDiskLow || n.GetSubjectId() != "root-1" || n.GetResolved() || n.GetSeverity() != pb.Severity_SEVERITY_WARNING || n.GetAttrs()["free_bytes"] != "1" {
		t.Fatalf("got notice %v, want open disk.low for root-1", n)
	}

	_ = st.Close()

	// A restart raises the same condition again: nothing new goes out.
	st = openStore(t, dir)
	r = newReporter(t, st, clk, 0)

	if err := r.Raise(ctx, CodeDiskLow, "root-1", pb.Severity_SEVERITY_WARNING, "low on space", attrs); err != nil {
		t.Fatalf("raise: %v", err)
	}

	if got := drain(t, st); len(got) != 0 {
		t.Fatalf("got %d notices after restart, want 0", len(got))
	}

	open, err := r.Open(ctx, CodeDiskLow, "root-1")
	if err != nil || !open {
		t.Fatalf("got Open = %v, %v, want true", open, err)
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	r := newReporter(t, st, &fakeClock{t: time.Unix(1_000_000, 0)}, 0)

	// Resolving something never raised sends nothing.
	if err := r.Resolve(ctx, CodeDiskLow, "root-1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := drain(t, st); len(got) != 0 {
		t.Fatalf("got %d notices resolving an unknown condition, want 0", len(got))
	}

	if err := r.Raise(ctx, CodeDiskLow, "root-1", pb.Severity_SEVERITY_WARNING, "low", nil); err != nil {
		t.Fatalf("raise: %v", err)
	}

	for range 2 {
		if err := r.Resolve(ctx, CodeDiskLow, "root-1"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}

	got := drain(t, st)
	if len(got) != 2 || got[0].GetResolved() || !got[1].GetResolved() {
		t.Fatalf("got %v, want an open then a resolved notice", got)
	}

	if got[1].GetMessage() != "low" || got[1].GetSeverity() != pb.Severity_SEVERITY_WARNING {
		t.Fatalf("got resolved notice %v, want the original message and severity", got[1])
	}

	// Raised again after clearing: a new notice.
	if err := r.Raise(ctx, CodeDiskLow, "root-1", pb.Severity_SEVERITY_WARNING, "low", nil); err != nil {
		t.Fatalf("raise: %v", err)
	}

	if got := drain(t, st); len(got) != 1 || got[0].GetResolved() {
		t.Fatalf("got %v, want one open notice", got)
	}
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	r := newReporter(t, st, &fakeClock{t: time.Unix(1_000_000, 0)}, 0)

	for _, s := range []string{"a", "b"} {
		if err := r.Raise(ctx, CodeDiskLow, s, pb.Severity_SEVERITY_WARNING, "low", nil); err != nil {
			t.Fatalf("raise: %v", err)
		}
	}

	drain(t, st)

	if err := r.Reconcile(ctx, CodeDiskLow, []string{"a"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := drain(t, st)
	if len(got) != 1 || got[0].GetSubjectId() != "b" || !got[0].GetResolved() {
		t.Fatalf("got %v, want b resolved", got)
	}
}

func TestRateLimit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}

	st := openStore(t, dir)
	r := newReporter(t, st, clk, 2)

	for _, s := range []string{"a", "b", "c", "d"} {
		if err := r.Raise(ctx, CodeDiskLow, s, pb.Severity_SEVERITY_WARNING, "low", nil); err != nil {
			t.Fatalf("raise: %v", err)
		}
	}

	if got := drain(t, st); len(got) != 2 {
		t.Fatalf("got %d notices, want 2 within the limit", len(got))
	}

	// d clears while held back: the change folds away and never goes out.
	if err := r.Resolve(ctx, CodeDiskLow, "d"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The budget is still spent: flushing sends nothing.
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := drain(t, st); len(got) != 0 {
		t.Fatalf("got %d notices before the window passed, want 0", len(got))
	}

	// A restart doesn't reset the budget.
	_ = st.Close()
	st = openStore(t, dir)
	r = newReporter(t, st, clk, 2)

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := drain(t, st); len(got) != 0 {
		t.Fatalf("got %d notices after restart, want 0", len(got))
	}

	clk.add(DefaultWindow + time.Second)

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got := drain(t, st)
	if len(got) != 1 || got[0].GetSubjectId() != "c" || got[0].GetResolved() {
		t.Fatalf("got %v, want only c sent once the window passed", got)
	}
}

func TestUpgraded(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	r := newReporter(t, st, &fakeClock{t: time.Unix(1_000_000, 0)}, 0)

	steps := []struct {
		version string
		want    int
	}{
		{"v1.0.0", 0}, // first run: recorded only
		{"v1.0.0", 0},
		{"v1.1.0", 1},
		{"v1.1.0", 0},
	}

	for _, s := range steps {
		if err := r.Upgraded(ctx, s.version); err != nil {
			t.Fatalf("upgraded: %v", err)
		}

		got := drain(t, st)
		if len(got) != s.want {
			t.Fatalf("version %s: got %d notices, want %d", s.version, len(got), s.want)
		}

		if s.want == 1 {
			n := got[0]
			if n.GetCode() != CodeAgentUpgraded || !n.GetResolved() || n.GetSubjectId() != "" || n.GetAttrs()["from"] != "v1.0.0" || n.GetAttrs()["to"] != "v1.1.0" {
				t.Fatalf("got %v, want a resolved upgrade from v1.0.0 to v1.1.0", n)
			}
		}
	}
}

func TestBounds(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	r := newReporter(t, st, &fakeClock{t: time.Unix(1_000_000, 0)}, 0)

	attrs := map[string]string{}
	for _, k := range strings.Split("a b c d e f g h i j", " ") {
		attrs[k] = k
	}

	msg := strings.Repeat("é", 200) // 400 bytes

	if err := r.Raise(ctx, CodeDiskLow, "", pb.Severity_SEVERITY_WARNING, msg, attrs); err != nil {
		t.Fatalf("raise: %v", err)
	}

	n := drain(t, st)[0]
	if len(n.GetMessage()) > MaxMessageBytes || !strings.HasPrefix(msg, n.GetMessage()) || len(n.GetMessage()) != 256 {
		t.Fatalf("got message of %d bytes, want 256 on a rune boundary", len(n.GetMessage()))
	}

	if len(n.GetAttrs()) != MaxAttrs || n.GetAttrs()["i"] != "" {
		t.Fatalf("got attrs %v, want the first %d keys", n.GetAttrs(), MaxAttrs)
	}
}
