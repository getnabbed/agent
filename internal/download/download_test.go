package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

var quiet = slog.New(slog.DiscardHandler)

// fakeEngine blocks each Download until released or cancelled.
type fakeEngine struct {
	release chan error
	active  atomic.Int32
	maxSeen atomic.Int32
	calls   atomic.Int32
	started chan string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{release: make(chan error, 16), started: make(chan string, 16)}
}

func (f *fakeEngine) Protocol() pb.Protocol { return pb.Protocol_PROTOCOL_TORRENT }
func (f *fakeEngine) Available() bool       { return true }

func (f *fakeEngine) Download(ctx context.Context, job engine.Job, progress func(engine.Progress)) (*engine.Result, error) {
	f.calls.Add(1)

	n := f.active.Add(1)
	defer f.active.Add(-1)

	for {
		cur := f.maxSeen.Load()
		if n <= cur || f.maxSeen.CompareAndSwap(cur, n) {
			break
		}
	}

	progress(engine.Progress{Bytes: 5, Total: 10, SpeedBps: 1})
	f.started <- job.ID

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-f.release:
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(filepath.Join(job.Dir, "out.bin"), []byte("data"), 0o600); err != nil {
			return nil, err
		}

		return &engine.Result{Files: []engine.File{{Path: "out.bin", Size: 4}}}, nil
	}
}

type okImporter struct {
	imported atomic.Int32
	// failNext makes the next import fail retryably, as with a missing root.
	failNext atomic.Bool
}

func (o *okImporter) Import(_ context.Context, _ engine.Job, _ *engine.Result, d *pb.Download) (*pb.ImportCompleted, error) {
	if o.failNext.CompareAndSwap(true, false) {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, true, "library root is not set up")
	}

	o.imported.Add(1)

	return &pb.ImportCompleted{DownloadId: d.GetId(), RootId: "root-1"}, nil
}

// memFetcher serves payloads from memory.
type memFetcher struct{ data map[string][]byte }

func (m memFetcher) Fetch(_ context.Context, id string, w io.Writer) error {
	b, ok := m.data[id]
	if !ok {
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "not found")
	}

	_, err := w.Write(b)

	return err
}

type telemetrySink struct{ n atomic.Int32 }

func (t *telemetrySink) SendTelemetry(string, *pb.Telemetry) { t.n.Add(1) }

type harness struct {
	t   *testing.T
	st  *store.Store
	m   *Manager
	eng *fakeEngine
	imp *okImporter
	tel *telemetrySink
}

var payload = []byte("d8:announce0:4:infod4:name1:xee")

