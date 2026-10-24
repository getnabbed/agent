package usenet_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/postproc"
)

func seq(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}

	return []byte(b.String())
}

// A release posted with an obfuscated name loses one article. The engine
// defers the recovery volumes; the assembler fetches just the volume it
// needs from the server, repairs the file, restores its name and cleans up.
func TestDamagedDownloadIsRepairedWithFetchedVolumes(t *testing.T) {
	files := map[string][]byte{"a8f3k2b9": seq(40000), "movie.nfo": seq(300)}

	for _, name := range []string{"release.par2", "release.vol0+2.par2", "release.vol2+1.par2"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "par2", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}

		files[name] = raw
	}

	srv := usenet.NewTestServer(t)

	// Part 3 holds bytes 40000-59999: slices 2 and 3 of 16 KiB.
	job := srv.Post(t, files, 20_000, map[string]bool{"a8f3k2b9-3@test": true})
	e := srv.Engine(t, 4)

	res, err := e.Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 1 || len(res.Deferred) != 2 {
		t.Fatalf("got %d missing articles and deferred %v, want 1 and both volumes", res.MissingArticles, res.Deferred)
	}

	before := srv.Bodies()

	a := &postproc.Assembler{Log: slog.New(slog.DiscardHandler), Fetcher: e}

	out, err := a.Assemble(context.Background(), job, res)
	if err != nil {
		t.Fatal(err)
	}

	// Two blocks needed: the 2-block volume, in its two articles, and not
	// the 1-block one.
	if n := srv.Bodies() - before; n != 2 {
		t.Fatalf("got %d articles fetched for repair, want 2", n)
	}

	var got []string
	for _, f := range out.Files {
		got = append(got, f.Path)
	}

	if strings.Join(got, ",") != "data/movie.mkv,data/movie.nfo" {
		t.Fatalf("got files %v, want the repaired, renamed release without par2 files", got)
	}

	mkv, err := os.ReadFile(filepath.Join(job.Dir, "data", "movie.mkv"))
	if err != nil || !bytes.Equal(mkv, seq(40000)) {
		t.Fatalf("got movie.mkv err=%v, want the original content", err)
	}

	left, _ := filepath.Glob(filepath.Join(job.Dir, "data", "*.par2"))
	if len(left) != 0 {
		t.Fatalf("got %v left after clean-up", left)
	}
}
