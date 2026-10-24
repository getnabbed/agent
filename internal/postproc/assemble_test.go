package postproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

var quiet = slog.New(slog.DiscardHandler)

// seq reproduces `seq 1 n`, the content the par2 fixtures were made from.
func seq(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}

	return []byte(b.String())
}

// job lays out a work dir the way the usenet engine leaves it: files under
// data/, named as in the NZB, with the PAR2 index beside them.
func job(t *testing.T, files map[string][]byte) (engine.Job, *engine.Result) {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}

	index, err := os.ReadFile(filepath.Join("..", "par2", "testdata", "release.par2"))
	if err != nil {
		t.Fatal(err)
	}

	files["release.par2"] = index
	res := &engine.Result{}

	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, "data", name), data, 0o600); err != nil {
			t.Fatal(err)
		}

		res.Files = append(res.Files, engine.File{Path: "data/" + name, Size: int64(len(data))})
	}

	return engine.Job{ID: "d1", Dir: dir, PayloadPath: filepath.Join(dir, "payload.nzb")}, res
}

func assemble(j engine.Job, res *engine.Result) (*engine.Result, error) {
	return (&Assembler{Log: quiet}).Assemble(context.Background(), j, res)
}

func paths(res *engine.Result) string {
	var p []string
	for _, f := range res.Files {
		p = append(p, f.Path)
	}

	return strings.Join(p, ",")
}

func TestRestoresObfuscatedNames(t *testing.T) {
	j, res := job(t, map[string][]byte{"a8f3k2b9": seq(40000), "x91mq": seq(300)})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "data/movie.mkv,data/movie.nfo" {
		t.Fatalf("got files %s, want the real names and the par2 index removed", got)
	}

	got, err := os.ReadFile(filepath.Join(j.Dir, "data", "movie.mkv"))
	if err != nil || !bytes.Equal(got, seq(40000)) {
		t.Fatalf("got movie.mkv err=%v, want the original content", err)
	}

	// Running again on its own output changes nothing.
	again, err := assemble(j, out)
	if err != nil || paths(again) != paths(out) {
		t.Fatalf("got %s, %v on a second run, want the same files", paths(again), err)
	}
}

func TestMissingArticlesWithIntactFilesPass(t *testing.T) {
	j, res := job(t, map[string][]byte{"movie.mkv": seq(40000), "movie.nfo": seq(300)})
	res.MissingArticles = 1 // say, in a sample file outside the recovery set

	if _, err := assemble(j, res); err != nil {
		t.Fatalf("got %v, want a pass after a full verify", err)
	}
}

func damaged() []byte {
	d := seq(40000)
	clear(d[40000:41000])

	return d
}

func TestRepairableDamageFailsRetryably(t *testing.T) {
	j, res := job(t, map[string][]byte{"obf1": damaged(), "obf2": seq(300)})
	res.MissingArticles = 1
	res.Deferred = []string{"release.vol0+2.par2", "release.vol2+1.par2"}

	_, err := assemble(j, res)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_REPAIR || !f.Retryable || !strings.Contains(f.Detail, "1 of 3") {
		t.Fatalf("got %v, want a retryable REPAIR failure needing 1 of 3 blocks", err)
	}

	// The intact file still got its name back, so a retry starts from it.
	if _, err := os.Stat(filepath.Join(j.Dir, "data", "movie.nfo")); err != nil {
		t.Fatalf("got %v, want movie.nfo restored", err)
	}
}

func TestUnrepairableDamageFailsForGood(t *testing.T) {
	j, res := job(t, map[string][]byte{"movie.mkv": damaged()})
	res.MissingArticles = 2

	_, err := assemble(j, res)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_INCOMPLETE || f.Retryable {
		t.Fatalf("got %v, want a permanent INCOMPLETE failure so the cloud picks another release", err)
	}
}

func TestWithoutPar2(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"a.mkv": []byte("x")})

	if out, err := assemble(j, res); err != nil || paths(out) != "data/a.mkv" {
		t.Fatalf("got %s, %v; want a pass with the same file", paths(out), err)
	}

	res.MissingArticles = 3

	var f *engine.Failure
	if _, err := assemble(j, res); !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_INCOMPLETE || f.Retryable {
		t.Fatalf("got %v, want a permanent INCOMPLETE failure", err)
	}
}

func TestTakenNameIsNotOverwritten(t *testing.T) {
	j, res := job(t, map[string][]byte{"obf": seq(40000), "movie.nfo": seq(300), "movie.mkv": []byte("someone else's file")})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(filepath.Join(j.Dir, "data", "movie.mkv"))
	if string(got) != "someone else's file" || !strings.Contains(paths(out), "data/obf") {
		t.Fatalf("got movie.mkv %q and files %s; want the existing file kept and obf left in place", got, paths(out))
	}
}
