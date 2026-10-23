package usenet

import (
	"bytes"
	"context"
	"crypto/rand"
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

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"show.s01e01.mkv":          "show.s01e01.mkv",
		"../../etc/passwd":         "passwd",
		`..\..\windows\system.ini`: "system.ini",
		"..":                       "file-1",
		"":                         "file-1",
		".hidden":                  "hidden",
		"a\x00b\nc.txt":            "a_b_c.txt",
		"con.txt":                  "file-1",
		"C:evil.exe":               "C_evil.exe",
	}

	cases["trailing"+". "] = "trailing" // Windows silently strips trailing dots and spaces

	for in, want := range cases {
		if got := SafeName(in, 0); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("é", 300)
	if got := SafeName(long, 0); len(got) > maxNameBytes || !strings.HasPrefix(long, got) {
		t.Errorf("long name not truncated on a rune boundary: %d bytes", len(got))
	}
}

const sampleNZB = `<?xml version="1.0" encoding="iso-8859-1" ?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <head><meta type="password">secret</meta></head>
 <file poster="p" date="1" subject="[1/3] - &quot;../evil.mkv&quot; yEnc (1/2)">
  <groups><group>alt.binaries.test</group></groups>
  <segments>
   <segment bytes="20" number="2">b@x</segment>
   <segment bytes="10" number="1">a@x</segment>
   <segment bytes="10" number="1">dup@x</segment>
  </segments>
 </file>
 <file poster="p" date="1" subject="&quot;evil.mkv&quot; yEnc (1/1)">
  <segments><segment bytes="5" number="1">c@x</segment></segments>
 </file>
 <file poster="p" date="1" subject="&quot;x.vol00+01.par2&quot; yEnc (1/1)">
  <segments><segment bytes="5" number="1">d@x</segment></segments>
 </file>
</nzb>`

func TestParseNZB(t *testing.T) {
	n, err := ParseNZB(strings.NewReader(sampleNZB))
	if err != nil {
		t.Fatal(err)
	}

	if n.Meta["password"] != "secret" || len(n.Files) != 3 {
		t.Fatalf("parsed %+v", n)
	}

	f := n.Files[0]
	if f.Name != "evil.mkv" || len(f.Segments) != 2 || f.Segments[0].MessageID != "a@x" || f.Bytes() != 30 {
		t.Fatalf("file 0 = %+v", f)
	}

	if n.Files[1].Name != "evil (2).mkv" {
		t.Fatalf("duplicate name not made unique: %q", n.Files[1].Name)
	}

	if !IsPAR2Volume(n.Files[2].Name) || IsPAR2Volume("x.par2") {
		t.Fatal("par2 volume detection wrong")
	}
}

func TestYEncRoundTrip(t *testing.T) {
	data := make([]byte, 5000)
	_, _ = rand.Read(data)

	// Force every byte that needs escaping, and a leading dot.
	copy(data, []byte{256 - 42, 256 + '\n' - 42, 256 + '\r' - 42, '=' - 42, '.' - 42, 256 + '\t' - 42})

	art := EncodeYEnc("f.bin", 20000, 1001, data, 2, 4)

	p, err := DecodeYEnc(bytes.NewReader(art))
	if err != nil {
		t.Fatal(err)
	}

	if p.Name != "f.bin" || p.Begin != 1001 || p.End != 6000 || p.FileSize != 20000 || !bytes.Equal(p.Data, data) {
		t.Fatalf("round trip mismatch: name=%q begin=%d end=%d size=%d equal=%v", p.Name, p.Begin, p.End, p.FileSize, bytes.Equal(p.Data, data))
	}
}

func TestYEncRejectsCorruption(t *testing.T) {
	art := EncodeYEnc("f.bin", 100, 1, []byte("hello world"), 1, 1)

	bad := append([]byte(nil), art...)
	bad[bytes.Index(bad, []byte("\r\n=yend"))-1] ^= 0x01 // flip a data bit

	if _, err := DecodeYEnc(bytes.NewReader(bad)); !errors.Is(err, ErrYEncCRC) {
		t.Fatalf("corrupt data err = %v, want ErrYEncCRC", err)
	}

	truncated, _, _ := bytes.Cut(art, []byte("=yend"))
	if _, err := DecodeYEnc(bytes.NewReader(truncated)); err == nil {
		t.Fatal("truncated article accepted")
	}
}

