package usenet

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookRecorder collects health hook calls.
type hookRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (h *hookRecorder) hook(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.ids = append(h.ids, id)
}

func (h *hookRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.ids)
}

func TestCheckReportsBadCredentials(t *testing.T) {
	s := newFakeNNTP(t, "user", "right")
	e := newTestEngine(t)

	rec := &hookRecorder{}
	e.SetHealthHook(rec.hook)
	e.SetProvider("src", s.provider("user", "wrong", 4))

	if h, _ := e.Health("src"); h.State != HealthUnknown {
		t.Fatalf("got %v before any connection, want unknown", h)
	}

	if err := e.Check(context.Background(), "src"); !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}

	h, _ := e.Health("src")
	if h.State != HealthFailing || !strings.Contains(h.Detail, "rejected the username or password") || strings.Contains(h.Detail, "wrong") {
		t.Fatalf("got %+v, want a failing state naming the credentials without the password", h)
	}

	if rec.count() != 1 {
		t.Fatalf("got %d hook calls, want 1", rec.count())
	}

	// Failing again changes nothing, so nothing new is reported.
	_ = e.Check(context.Background(), "src")
	if rec.count() != 1 {
		t.Fatalf("got %d hook calls after a repeat failure, want 1", rec.count())
	}
}

func TestCheckKeepsTheConnectionForDownloads(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	e := newTestEngine(t, s.provider("", "", 1))

	if err := e.Check(context.Background(), "p0"); err != nil {
		t.Fatal(err)
	}

	if h, _ := e.Health("p0"); h.State != HealthOK || h.Detail != "" {
		t.Fatalf("got %+v, want OK", h)
	}

	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(20_000)}, 5_000, nil)
	if _, err := e.Download(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}

	// The check's connection was reused, and a second check while it is
	// idle needs no new one either.
	if err := e.Check(context.Background(), "p0"); err != nil {
		t.Fatal(err)
	}

	if _, accepted := s.stats(); accepted != 1 {
		t.Fatalf("got %d connections, want the check's one reused", accepted)
	}
}

func TestUnreachableProviderTurnsFailingOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(20_000)}, 5_000, nil)

	e := newTestEngine(t)
	e.backoff = func(int) time.Duration { return time.Millisecond }

	rec := &hookRecorder{}
	e.SetHealthHook(rec.hook)
	e.SetProvider("dead", Provider{Host: "127.0.0.1", Port: port, Connections: 2})

	_, _ = e.Download(context.Background(), job, nil)

	// Linux and macOS say "connection refused"; Windows says the target
	// machine "actively refused it".
	h, _ := e.Health("dead")
	if h.State != HealthFailing || !strings.Contains(h.Detail, "refused") || rec.count() != 1 {
		t.Fatalf("got %+v after %d hook calls, want one change to failing with a reason", h, rec.count())
	}
}

func TestCheckUnknownProvider(t *testing.T) {
	e := newTestEngine(t)

	if err := e.Check(context.Background(), "nope"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("got %v, want ErrNoProvider", err)
	}

	if _, ok := e.Health("nope"); ok {
		t.Fatal("got health for an unknown provider")
	}
}

func TestDescribeKeepsDetailsShortAndPrintable(t *testing.T) {
	d := Describe(errors.New("400 " + strings.Repeat("é", 300) + "\r\nInjected: header"))
	if len(d) > maxDetail || strings.ContainsAny(d, "\r\n") || !strings.HasPrefix(d, "the provider refused") && !strings.HasPrefix(d, "400") {
		t.Fatalf("got %q", d)
	}
}

func TestCheckRetriesAfterALoginFailure(t *testing.T) {
	s := newFakeNNTP(t, "user", "old")
	e := newTestEngine(t)
	e.SetProvider("src", s.provider("user", "new", 2))

	if err := e.Check(context.Background(), "src"); !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}

	// The user renews the account; the stored password now works.
	s.setPass("new")

	if err := e.Check(context.Background(), "src"); err != nil {
		t.Fatalf("got %v, want the check to try again and pass", err)
	}

	if h, _ := e.Health("src"); h.State != HealthOK {
		t.Fatalf("got %+v, want OK", h)
	}

	// Downloads may use the provider again.
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(10_000)}, 5_000, nil)
	if _, err := e.Download(context.Background(), job, nil); err != nil {
		t.Fatalf("got %v downloading after recovery", err)
	}
}
