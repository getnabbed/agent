package usenet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// dropAll lists every article of name, as buildJob numbers them, for a
// file of size bytes in parts of partSize.
func dropAll(name string, size, partSize int) map[string]bool {
	drop := map[string]bool{}
	for p := range (size + partSize - 1) / partSize {
		drop[fmt.Sprintf("%s-%d@test", name, p+1)] = true
	}

	return drop
}

func TestReleaseBeyondRepairStopsEarly(t *testing.T) {
	// A release taken down from the provider: every article of the movie
	// is gone, and its PAR2 recovery volume holds far less than that.
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{
		"movie.mkv":           randBytes(400_000),
		"movie.par2":          randBytes(1_000),
		"movie.vol00+01.par2": randBytes(3_000),
	}, 10_000, dropAll("movie.mkv", 400_000, 10_000))

	_, err := newTestEngine(t, s.provider("", "", 1)).Download(context.Background(), job, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_INCOMPLETE || f.Retryable ||
		!strings.Contains(f.Detail, "more than the release's PAR2 recovery volumes") {
		t.Fatalf("got %v, want a permanent INCOMPLETE failure saying the release cannot be repaired", err)
	}

	// One missing 10 kB article already exceeds 3 kB of recovery data. The
	// index may have been fetched first; the other 39 articles must not.
	if n := s.bodies.Load(); n > 3 {
		t.Fatalf("got %d articles requested, want the download stopped after the first missing ones", n)
	}
}

func TestRepairableLossKeepsGoing(t *testing.T) {
	// One article missing, and recovery volumes larger than it: PAR2 can
	// restore it, so the download must finish for repair to run.
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{
		"movie.mkv":           randBytes(100_000),
		"movie.par2":          randBytes(1_000),
		"movie.vol00+03.par2": randBytes(30_000),
	}, 10_000, map[string]bool{"movie.mkv-4@test": true})

	res, err := newTestEngine(t, s.provider("", "", 2)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("got %v, want the download to finish so PAR2 can repair it", err)
	}

	if res.MissingArticles != 1 {
		t.Fatalf("got %d missing articles, want 1", res.MissingArticles)
	}
}

func TestUnnamedPAR2KeepsGoing(t *testing.T) {
	// Obfuscated names hide which files are recovery data, so the loss
	// cannot be weighed against it: keep today's behaviour and fetch all.
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{
		"a8f3k2b9": randBytes(100_000),
		"x91mq":    randBytes(30_000),
	}, 10_000, dropAll("a8f3k2b9", 100_000, 10_000))

	res, err := newTestEngine(t, s.provider("", "", 2)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("got %v, want the download to finish", err)
	}

	if res.MissingArticles != 10 || s.bodies.Load() != 13 {
		t.Fatalf("got %d missing of %d requested, want all 13 articles asked for and 10 missing", res.MissingArticles, s.bodies.Load())
	}
}

func TestRecoveryFetchDoesNotStopEarly(t *testing.T) {
	// Fetching recovery volumes for repair reports what is missing; the
	// assembler decides whether repair can still work.
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{
		"movie.mkv":           randBytes(20_000),
		"movie.par2":          randBytes(1_000),
		"movie.vol00+01.par2": randBytes(30_000),
	}, 10_000, dropAll("movie.vol00+01.par2", 30_000, 10_000))

	res, err := newTestEngine(t, s.provider("", "", 2)).FetchFiles(context.Background(), job, []string{"movie.vol00+01.par2"})
	if err != nil {
		t.Fatalf("got %v, want the fetch to report its missing articles", err)
	}

	if res.MissingArticles != 3 {
		t.Fatalf("got %d missing articles, want 3", res.MissingArticles)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 999: "999 B", 1_000: "1.0 kB", 3_100: "3.1 kB", 5_889_100_000: "5.9 GB",
	} {
		if got := humanBytes(n); got != want {
			t.Fatalf("got humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
