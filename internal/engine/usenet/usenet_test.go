package usenet

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

	cases["trailing"+". "] = "trailing"   // Windows silently strips trailing dots and spaces
	cases["x\u202egnp.exe"] = "x_gnp.exe" // bidi override cannot spoof the extension
	cases["bad\xffutf8.bin"] = "bad_utf8.bin"
	cases["COM\u00b9.txt"] = "file-1"
	cases["CONIN$"] = "file-1"
	cases["CON .txt"] = "file-1"

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

func TestYEncRejectsAnUnreadableChecksum(t *testing.T) {
	art := EncodeYEnc("f.bin", 100, 1, []byte("hello world"), 1, 1)
	i := bytes.Index(art, []byte("pcrc32=")) + len("pcrc32=")

	for _, crc := range []string{"zzzzzzzz", "", "1234567890"} {
		bad := slices.Concat(art[:i], []byte(crc), []byte("\r\n"))
		if _, err := DecodeYEnc(bytes.NewReader(bad)); !errors.Is(err, ErrYEncCRC) {
			t.Fatalf("pcrc32=%q: got %v, want ErrYEncCRC", crc, err)
		}
	}

	// No checksum at all is allowed; PAR2 verifies the file.
	noCRC := slices.Concat(art[:i-len("pcrc32=")], []byte("\r\n"))
	if _, err := DecodeYEnc(bytes.NewReader(noCRC)); err != nil {
		t.Fatalf("got %v for a part without a checksum, want it accepted", err)
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

func newTestEngine(t *testing.T, providers ...Provider) *Engine {
	t.Helper()

	e := New(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = e.Close() })

	for i, p := range providers {
		e.SetProvider(fmt.Sprintf("p%d", i), p)
	}

	return e
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
	res, err := newTestEngine(t, s.provider("u", "p", 4)).Download(context.Background(), job, func(p engine.Progress) { last = p })
	if err != nil {
		t.Fatal(err)
	}

	// The download takes less than the progress interval; the final report
	// still says it reached the end.
	if last.Total == 0 || last.Bytes != last.Total {
		t.Fatalf("got last progress %+v, want a final report with every byte", last)
	}

	if res.MissingArticles != 0 || len(res.Deferred) != 1 || res.Deferred[0] != "m.vol00+01.par2" {
		t.Fatalf("result = %+v", res)
	}

	for _, f := range res.Files {
		got, err := os.ReadFile(filepath.Join(job.Dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}

		if want := files[path.Base(f.Path)]; !bytes.Equal(got, want) || f.Size != int64(len(want)) {
			t.Fatalf("%s: content or size mismatch (size %d)", f.Path, f.Size)
		}
	}

	if _, err := os.Stat(filepath.Join(job.Dir, "m.vol00+01.par2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deferred par2 volume was downloaded")
	}
}

// No article is fetched without a transfer slot, and a freed slot lets the
// download go on.
func TestTransferSlotsGateArticles(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	e := newTestEngine(t, s.provider("", "", 8))
	e.transfers = make(chan struct{}, 1)
	e.transfers <- struct{}{} // another download holds the only slot

	job, files := buildJob(t, s, map[string][]byte{"movie.mkv": randBytes(64_000)}, 4_000, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if _, err := e.Download(ctx, job, nil); !errors.Is(err, context.DeadlineExceeded) || s.bodies.Load() != 0 {
		t.Fatalf("got %v after %d articles with no slot free, want the deadline and no articles", err, s.bodies.Load())
	}

	<-e.transfers

	res, err := e.Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(job.Dir, res.Files[0].Path))
	if err != nil || !bytes.Equal(got, files["movie.mkv"]) {
		t.Fatalf("got err = %v or different content once the slot was free", err)
	}

	if n := len(e.transfers); n != 0 {
		t.Fatalf("got %d transfer slots still taken, want all returned", n)
	}
}

// A slot is taken only with a connection in hand, so a download stuck
// dialling a provider that hangs holds none, and a download on a working
// provider still finishes.
func TestAHungProviderHoldsNoTransferSlot(t *testing.T) {
	hung := newStallServer(t)
	good := newFakeNNTP(t, "", "")

	e := New(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = e.Close() })

	e.transfers = make(chan struct{}, 1)
	e.SetProvider("hung", hung.provider(1))
	e.SetProvider("good", good.provider("", "", 4))

	stuck, _ := buildJob(t, good, map[string][]byte{"a.mkv": randBytes(8_000)}, 4_000, nil)
	stuck.SourceID = "hung"

	job, files := buildJob(t, good, map[string][]byte{"b.mkv": randBytes(64_000)}, 4_000, nil)
	job.SourceID = "good"

	stuckCtx, stopStuck := context.WithCancel(context.Background())
	defer stopStuck()

	stuckDone := make(chan error, 1)
	go func() {
		_, err := e.Download(stuckCtx, stuck, nil)
		stuckDone <- err
	}()

	select {
	case <-hung.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck download never dialled the hung provider")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := e.Download(ctx, job, nil)
	if err != nil {
		t.Fatalf("got %v while another download waits on a hung provider, want the download to finish", err)
	}

	if got, err := os.ReadFile(filepath.Join(job.Dir, res.Files[0].Path)); err != nil || !bytes.Equal(got, files["b.mkv"]) {
		t.Fatalf("got err = %v or different content", err)
	}

	stopStuck()
	hung.close()

	if err := <-stuckDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v from the stuck download, want it cancelled", err)
	}
}

func TestMissingArticleIsReportedNotFatal(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(50_000)}, 10_000, map[string]bool{"a.bin-3@test": true})
	job.Notes = &engine.Notes{}

	res, err := newTestEngine(t, s.provider("", "", 2)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 1 || len(res.Files) != 1 || res.Files[0].Size != 50_000 {
		t.Fatalf("result = %+v", res)
	}

	notes := job.Notes.Drain()
	if len(notes) != 1 || notes[0].GetCode() != "usenet.articles_missing" || notes[0].GetAttrs()["count"] != "1" {
		t.Fatalf("got notes %v, want one note for the missing article", notes)
	}
}

