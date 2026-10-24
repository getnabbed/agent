package usenet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchFilesDownloadsDeferredVolumes(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, files := buildJob(t, s, map[string][]byte{
		"movie.mkv":       randBytes(50_000),
		"m.vol00+01.par2": randBytes(30_000),
		"m.vol01+02.par2": randBytes(9_000),
	}, 8_000, nil)
	e := newTestEngine(t, s.provider("", "", 2))

	res, err := e.Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Deferred) != 2 {
		t.Fatalf("got Deferred = %v, want both volumes", res.Deferred)
	}

	downloaded := s.bodies.Load()

	got, err := e.FetchFiles(context.Background(), job, []string{"m.vol01+02.par2", "not-in-the.nzb"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Files) != 1 || got.Files[0].Path != "data/m.vol01+02.par2" || got.Files[0].Size != 9_000 || got.MissingArticles != 0 {
		t.Fatalf("got %+v, want only data/m.vol01+02.par2", got)
	}

	if s.bodies.Load()-downloaded != 2 {
		t.Fatalf("got %d articles fetched, want the volume's 2", s.bodies.Load()-downloaded)
	}

	data, err := os.ReadFile(filepath.Join(job.Dir, "data", "m.vol01+02.par2"))
	if err != nil || !bytes.Equal(data, files["m.vol01+02.par2"]) {
		t.Fatalf("got %v, want the volume's content", err)
	}

	if _, err := os.Stat(filepath.Join(job.Dir, "data", "m.vol00+01.par2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want the other volume left deferred", err)
	}

	// Fetching again resumes from its own checkpoint, and the download's
	// checkpoint still covers everything it fetched.
	before := s.bodies.Load()

	if _, err := e.FetchFiles(context.Background(), job, []string{"m.vol01+02.par2"}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Download(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}

	if n := s.bodies.Load() - before; n != 0 {
		t.Fatalf("got %d articles refetched, want 0", n)
	}
}

func TestFetchFilesReportsMissingArticles(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"m.vol00+04.par2": randBytes(40_000)}, 10_000, map[string]bool{"m.vol00+04.par2-2@test": true})

	got, err := newTestEngine(t, s.provider("", "", 2)).FetchFiles(context.Background(), job, []string{"m.vol00+04.par2"})
	if err != nil {
		t.Fatal(err)
	}

	if got.MissingArticles != 1 || len(got.Files) != 1 || got.Files[0].Size != 40_000 {
		t.Fatalf("got %+v, want the volume with 1 missing article", got)
	}
}

func TestFetchFilesCancelled(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"m.vol00+04.par2": randBytes(100_000)}, 1_000, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := newTestEngine(t, s.provider("", "", 2)).FetchFiles(ctx, job, []string{"m.vol00+04.par2"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
