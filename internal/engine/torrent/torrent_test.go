package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// makeTorrent writes content under a temp dir and the matching .torrent
// into payloadDir. It returns the content dir and the metainfo.
func makeTorrent(t *testing.T, payloadDir string, files map[string][]byte, mutate func(*metainfo.Info)) (string, *metainfo.MetaInfo) {
	t.Helper()

	src := t.TempDir()
	root := filepath.Join(src, "release")

	for name, data := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	info := metainfo.Info{PieceLength: 32 << 10}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}

	if mutate != nil {
		mutate(&info)
	}

	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}

	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}

	f, err := os.Create(filepath.Join(payloadDir, "payload.torrent"))
	if err != nil {
		t.Fatal(err)
	}

	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	return src, mi
}

// seed starts a seeding client for files, optionally rate limited, and
// returns its address.
func seed(t *testing.T, payloadDir string, files map[string][]byte, uploadBps int) (peer string) {
	t.Helper()

	src, mi := makeTorrent(t, payloadDir, files, nil)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = src
	cfg.Seed = true
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.ListenPort = 0
	cfg.Slogger = slog.New(slog.DiscardHandler)

	if uploadBps > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(uploadBps), 64<<10)
	}

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cl.Close() })

	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}

	<-tor.GotInfo()

	if err := tor.VerifyDataContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	return fmt.Sprintf("127.0.0.1:%d", cl.LocalPort())
}

func newTestEngine(peer string) *Engine {
	// The test seeder is on loopback, which the default policy blocks.
	e := New(Config{Log: slog.New(slog.DiscardHandler), StallTimeout: 30 * time.Second, MetadataTimeout: 30 * time.Second, AllowLAN: true})
	e.cfg.noDHT = true
	e.cfg.peerAddrs = []string{peer}

	return e
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return b
}

func TestDownloadFromLocalSeeder(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"movie.mkv": randBytes(300 << 10), "subs/en.srt": randBytes(5 << 10)}
	peer := seed(t, dir, files, 0)

	e := newTestEngine(peer)
	defer func() { _ = e.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var sawProgress bool
	res, err := e.Download(ctx, engine.Job{ID: "t1", Dir: dir, PayloadPath: filepath.Join(dir, "payload.torrent")},
		func(p engine.Progress) { sawProgress = sawProgress || p.Total > 0 })
	if err != nil {
		t.Fatal(err)
	}

	if !sawProgress || len(res.Files) != 2 {
		t.Fatalf("progress=%v result=%+v", sawProgress, res)
	}

	for _, f := range res.Files {
		got, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			t.Fatal(err)
		}

		rel, _ := filepath.Rel(filepath.Join(dataDir, "release"), f.Path)
		if !bytes.Equal(got, files[filepath.ToSlash(rel)]) {
			t.Fatalf("%s: content mismatch", f.Path)
		}
	}
}

func TestUnsafePathIsRejected(t *testing.T) {
	dir := t.TempDir()
	// No seeder needed: the .torrent carries the file list, so the engine must
	// refuse it before any peer is involved.
	makeTorrent(t, dir, map[string][]byte{"a/b.bin": randBytes(40 << 10), "c.bin": randBytes(1 << 10)}, func(info *metainfo.Info) {
		info.Files[0].Path = []string{"..", "..", "escape.bin"}
	})

	e := newTestEngine("127.0.0.1:1")
	defer func() { _ = e.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Download(ctx, engine.Job{ID: "t2", Dir: dir, PayloadPath: filepath.Join(dir, "payload.torrent")}, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_PAYLOAD || f.Retryable {
		t.Fatalf("err = %v, want non-retryable PAYLOAD failure", err)
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.bin")); err == nil {
		t.Fatal("file written outside the work directory")
	}
}

func TestCancelAndResume(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"big.bin": randBytes(2 << 20)}
	peer := seed(t, dir, files, 512<<10) // ~4s, so progress is seen mid-download

	e := newTestEngine(peer)
	defer func() { _ = e.Close() }()

	job := engine.Job{ID: "t3", Dir: dir, PayloadPath: filepath.Join(dir, "payload.torrent")}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := e.Download(ctx, job, func(p engine.Progress) {
		if p.Bytes > 0 {
			cancel()
		}
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first run err = %v, want context.Canceled", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()

	res, err := e.Download(ctx2, job, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, res.Files[0].Path))
	if !bytes.Equal(got, files["big.bin"]) {
		t.Fatal("resumed content mismatch")
	}
}

func TestLANPeersBlockedByDefault(t *testing.T) {
	dir := t.TempDir()
	peer := seed(t, dir, map[string][]byte{"a.bin": randBytes(64 << 10)}, 0)

	e := New(Config{Log: slog.New(slog.DiscardHandler), StallTimeout: 2 * time.Second, MetadataTimeout: 5 * time.Second})
	e.cfg.noDHT = true
	e.cfg.peerAddrs = []string{peer}

	defer func() { _ = e.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Download(ctx, engine.Job{ID: "lan", Dir: dir, PayloadPath: filepath.Join(dir, "payload.torrent")}, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_INCOMPLETE {
		t.Fatalf("err = %v, want INCOMPLETE (loopback seeder must be unreachable)", err)
	}
}

// H1: a zero info hash used to panic inside anacrolix and kill the agent.
func TestZeroOrV2OnlyMagnetIsRejected(t *testing.T) {
	e := New(Config{Log: slog.New(slog.DiscardHandler), MetadataTimeout: time.Second})
	defer func() { _ = e.Close() }()

	for _, m := range []string{
		"magnet:?xt=urn:btih:0000000000000000000000000000000000000000",
		"magnet:?xt=urn:btmh:1220caf1e1c30e81cb361b9ee167c4aa64228a7fa4fa9f6105232b28ad099f3a302e",
	} {
		_, err := e.Download(context.Background(), engine.Job{ID: "z", Dir: t.TempDir(), Magnet: m}, nil)

		var f *engine.Failure
		if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_PAYLOAD || f.Retryable {
			t.Fatalf("%s: err = %v, want non-retryable PAYLOAD failure", m, err)
		}
	}

	// The engine is still usable afterwards (no lock left held).
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSameTorrentTwiceIsRefused(t *testing.T) {
	dir := t.TempDir()
	peer := seed(t, dir, map[string][]byte{"big.bin": randBytes(2 << 20)}, 256<<10)

	e := newTestEngine(peer)
	defer func() { _ = e.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	go func() {
		_, _ = e.Download(ctx, engine.Job{ID: "first", Dir: dir, PayloadPath: filepath.Join(dir, "payload.torrent")},
			func(engine.Progress) {
				select {
				case <-started:
				default:
					close(started)
				}
			})
	}()

	<-started

	other := t.TempDir()
	raw, _ := os.ReadFile(filepath.Join(dir, "payload.torrent"))
	_ = os.WriteFile(filepath.Join(other, "payload.torrent"), raw, 0o600)

	_, err := e.Download(ctx, engine.Job{ID: "second", Dir: other, PayloadPath: filepath.Join(other, "payload.torrent")}, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_CLIENT {
		t.Fatalf("err = %v, want CLIENT failure for duplicate torrent", err)
	}
}