func TestBadCredentialsFailFast(t *testing.T) {
	s := newFakeNNTP(t, "u", "right")
	job, _ := buildJob(t, s, map[string][]byte{"a.bin": randBytes(30_000)}, 10_000, nil)

	_, err := newTestEngine(t, s.provider("u", "wrong", 2)).Download(context.Background(), job, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_CLIENT || f.Retryable {
		t.Fatalf("err = %v, want non-retryable CLIENT failure", err)
	}
}

func TestResumeSkipsCheckpointedSegments(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, files := buildJob(t, s, map[string][]byte{"a.bin": randBytes(80_000)}, 10_000, nil)
	e := newTestEngine(t, s.provider("", "", 1))

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

	if _, err := newTestEngine(t, s.provider("", "", 2)).Download(ctx, job, nil); !errors.Is(err, context.Canceled) {
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

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("provider", "provider", p)

	if out := logged.String(); strings.Contains(out, "p@ss") || !strings.Contains(out, `"host":"news.example.com"`) {
		t.Fatalf("got log line %s, want the host and no password", out)
	}
}

func TestLongNamesLeaveRoomForSuffix(t *testing.T) {
	long := strings.Repeat("a", 300) + ".mkv"
	used := map[string]bool{}

	a := uniqueName(SafeName(long, 0), used)
	b := uniqueName(SafeName(long, 1), used)

	if a == b || len(b) > maxNameBytes || strings.HasSuffix(SafeName(strings.Repeat("b", 238)+". x", 0), ".") {
		t.Fatalf("a=%d bytes b=%d bytes, distinct=%v", len(a), len(b), a != b)
	}

	if uniqueName("Café.txt", used) == "" || uniqueName("Café.txt", used) == "Café.txt" {
		t.Fatal("NFC/NFD forms of the same name were not treated as a collision")
	}
}

func TestNZBLimitsAreStreamed(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<nzb><file subject="&quot;a&quot;"><segments>`)

	for i := range maxNZBSegments + 1 {
		fmt.Fprintf(&b, `<segment bytes="1" number="%d">m%d@x</segment>`, i+1, i)
	}

	b.WriteString(`</segments></file></nzb>`)

	if _, err := ParseNZB(strings.NewReader(b.String())); err == nil || !strings.Contains(err.Error(), "segments") {
		t.Fatalf("err = %v, want segment limit error", err)
	}

	if _, err := ParseNZB(strings.NewReader(`<nzb><file subject="x"><file></file></file></nzb>`)); err == nil {
		t.Fatal("nested <file> accepted")
	}
}

// M2: an NZB file named like the payload must not overwrite it.
func TestPayloadCannotBeOverwritten(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	job, _ := buildJob(t, s, map[string][]byte{"payload.nzb": randBytes(5000)}, 10_000, nil)

	before, _ := os.ReadFile(job.PayloadPath)

	res, err := newTestEngine(t, s.provider("", "", 1)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(job.PayloadPath)
	if !bytes.Equal(before, after) {
		t.Fatal("payload was overwritten by an NZB file")
	}

	if res.Files[0].Path != "data/payload.nzb" {
		t.Fatalf("output path = %q, want data/payload.nzb", res.Files[0].Path)
	}
}

// M3: a part claiming an absurd file size is discarded, not written.
func TestInflatedYEncSizeIsDiscarded(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	dir := t.TempDir()

	huge := int64(1) << 62
	s.add("a@x", EncodeYEnc("a.bin", huge, huge-4, []byte("evil!"), 1, 1))

	nzb := `<nzb><file subject="&quot;a.bin&quot;"><segments><segment bytes="100" number="1">a@x</segment></segments></file></nzb>`
	if err := os.WriteFile(filepath.Join(dir, "payload.nzb"), []byte(nzb), 0o600); err != nil {
		t.Fatal(err)
	}

	job := engine.Job{ID: "h", Dir: dir, PayloadPath: filepath.Join(dir, "payload.nzb")}

	res, err := newTestEngine(t, s.provider("", "", 1)).Download(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 1 || res.Files[0].Size != 0 {
		t.Fatalf("result = %+v, want the article discarded and no size claimed", res)
	}

	if fi, err := os.Stat(filepath.Join(dir, "data", "a.bin")); err == nil && fi.Size() > 0 {
		t.Fatalf("file grew to %d bytes", fi.Size())
	}
}

func TestPartsMustAgreeOnFileSize(t *testing.T) {
	s := newFakeNNTP(t, "", "")
	dir := t.TempDir()

	s.add("p1@x", EncodeYEnc("a.bin", 20, 1, []byte("0123456789"), 1, 2))
	s.add("p2@x", EncodeYEnc("a.bin", 30, 11, []byte("abcdefghij"), 2, 2))

	nzb := `<nzb><file subject="&quot;a.bin&quot;"><segments>` +
		`<segment bytes="200" number="1">p1@x</segment><segment bytes="200" number="2">p2@x</segment>` +
		`</segments></file></nzb>`
	if err := os.WriteFile(filepath.Join(dir, "payload.nzb"), []byte(nzb), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := newTestEngine(t, s.provider("", "", 1)).Download(context.Background(), engine.Job{ID: "c", Dir: dir, PayloadPath: filepath.Join(dir, "payload.nzb")}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if res.MissingArticles != 1 || res.Files[0].Size != 20 {
		t.Fatalf("result = %+v, want the disagreeing part discarded and size 20", res)
	}
}

func TestProviderURLSafety(t *testing.T) {
	if _, err := ParseProviderURL("nntp://user:pw@news.example.com"); err == nil {
		t.Fatal("plaintext credentials to a remote host were accepted")
	}

	if _, err := ParseProviderURL("nntp://user:pw@127.0.0.1:1119"); err != nil {
		t.Fatalf("loopback plaintext refused: %v", err)
	}

	_, err := ParseProviderURL("nntps://user:S3cr%zz@news.example.com")
	if err == nil || strings.Contains(err.Error(), "S3cr") {
		t.Fatalf("err = %v, want an error that does not echo the password", err)
	}
}

func TestOverlongServerLineIsRejected(t *testing.T) {
	r, w := net.Pipe()
	defer func() { _ = r.Close() }()

	go func() {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxLineBytes+10))
		_ = w.Close()
	}()

	c := &lineLimitConn{Conn: r}
	buf := make([]byte, 4096)

	for {
		_, err := c.Read(buf)
		if errors.Is(err, errLineTooLong) {
			return
		}

		if err != nil {
			t.Fatalf("err = %v, want errLineTooLong", err)
		}
	}
}