func newHarness(t *testing.T, maxConcurrent int) *harness {
	t.Helper()

	st, err := store.Open(context.Background(), t.TempDir(), "agent-1", quiet)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	h := &harness{t: t, st: st, eng: newFakeEngine(), imp: &okImporter{}, tel: &telemetrySink{}}

	h.m, err = New(Config{
		Store: st, Engines: []engine.Engine{h.eng},
		Fetcher:  memFetcher{data: map[string][]byte{"p1": payload}},
		Importer: h.imp, Dir: filepath.Join(t.TempDir(), "downloads"),
		MaxConcurrent: maxConcurrent, Log: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}

	h.m.SetTelemetry(h.tel)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { _ = h.m.Run(ctx); close(done) }()

	t.Cleanup(func() { cancel(); <-done })

	return h
}

func torrentDownload(id string) *pb.Download {
	sum := sha256.Sum256(payload)

	return &pb.Download{Id: id, AcquisitionId: "acq-" + id, Release: &pb.Release{
		Protocol: pb.Protocol_PROTOCOL_TORRENT, Title: "t",
		Payload: &pb.Release_PayloadRef{PayloadRef: &pb.PayloadRef{Id: "p1", Sha256: sum[:], Size: uint64(len(payload)), Kind: pb.PayloadKind_PAYLOAD_KIND_TORRENT}},
	}}
}

func (h *harness) start(d *pb.Download) error {
	err := h.st.Update(context.Background(), func(tx *store.Tx) error {
		return h.m.Start(context.Background(), tx, &pb.StartDownload{Download: d})
	})
	h.m.Kick()

	return err
}

func (h *harness) act(a Action, id string) error {
	err := h.st.Update(context.Background(), func(tx *store.Tx) error { return h.m.Act(context.Background(), tx, a, id) })
	h.m.Kick()

	return err
}

func (h *harness) state(id string) State {
	h.t.Helper()

	r, err := get(context.Background(), h.st.DB(), id)
	if err != nil {
		h.t.Fatal(err)
	}

	return r.State
}

func (h *harness) waitState(id string, want State) {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for h.state(id) != want {
		if time.Now().After(deadline) {
			h.t.Fatalf("download %s is %s, want %s", id, h.state(id), want)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// waitStarted waits for the engine to start the single download "d1".
func (h *harness) waitStarted() {
	h.t.Helper()

	select {
	case got := <-h.eng.started:
		if got != "d1" {
			h.t.Fatalf("engine started %s, want d1", got)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("engine never started d1")
	}
}

// updates returns the DownloadUpdate states in the outbox for id, in order.
func (h *harness) updates(id string) []State {
	_, events, _, err := h.st.PendingEvents(context.Background(), 0, 1000)
	if err != nil {
		h.t.Fatal(err)
	}

	var out []State

	for _, ev := range events {
		if u := ev.GetDownloadUpdate(); u != nil && u.GetDownloadId() == id {
			out = append(out, u.GetState())
		}
	}

	return out
}

func TestHappyPathWalksEveryStage(t *testing.T) {
	h := newHarness(t, 2)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()
	h.eng.release <- nil
	h.waitState("d1", Done)

	want := []State{Queued, Fetching, Downloading, Assembling, Importing, Done}
	if got := h.updates("d1"); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	if h.imp.imported.Load() != 1 || h.tel.n.Load() == 0 {
		t.Fatalf("imported=%d telemetry=%d", h.imp.imported.Load(), h.tel.n.Load())
	}

	// The importer's ImportCompleted is queued once, with the move to DONE.
	_, events, _, err := h.st.PendingEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	var completed, doneAt, completedAt int

	for i, ev := range events {
		if ic := ev.GetImportCompleted(); ic != nil && ic.GetDownloadId() == "d1" {
			completed, completedAt = completed+1, i
		}

		if ev.GetDownloadUpdate().GetState() == Done {
			doneAt = i
		}
	}

	if completed != 1 || completedAt > doneAt {
		t.Fatalf("got %d ImportCompleted events (at %d, DONE at %d), want one queued before DONE", completed, completedAt, doneAt)
	}

	got, _ := os.ReadFile(filepath.Join(h.m.workDir("d1"), "payload.torrent"))
	if !bytes.Equal(got, payload) {
		t.Fatal("payload not fetched into the work dir")
	}
}

func TestStartIsIdempotentAndValidated(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	if err := h.start(torrentDownload("d1")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate start err = %v, want ErrAlreadyExists", err)
	}

	bad := torrentDownload("d2")
	bad.Release.GetPayloadRef().Sha256 = []byte("short")

	if err := h.start(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad sha err = %v, want ErrInvalidArgument", err)
	}

	usenet := torrentDownload("d3")
	usenet.Release.Protocol = pb.Protocol_PROTOCOL_USENET
	usenet.Release.GetPayloadRef().Kind = pb.PayloadKind_PAYLOAD_KIND_NZB

	if err := h.start(usenet); !errors.Is(err, ErrProtocolUnavailable) {
		t.Fatalf("usenet without engine err = %v, want ErrProtocolUnavailable", err)
	}
}

func TestPauseResume(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()

	if err := h.act(Pause, "d1"); err != nil {
		t.Fatal(err)
	}

	h.waitState("d1", Paused)

	if err := h.act(Pause, "d1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pause twice err = %v, want ErrInvalidTransition", err)
	}

	if err := h.act(Resume, "d1"); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()
	h.eng.release <- nil
	h.waitState("d1", Done)

	if h.eng.calls.Load() != 2 {
		t.Fatalf("engine calls = %d, want 2", h.eng.calls.Load())
	}
}

func TestCancelRemovesWorkDir(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()

	if err := h.act(Cancel, "d1"); err != nil {
		t.Fatal(err)
	}

	h.waitState("d1", Cancelled)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(h.m.workDir("d1")); errors.Is(err, os.ErrNotExist) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("work dir not removed after cancel")
		}

		time.Sleep(5 * time.Millisecond)
	}

	if err := h.act(Resume, "d1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume cancelled err = %v, want ErrInvalidTransition", err)
	}
}

func TestFailureThenRetry(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()
	h.eng.release <- engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, true, "dead torrent")
	h.waitState("d1", Failed)

	got := h.updates("d1")
	if got[len(got)-1] != Failed {
		t.Fatalf("last event = %v, want FAILED", got[len(got)-1])
	}

	if err := h.act(Retry, "d1"); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()
	h.eng.release <- nil
	h.waitState("d1", Done)

	r, _ := get(context.Background(), h.st.DB(), "d1")
	if r.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", r.Attempts)
	}
}

