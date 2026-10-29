package usenet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// copyArticles gives dst every article of src except those in skip.
func copyArticles(dst, src *fakeNNTP, skip map[string]bool) {
	src.mu.Lock()
	defer src.mu.Unlock()

	for id, body := range src.articles {
		if !skip[id] {
			dst.add(id, body)
		}
	}
}

// checkFiles fails unless every result file matches its source bytes.
func checkFiles(t *testing.T, job engine.Job, res *engine.Result, files map[string][]byte) {
	t.Helper()

	for _, f := range res.Files {
		got, err := os.ReadFile(filepath.Join(job.Dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(got, files[path.Base(f.Path)]) {
			t.Fatalf("%s: content mismatch", f.Path)
		}
	}
}

func TestConnectionLimitIsSharedAcrossDownloads(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	e := newTestEngine(t, s.provider("", "", 2))

	type run struct {
		job   engine.Job
		files map[string][]byte
		res   *engine.Result
		err   error
	}

	runs := make([]run, 3)
	for i := range runs {
		name := fmt.Sprintf("f%d.bin", i) // distinct message ids per job
		runs[i].job, runs[i].files = buildJob(t, s, map[string][]byte{name: randBytes(60_000)}, 2_000, nil)
	}

	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runs[i].res, runs[i].err = e.Download(context.Background(), runs[i].job, nil)
		}()
	}

	wg.Wait()

	for i, r := range runs {
		if r.err != nil {
			t.Fatalf("download %d: %v", i, r.err)
		}

		if r.res.MissingArticles != 0 {
			t.Fatalf("download %d: got MissingArticles = %d, want 0", i, r.res.MissingArticles)
		}

		checkFiles(t, r.job, r.res, r.files)
	}

	if peak, _ := s.stats(); peak > 2 {
		t.Fatalf("got peak connections = %d across three downloads, want <= 2", peak)
	}
}

func TestBackupFillsMissingArticles(t *testing.T) {
	full := newFakeNNTP(t, "", "")
	job, files := buildJob(t, full, map[string][]byte{"a.bin": randBytes(50_000)}, 10_000, nil)

	primary := newFakeNNTP(t, "", "")
	copyArticles(primary, full, map[string]bool{"a.bin-2@test": true, "a.bin-4@test": true})

	backup := full.provider("", "", 2)
	backup.Backup = true

	job.Notes = &engine.Notes{}

	res, err := newTestEngine(t, primary.provider("", "", 2), backup).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 0 {
		t.Fatalf("got MissingArticles = %d, want 0", res.MissingArticles)
	}

	checkFiles(t, job, res, files)

	if got := full.bodies.Load(); got != 2 {
		t.Fatalf("got %d articles from the backup, want only the 2 the primary lacked", got)
	}

	if s := res.Stats; s == nil || s.SourceID != "p0" || s.Bytes == 0 || s.RunBytes != s.Bytes {
		t.Fatalf("got stats %+v, want the primary p0 as the main source and every byte from this run", s)
	}

	notes := job.Notes.Drain()
	if len(notes) != 1 || notes[0].GetCode() != "usenet.provider_fallback" ||
		notes[0].GetAttrs()["from"] != "p0" || notes[0].GetAttrs()["to"] != "p1" || notes[0].GetAttrs()["articles"] != "2" {
		t.Fatalf("got notes %v, want one fallback note for 2 articles from p1", notes)
	}
}

func TestDamagedArticleTriesNextProvider(t *testing.T) {
	good := newFakeNNTP(t, "", "")
	job, files := buildJob(t, good, map[string][]byte{"a.bin": randBytes(30_000)}, 10_000, nil)

	bad := newFakeNNTP(t, "", "")
	copyArticles(bad, good, nil)

	bad.mu.Lock()
	body := append([]byte(nil), bad.articles["a.bin-2@test"]...)
	body[bytes.Index(body, []byte("\r\n=yend"))-1] ^= 0x01
	bad.articles["a.bin-2@test"] = body
	bad.mu.Unlock()

	backup := good.provider("", "", 1)
	backup.Backup = true

	res, err := newTestEngine(t, bad.provider("", "", 1), backup).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 0 {
		t.Fatalf("got MissingArticles = %d, want 0", res.MissingArticles)
	}

	checkFiles(t, job, res, files)
}