// buildJob posts files to the fake server and writes a matching NZB.
func buildJob(t *testing.T, s *fakeNNTP, files map[string][]byte, partSize int, drop map[string]bool) (engine.Job, map[string][]byte) {
	t.Helper()

	dir := t.TempDir()

	var nzb strings.Builder
	nzb.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">`)

	for name, data := range files {
		total := (len(data) + partSize - 1) / partSize
		fmt.Fprintf(&nzb, `<file subject="&quot;%s&quot; yEnc (1/%d)"><segments>`, name, total)

		for p := range total {
			lo, hi := p*partSize, min((p+1)*partSize, len(data))
			id := fmt.Sprintf("%s-%d@test", name, p+1)
			art := EncodeYEnc(name, int64(len(data)), int64(lo+1), data[lo:hi], p+1, total)

			if !drop[id] {
				s.add(id, art)
			}

			fmt.Fprintf(&nzb, `<segment bytes="%d" number="%d">%s</segment>`, len(art), p+1, id)
		}

		nzb.WriteString(`</segments></file>`)
	}

	nzb.WriteString(`</nzb>`)

	if err := os.WriteFile(filepath.Join(dir, "payload.nzb"), []byte(nzb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	return engine.Job{ID: "d1", Dir: dir, PayloadPath: filepath.Join(dir, "payload.nzb")}, files
}

func newTestEngine(p Provider) *Engine {
	return New(func(context.Context, string) (Provider, error) { return p, nil }, func() bool { return true }, slog.New(slog.DiscardHandler))
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return b
}

func TestDownloadAssemblesFiles(t *testing.T) {
	s := newFakeNNTP(t, "u", "p")
	job, files := buildJob(t, s, map[string][]byte{
		"movie.mkv":       randBytes(100_000),
		"movie.nfo":       randBytes(700),
		"m.vol00+01.par2": randBytes(3000),
	}, 16_000, nil)

	var last engine.Progress
	res, err := newTestEngine(s.provider("u", "p", 4)).Download(context.Background(), job, func(p engine.Progress) { last = p })
	if err != nil {
		t.Fatal(err)
	}

	_ = last

	if res.MissingArticles != 0 || len(res.Deferred) != 1 || res.Deferred[0] != "m.vol00+01.par2" {
		t.Fatalf("result = %+v", res)
	}

	for _, f := range res.Files {
		got, err := os.ReadFile(filepath.Join(job.Dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(got, files[f.Path]) || f.Size != int64(len(files[f.Path])) {
			t.Fatalf("%s: content or size mismatch (size %d)", f.Path, f.Size)
		}
	}

	if _, err := os.Stat(filepath.Join(job.Dir, "m.vol00+01.par2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deferred par2 volume was downloaded")
	}
}

func TestMissingArticleIsReportedNotFatal(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(50_000)}, 10_000, map[string]bool{"a.bin-3@test": true})

	res, err := newTestEngine(s.provider("", "", 2)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 1 || len(res.Files) != 1 || res.Files[0].Size != 50_000 {
		t.Fatalf("result = %+v", res)
	}
}

func TestBadCredentialsFailFast(t *testing.T) {
	s := newFakeNNTP(t, "u", "right")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(30_000)}, 10_000, nil)

	_, err := newTestEngine(s.provider("u", "wrong", 2)).Download(context.Background(), job, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_CLIENT || f.Retryable {
		t.Fatalf("err = %v, want non-retryable CLIENT failure", err)
	}
}

func TestResumeSkipsCheckpointedSegments(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, files := buildJob(t, s, map[string][]byte{"a.bin": randBytes(80_000)}, 10_000, nil)
	e := newTestEngine(s.provider("", "", 1))

	if _, err := e.Download(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}

	first := s.bodies.Load()

	res, err := e.Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if s.bodies.Load() != first {
		t.Fatalf("second run fetched %d articles, want 0", s.bodies.Load()-first)
	}

	got, _ := os.ReadFile(filepath.Join(job.Dir, res.Files[0].Path))
	if !bytes.Equal(got, files["a.bin"]) {
		t.Fatal("resumed file content differs")
	}
}

func TestCancelReturnsContextError(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(200_000)}, 1_000, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := newTestEngine(s.provider("", "", 2)).Download(ctx, job, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestParseProviderURL(t *testing.T) {
	p, err := ParseProviderURL("nntps://user:p%40ss@news.example.com?connections=20")
	if err != nil {
		t.Fatal(err)
	}

	if !p.TLS || p.Port != 563 || p.Username != "user" || p.Password != "p@ss" || p.Connections != 20 {
		t.Fatalf("parsed %+v", p)
	}

	if strings.Contains(p.String(), "p@ss") {
		t.Fatal("String leaks the password")
	}
}
