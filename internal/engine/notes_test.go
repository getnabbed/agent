package engine

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

func TestNilNotesDiscard(t *testing.T) {
	var n *Notes

	n.Info("a", "b")

	if got := n.Drain(); got != nil {
		t.Fatalf("got %v from nil notes", got)
	}
}

func TestNotesKeepAttrsAndDrainOnce(t *testing.T) {
	n := &Notes{}
	n.Info("par2.verified", "PAR2 verified 4 files", "files", 4, "ok", true, "odd")

	got := n.Drain()
	if len(got) != 1 || got[0].GetCode() != "par2.verified" || got[0].GetSeverity() != pb.Severity_SEVERITY_INFO {
		t.Fatalf("got %v", got)
	}

	if a := got[0].GetAttrs(); len(a) != 2 || a["files"] != "4" || a["ok"] != "true" {
		t.Fatalf("got attrs %v, want files and ok, the unpaired key dropped", a)
	}

	if again := n.Drain(); len(again) != 0 {
		t.Fatalf("got %v on a second drain", again)
	}
}

func TestNotesAreBounded(t *testing.T) {
	n := &Notes{}

	var attrs []any
	for i := range 12 {
		attrs = append(attrs, fmt.Sprintf("k%02d", i), i)
	}

	long := strings.Repeat("é", 200) // 400 bytes
	n.Warn("big", long, attrs...)

	got := n.Drain()[0]
	if len(got.GetMessage()) > maxNoteMessage || !utf8.ValidString(got.GetMessage()) {
		t.Fatalf("got a %d-byte message, valid UTF-8 %v", len(got.GetMessage()), utf8.ValidString(got.GetMessage()))
	}

	if len(got.GetAttrs()) != maxNoteAttrs || got.GetAttrs()["k00"] != "0" || got.GetAttrs()["k11"] != "" {
		t.Fatalf("got attrs %v, want the first %d keys", got.GetAttrs(), maxNoteAttrs)
	}
}

func TestBeyondTheLimitTheMostSevereNotesAreKeptInOrder(t *testing.T) {
	n := &Notes{}

	for i := range 30 {
		n.Info("info", fmt.Sprint(i))
	}

	n.Error("bad", "first error")
	n.Warn("meh", "a warning")

	got := n.Drain()
	if len(got) != maxNotes {
		t.Fatalf("got %d notes, want %d", len(got), maxNotes)
	}

	if got[0].GetMessage() != "0" || got[maxNotes-2].GetCode() != "bad" || got[maxNotes-1].GetCode() != "meh" {
		t.Fatalf("got first %q, then %q and %q; want the earliest infos, then the error and warning in order",
			got[0].GetMessage(), got[maxNotes-2].GetCode(), got[maxNotes-1].GetCode())
	}
}
