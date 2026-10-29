package usenet

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

type noticeCall struct {
	code, subject string
	resolve       bool
	attrs         map[string]string
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []noticeCall
}

func (f *fakeNotifier) Raise(_ context.Context, code, subject string, _ pb.Severity, _ string, attrs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, noticeCall{code: code, subject: subject, attrs: attrs})

	return nil
}

func (f *fakeNotifier) Resolve(_ context.Context, code, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, noticeCall{code: code, subject: subject, resolve: true})

	return nil
}

func (f *fakeNotifier) snapshot() []noticeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]noticeCall(nil), f.calls...)
}

func notifyingEngine(t *testing.T, n Notifier) *Engine {
	t.Helper()

	e := New(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = e.Close() })
	e.SetNotifier(n)

	return e
}

func TestCappedConnectionsRaiseANotice(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	s.limit(2)

	n := &fakeNotifier{}
	e := notifyingEngine(t, n)
	e.SetProvider("src-1", s.provider("", "", 6))

	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(80_000)}, 2_000, nil)
	if _, err := e.Download(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}

	calls := n.snapshot()
	if len(calls) == 0 {
		t.Fatalf("got no notices, want usenet.connections_capped raised")
	}

	for _, c := range calls {
		if c.code != codeConnectionsCapped || c.subject != "src-1" || c.resolve || c.attrs["configured"] != "6" {
			t.Fatalf("got %+v, want only raises of %s for src-1 with configured=6", c, codeConnectionsCapped)
		}
	}

	e.RemoveProvider("src-1")

	if calls := n.snapshot(); !calls[len(calls)-1].resolve {
		t.Fatalf("got %+v last, want a resolve after removing the source", calls[len(calls)-1])
	}
}

func TestFullPoolResolvesTheCap(t *testing.T) {
	s := newFakeNNTP(t, "", "")

	n := &fakeNotifier{}
	e := notifyingEngine(t, n)
	e.SetProvider("src-1", s.provider("", "", 2))

	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(80_000)}, 2_000, nil)
	if _, err := e.Download(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}

	calls := n.snapshot()
	if len(calls) != 1 || !calls[0].resolve || calls[0].subject != "src-1" {
		t.Fatalf("got %+v, want one resolve for src-1 once both connections opened", calls)
	}
}
