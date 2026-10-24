package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const reconcileInterval = 5 * time.Second

// Telemetry receives lossy progress updates. *link.Client implements it.
type Telemetry interface {
	SendTelemetry(key string, t *pb.Telemetry)
}

// Config configures a Manager.
type Config struct {
	Store   *store.Store
	Engines []engine.Engine
	Fetcher PayloadFetcher
	// Assembler and Importer default to stages that pass completed data
	// through and report import as not implemented.
	Assembler Assembler
	Importer  Importer
	// Dir holds one work directory per download.
	Dir string
	// MaxConcurrent bounds downloads in running states. Default 3.
	MaxConcurrent int
	Log           *slog.Logger
}

// Manager owns the download runners.
type Manager struct {
	cfg     Config
	engines map[pb.Protocol]engine.Engine
	log     *slog.Logger

	kick      chan struct{}
	telemetry atomic.Pointer[Telemetry]

	mu      sync.Mutex
	running map[string]*runner
	wg      sync.WaitGroup
}

type runner struct {
	cancel context.CancelFunc
}

// New returns a Manager. Call Run to start it.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil || cfg.Fetcher == nil || cfg.Dir == "" {
		return nil, errors.New("download: Store, Fetcher and Dir are required")
	}

	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}

	if cfg.Assembler == nil {
		cfg.Assembler = PassthroughAssembler{}
	}

	if cfg.Importer == nil {
		cfg.Importer = UnimplementedImporter{}
	}

	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("download dir: %w", err)
	}

	m := &Manager{
		cfg:     cfg,
		engines: map[pb.Protocol]engine.Engine{},
		log:     cfg.Log.With("component", "download"),
		kick:    make(chan struct{}, 1),
		running: map[string]*runner{},
	}

	for _, e := range cfg.Engines {
		m.engines[e.Protocol()] = e
	}

	return m, nil
}

// SetTelemetry sets where progress goes. It may be called after Run.
func (m *Manager) SetTelemetry(t Telemetry) { m.telemetry.Store(&t) }

// Capabilities lists the protocols this agent can download now, for Hello.
func (m *Manager) Capabilities() []string {
	var out []string

	for _, p := range []pb.Protocol{pb.Protocol_PROTOCOL_USENET, pb.Protocol_PROTOCOL_TORRENT} {
		if e, ok := m.engines[p]; ok && e.Available() {
			out = append(out, "download."+protocolName(p))
		}
	}

	return out
}

func protocolName(p pb.Protocol) string {
	switch p {
	case pb.Protocol_PROTOCOL_USENET:
		return "usenet"
	case pb.Protocol_PROTOCOL_TORRENT:
		return "torrent"
	}

	return "unknown"
}

// Kick asks the scheduler to reconcile now, for example after a command
// committed.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run schedules downloads until ctx ends, then stops every runner. Work in
// progress stays in its state and resumes on the next Run.
func (m *Manager) Run(ctx context.Context) error {
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()

	defer m.wg.Wait()
	defer m.stopAll()

	for {
		m.reconcile(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-m.kick:
		case <-t.C:
		}
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.running {
		r.cancel()
	}
}

// reconcile makes the set of runners match the database: it stops runners
// whose download left the running states, starts runners up to the limit,
// and removes work directories of cancelled downloads. Every query is
// bounded, so the cost of a pass does not grow with history.
func (m *Manager) reconcile(ctx context.Context) {
	db := m.cfg.Store.DB()

	m.mu.Lock()
	running := make([]string, 0, len(m.running))
	for id := range m.running {
		running = append(running, id)
	}
	m.mu.Unlock()

	// Stop runners whose download a command moved out of a running state.
	for _, id := range running {
		r, err := get(ctx, db, id)
		if err != nil || !Running(r.State) {
			m.stop(id)
		}
	}

	cands, err := runnable(ctx, db, m.cfg.MaxConcurrent*2)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Error("list runnable downloads", "err", err)
		}

		return
	}

	m.mu.Lock()
	for _, c := range cands {
		if len(m.running) >= m.cfg.MaxConcurrent {
			break
		}

		if _, ok := m.running[c.ID]; ok {
			continue
		}

		rctx, cancel := context.WithCancel(ctx)
		m.running[c.ID] = &runner{cancel: cancel}
		m.wg.Add(1)

		go m.run(rctx, c.ID)
	}

	busy := make(map[string]bool, len(m.running))
	for id := range m.running {
		busy[id] = true
	}
	m.mu.Unlock()

	cancelled, err := uncleanedCancelled(ctx, db, cleanupBatch)
	if err != nil {
		return
	}

	for _, id := range cancelled {
		if !busy[id] {
			m.cleanup(ctx, id)
		}
	}
}