func TestUnknownDownload(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.act(Pause, "nope"); !errors.Is(err, ErrUnknownDownload) {
		t.Fatalf("err = %v, want ErrUnknownDownload", err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	h := newHarness(t, 2)

	for _, id := range []string{"a", "b", "c", "d"} {
		if err := h.start(torrentDownload(id)); err != nil {
			t.Fatal(err)
		}
	}

	for range 4 {
		select {
		case <-h.eng.started:
		case <-time.After(5 * time.Second):
			t.Fatal("downloads did not all start")
		}

		h.eng.release <- nil
	}

	for _, id := range []string{"a", "b", "c", "d"} {
		h.waitState(id, Done)
	}

	if h.eng.maxSeen.Load() > 2 {
		t.Fatalf("max concurrent engine runs = %d, limit 2", h.eng.maxSeen.Load())
	}
}

// A download left mid-stage by a crash is picked up by the next Run.
func TestRecoversInterruptedDownload(t *testing.T) {
	h := newHarness(t, 1)

	err := h.st.Update(context.Background(), func(tx *store.Tx) error {
		if err := insert(context.Background(), tx, torrentDownload("d1")); err != nil {
			return err
		}

		_, err := transition(context.Background(), tx, "d1", Queued, Downloading, nil)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(h.m.workDir("d1"), 0o750); err != nil {
		t.Fatal(err)
	}

	h.m.Kick()
	h.waitStarted()
	h.eng.release <- nil
	h.waitState("d1", Done)
}

func TestDefaultImporterFailsRetryably(t *testing.T) {
	_, err := UnimplementedImporter{}.Import(context.Background(), engine.Job{}, &engine.Result{}, nil)

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_IMPORT || !f.Retryable {
		t.Fatalf("err = %v, want retryable IMPORT failure", err)
	}

	_, err = PassthroughAssembler{}.Assemble(context.Background(), engine.Job{}, &engine.Result{MissingArticles: 2})
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_INCOMPLETE {
		t.Fatalf("err = %v, want INCOMPLETE failure", err)
	}
}

type staticTokens struct{ invalidated atomic.Int32 }

func (s *staticTokens) Token(context.Context) (auth.AccessToken, error) {
	return auth.AccessToken{Raw: "tok"}, nil
}

func (s *staticTokens) Invalidate(string) { s.invalidated.Add(1) }

func TestHTTPFetcherVerifiesPayload(t *testing.T) {
	body := []byte("<nzb>payload</nzb>")
	sum := sha256.Sum256(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/api/agent/payloads/ok":
			_, _ = w.Write(body)
		case "/api/agent/payloads/gone":
			w.WriteHeader(http.StatusGone)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api, _ := url.Parse(srv.URL)
	f := &HTTPFetcher{API: api, Client: srv.Client(), Tokens: &staticTokens{}}
	dir := t.TempDir()

	ref := &pb.PayloadRef{Id: "ok", Sha256: sum[:], Size: uint64(len(body)), Kind: pb.PayloadKind_PAYLOAD_KIND_NZB}
	if err := fetchPayload(context.Background(), f, ref, dir, "payload.nzb"); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "payload.nzb"))
	if !bytes.Equal(got, body) {
		t.Fatal("payload content mismatch")
	}

	tampered := &pb.PayloadRef{Id: "ok", Sha256: make([]byte, 32), Size: uint64(len(body))}

	var fail *engine.Failure
	if err := fetchPayload(context.Background(), f, tampered, t.TempDir(), "p"); !errors.As(err, &fail) || fail.Retryable {
		t.Fatalf("hash mismatch err = %v, want non-retryable failure", err)
	}

	gone := &pb.PayloadRef{Id: "gone", Sha256: sum[:], Size: uint64(len(body))}
	if err := fetchPayload(context.Background(), f, gone, t.TempDir(), "p"); !errors.As(err, &fail) || fail.Retryable || fail.Class != pb.FailureClass_FAILURE_CLASS_PAYLOAD {
		t.Fatalf("410 err = %v, want non-retryable PAYLOAD failure", err)
	}

	oversized := &pb.PayloadRef{Id: "ok", Sha256: sum[:], Size: 4}
	if err := fetchPayload(context.Background(), f, oversized, t.TempDir(), "p"); !errors.As(err, &fail) || fail.Retryable {
		t.Fatalf("oversize err = %v, want non-retryable failure", err)
	}
}

func TestTargetTable(t *testing.T) {
	all := []State{Queued, Fetching, Downloading, Assembling, Importing, Done, Paused, Failed, Cancelled}
	allowed := map[Action][]State{
		Pause:  {Queued, Fetching, Downloading},
		Resume: {Paused},
		Retry:  {Failed},
		Cancel: {Queued, Fetching, Downloading, Assembling, Importing, Paused, Failed},
	}

	for action, froms := range allowed {
		ok := map[State]bool{}
		for _, s := range froms {
			ok[s] = true
		}

		for _, s := range all {
			if _, got := Target(action, s); got != ok[s] {
				t.Errorf("Target(%s, %s) allowed = %v, want %v", action, s, got, ok[s])
			}
		}
	}
}

func TestUnfinishedLimitAndSpecSize(t *testing.T) {
	h := newHarness(t, 1)

	big := torrentDownload("big")
	big.Release.Title = strings.Repeat("x", maxSpecBytes)

	if err := h.start(big); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized spec err = %v, want ErrInvalidArgument", err)
	}

	err := h.st.Update(context.Background(), func(tx *store.Tx) error {
		for i := range maxUnfinished {
			d := torrentDownload(fmt.Sprintf("f%d", i))
			if err := insert(context.Background(), tx, d); err != nil {
				return err
			}

			if _, err := transition(context.Background(), tx, d.GetId(), Queued, Failed, nil); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.start(torrentDownload("one-too-many")); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v, want ErrLimitExceeded", err)
	}
}

func TestCancelledCleanupIsMarkedAndGarbageCollected(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()

	if err := h.act(Cancel, "d1"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var removed int
		_ = h.st.DB().QueryRow("SELECT data_removed FROM downloads WHERE id = 'd1'").Scan(&removed)

		if removed == 1 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("cancelled download was never marked cleaned")
		}

		time.Sleep(5 * time.Millisecond)
	}

	if n, err := gcFinished(context.Background(), h.st, time.Now().Add(time.Hour), time.Now()); err != nil || n != 1 {
		t.Fatalf("gc = %d, %v; want 1 row", n, err)
	}

	if _, err := get(context.Background(), h.st.DB(), "d1"); !errors.Is(err, errNotFound) {
		t.Fatalf("row still present after GC: %v", err)
	}
}

func TestRedactRemovesHostPaths(t *testing.T) {
	h := newHarness(t, 1)

	got := h.m.redact("d1", "open "+filepath.Join(h.m.workDir("d1"), "data", "x")+": denied; also "+h.m.cfg.Dir)
	if strings.Contains(got, h.m.cfg.Dir) {
		t.Fatalf("host path leaked: %s", got)
	}
}

func TestPayloadRedirectPolicy(t *testing.T) {
	body := []byte("payload")
	sum := sha256.Sum256(body)

	var sawAuth atomic.Bool

	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}

		_, _ = w.Write(body)
	}))
	defer other.Close()

	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/payloads/elsewhere":
			http.Redirect(w, r, other.URL+"/blob", http.StatusFound)
		case "/api/agent/payloads/downgrade":
			http.Redirect(w, r, "http://"+r.Host+"/plain", http.StatusFound)
		}
	}))
	defer api.Close()

	// One client trusting both test servers' certificates.
	pool := api.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	pool.AddCert(other.Certificate())

	u, _ := url.Parse(api.URL)
	f := &HTTPFetcher{API: u, Client: api.Client(), Tokens: &staticTokens{}}

	ref := &pb.PayloadRef{Id: "elsewhere", Sha256: sum[:], Size: uint64(len(body))}
	if err := fetchPayload(context.Background(), f, ref, t.TempDir(), "p"); err != nil {
		t.Fatal(err)
	}

	if sawAuth.Load() {
		t.Fatal("bearer token was sent to another host")
	}

	ref.Id = "downgrade"
	if err := fetchPayload(context.Background(), f, ref, t.TempDir(), "p"); err == nil {
		t.Fatal("https to http redirect was followed")
	}
}

func TestRetryOfAFailedImportResumesAtImport(t *testing.T) {
	h := newHarness(t, 1)
	h.imp.failNext.Store(true)

	if err := h.start(torrentDownload("d1")); err != nil {
		t.Fatal(err)
	}

	h.waitStarted()
	h.eng.release <- nil
	h.waitState("d1", Failed)

	if err := h.act(Retry, "d1"); err != nil {
		t.Fatal(err)
	}

	h.waitState("d1", Done)

	if h.eng.calls.Load() != 1 || h.imp.imported.Load() != 1 {
		t.Fatalf("got %d engine calls and %d imports, want the download kept and only the import redone", h.eng.calls.Load(), h.imp.imported.Load())
	}

	got := h.updates("d1")
	want := []State{Queued, Fetching, Downloading, Assembling, Importing, Failed, Importing, Done}

	if !slices.Equal(got, want) {
		t.Fatalf("got events %v, want %v", got, want)
	}
}