func TestProviderConnectionLimitIsLearned(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	s.limit(2)

	job, files := buildJob(t, s, map[string][]byte{"a.bin": randBytes(80_000)}, 2_000, nil)

	res, err := newTestEngine(t, s.provider("", "", 6)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 0 {
		t.Fatalf("got MissingArticles = %d, want 0", res.MissingArticles)
	}

	checkFiles(t, job, res, files)
}

func TestBackupAuthFailureIsNotFatal(t *testing.T) {
	primary := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, primary, map[string][]byte{"a.bin": randBytes(30_000)}, 10_000, map[string]bool{"a.bin-1@test": true})

	locked := newFakeNNTP(t, "u", "right")
	backup := locked.provider("u", "wrong", 1)
	backup.Backup = true

	res, err := newTestEngine(t, primary.provider("", "", 2), backup).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("got err = %v, want the backup's bad credentials to be skipped", err)
	}

	if res.MissingArticles != 1 {
		t.Fatalf("got MissingArticles = %d, want 1", res.MissingArticles)
	}
}

func TestIdleConnectionsAreReused(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	e := newTestEngine(t, s.provider("", "", 1))

	for range 2 {
		job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(20_000)}, 5_000, nil)

		if _, err := e.Download(context.Background(), job, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, accepted := s.stats(); accepted != 1 {
		t.Fatalf("got %d connections for two downloads, want 1 reused", accepted)
	}
}

func TestUnreachableProviderFailsRetryable(t *testing.T) {
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
	e.SetProvider("dead", Provider{Host: "127.0.0.1", Port: port, Connections: 2})

	_, err = e.Download(context.Background(), job, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_CLIENT || !f.Retryable {
		t.Fatalf("got err = %v, want a retryable CLIENT failure", err)
	}
}

func TestNoProviderFails(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(1_000)}, 5_000, nil)

	e := newTestEngine(t)
	if e.Available() {
		t.Fatal("got Available = true with no providers")
	}

	if _, err := e.Download(context.Background(), job, nil); err == nil || !strings.Contains(err.Error(), ErrNoProvider.Error()) {
		t.Fatalf("got err = %v, want a failure naming no provider", err)
	}
}

func TestParseProviderList(t *testing.T) {
	ps, err := ParseProviderList("nntps://a:x@one.example\n  nntps://b:y@two.example?backup=true&connections=4")
	if err != nil {
		t.Fatal(err)
	}

	if len(ps) != 2 || ps[0].Backup || !ps[1].Backup || ps[1].Connections != 4 {
		t.Fatalf("got %+v", ps)
	}

	if _, err := ParseProviderList("nntps://a:x@one.example?backup=maybe"); err == nil {
		t.Fatal("got no error for backup=maybe")
	}
}

func TestCapacityMessages(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want bool
	}{
		{400, "service temporarily unavailable", true},
		{502, "Too many connections", true},
		{481, "Max connections reached for this account", true},
		{481, "Authentication failed", false},
		{502, "Permission denied", false},
	}

	for _, c := range cases {
		if got := isCapacity(c.code, c.msg); got != c.want {
			t.Fatalf("got isCapacity(%d, %q) = %v, want %v", c.code, c.msg, got, c.want)
		}
	}
}

func TestReplacedProviderFailsRetryableNotMissing(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	e := newTestEngine(t, s.provider("", "", 1))
	route := e.route("")
	e.SetProvider("p0", s.provider("", "", 1)) // closes the pool in route

	d := &download{e: e, route: route}
	err := d.fetch(context.Background(), workItem{segment: Segment{MessageID: "a.bin-1@test"}})

	var f *engine.Failure
	if !errors.As(err, &f) || !f.Retryable {
		t.Fatalf("got err = %v, want a retryable failure, not a missing article", err)
	}
}