// cleanupBatch bounds how many cancelled downloads one pass cleans.
const cleanupBatch = 32

func (m *Manager) stop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if r, ok := m.running[id]; ok {
		r.cancel()
	}
}

// cleanup removes a cancelled download's work directory and marks it done.
func (m *Manager) cleanup(ctx context.Context, id string) {
	if err := os.RemoveAll(m.workDir(id)); err != nil {
		m.log.Warn("remove cancelled download data", "download_id", id, "err", err)
		return
	}

	if err := markDataRemoved(ctx, m.cfg.Store.DB(), id); err != nil {
		m.log.Warn("record cancelled download cleanup", "download_id", id, "err", err)
		return
	}

	m.log.Info("removed cancelled download data", "download_id", id)
}

// GC deletes finished rows that no longer matter: cancelled downloads
// whose data is gone, after a week, and completed downloads after 30 days.
// Failed downloads are kept for RetryDownload and count against the
// unfinished-download limit until the cloud cancels them.
func (m *Manager) GC(ctx context.Context) {
	n, err := gcFinished(ctx, m.cfg.Store, time.Now().Add(-7*24*time.Hour), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		m.log.Warn("garbage-collect downloads", "err", err)
		return
	}

	if n > 0 {
		m.log.Debug("garbage-collected finished downloads", "count", n)
	}
}

// workDir derives a directory name from the download id without trusting
// its characters.
func (m *Manager) workDir(id string) string {
	sum := sha256.Sum256([]byte(id))

	return filepath.Join(m.cfg.Dir, hex.EncodeToString(sum[:16]))
}

func payloadName(kind pb.PayloadKind) string {
	if kind == pb.PayloadKind_PAYLOAD_KIND_TORRENT {
		return "payload.torrent"
	}

	return "payload.nzb"
}

// run advances one download through its stages until it settles, a
// command moves it, or ctx ends.
func (m *Manager) run(ctx context.Context, id string) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()

		m.Kick()
	}()

	log := m.log.With("download_id", id)

	for {
		r, err := get(context.WithoutCancel(ctx), m.cfg.Store.DB(), id)
		if err != nil {
			log.Error("load download", "err", err)
			return
		}

		if !Running(r.State) || ctx.Err() != nil {
			return
		}

		log.Debug("stage", "state", r.State)

		res, err := m.safeStage(ctx, r)
		if err != nil {
			if ctx.Err() != nil {
				return // paused, cancelled or shutting down: state stays as the command left it
			}

			f := engine.AsFailure(err, defaultClass(r.State))
			f.Detail = m.redact(r.ID, f.Detail)
			log.Warn("download failed", "state", r.State, "class", f.Class, "retryable", f.Retryable, "detail", f.Detail)

			m.update(ctx, func(tx *store.Tx) error {
				_, err := transition(ctx, tx, id, r.State, Failed, f)
				return err
			})

			return
		}

		to, _ := next(r.State)

		var moved bool
		m.update(ctx, func(tx *store.Tx) error {
			if res != nil {
				if err := setResult(ctx, tx, id, res); err != nil {
					return err
				}
			}

			var err error
			moved, err = transition(ctx, tx, id, r.State, to, nil)

			return err
		})

		if !moved {
			return
		}
	}
}

func (m *Manager) update(ctx context.Context, fn func(*store.Tx) error) {
	// Record the outcome even if the runner is being stopped right now.
	if err := m.cfg.Store.Update(context.WithoutCancel(ctx), fn); err != nil {
		m.log.Error("record download state", "err", err)
	}
}

// stage does the work of r.State. A non-nil result is stored with the
// transition.
func (m *Manager) stage(ctx context.Context, r *row) (*engine.Result, error) {
	job := m.job(r)

	switch r.State {
	case Queued:
		return nil, os.MkdirAll(job.Dir, 0o750)
	case Fetching:
		ref := r.Spec.GetRelease().GetPayloadRef()
		if ref == nil {
			return nil, nil // magnet: nothing to fetch
		}

		return nil, fetchPayload(ctx, m.cfg.Fetcher, ref, job.Dir, payloadName(ref.GetKind()))
	case Downloading:
		e, ok := m.engines[r.Protocol]
		if !ok || !e.Available() {
			return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, true, "no %s engine available", protocolName(r.Protocol))
		}

		return e.Download(ctx, job, func(p engine.Progress) { m.progress(r.ID, p) })
	case Assembling:
		return m.cfg.Assembler.Assemble(ctx, job, resultOrEmpty(r))
	case Importing:
		return nil, m.cfg.Importer.Import(ctx, job, resultOrEmpty(r), r.Spec)
	}

	return nil, fmt.Errorf("no stage for state %s", r.State)
}

func resultOrEmpty(r *row) *engine.Result {
	if r.Result == nil {
		return &engine.Result{}
	}

	return r.Result
}

func (m *Manager) job(r *row) engine.Job {
	j := engine.Job{ID: r.ID, Dir: m.workDir(r.ID), SourceID: r.SourceID}

	rel := r.Spec.GetRelease()
	if ref := rel.GetPayloadRef(); ref != nil {
		j.PayloadPath = filepath.Join(j.Dir, payloadName(ref.GetKind()))
	} else {
		j.Magnet = rel.GetMagnet()
	}

	return j
}

func (m *Manager) progress(id string, p engine.Progress) {
	t := m.telemetry.Load()
	if t == nil {
		return
	}

	dp := &pb.DownloadProgress{DownloadId: id, Bytes: uint64(max(p.Bytes, 0)), SpeedBps: uint64(max(p.SpeedBps, 0))}
	if p.Total > 0 {
		dp.Percent = float32(100 * float64(p.Bytes) / float64(p.Total))
	}

	if p.SpeedBps > 0 && p.Total > p.Bytes {
		dp.EtaS = uint64((p.Total - p.Bytes) / p.SpeedBps)
	}

	(*t).SendTelemetry("progress/"+id, &pb.Telemetry{Kind: &pb.Telemetry_DownloadProgress{DownloadProgress: dp}})
}

// defaultClass classifies an unexpected error by the stage it came from.
func defaultClass(s State) pb.FailureClass {
	switch s {
	case Fetching:
		return pb.FailureClass_FAILURE_CLASS_PAYLOAD
	case Downloading:
		return pb.FailureClass_FAILURE_CLASS_CLIENT
	case Assembling:
		return pb.FailureClass_FAILURE_CLASS_REPAIR
	case Importing:
		return pb.FailureClass_FAILURE_CLASS_IMPORT
	}

	return pb.FailureClass_FAILURE_CLASS_DISK
}

// safeStage runs a stage and turns a panic into a non-retryable failure of
// this download, so one hostile input cannot take down the agent. Panics in
// goroutines an engine starts itself cannot be caught here; engines must
// validate input before handing it to libraries.
func (m *Manager) safeStage(ctx context.Context, r *row) (res *engine.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			m.log.Error("panic in download stage", "download_id", r.ID, "state", r.State,
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))

			res, err = nil, engine.Fail(defaultClass(r.State), false, "internal error while %s", stateName(r.State))
		}
	}()

	return m.stage(ctx, r)
}

// maxDetailBytes caps failure text sent to the cloud.
const maxDetailBytes = 512

// redact removes host paths from failure text before it leaves the host.
func (m *Manager) redact(id, detail string) string {
	detail = strings.ReplaceAll(detail, m.workDir(id), "<download>")
	detail = strings.ReplaceAll(detail, m.cfg.Dir, "<downloads>")

	if len(detail) > maxDetailBytes {
		detail = detail[:maxDetailBytes]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}

	return detail
}
