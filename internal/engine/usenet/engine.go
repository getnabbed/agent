package usenet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/netguard"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	// segmentAttempts bounds tries of one article on one provider after
	// connection errors. Missing or damaged articles move to the next
	// provider at once.
	segmentAttempts    = 3
	checkpointInterval = 5 * time.Second
	progressInterval   = time.Second
	janitorInterval    = 30 * time.Second
)

// ErrNoProvider means no usenet provider is configured.
var ErrNoProvider = errors.New("no usenet provider configured")

// Engine is the native usenet engine. It owns one connection pool per
// provider, shared by every download.
type Engine struct {
	timeout time.Duration
	backoff func(failures int) time.Duration
	log     *slog.Logger

	mu        sync.Mutex
	providers []*registered // priority order
	stop      chan struct{}
	stopped   chan struct{}
}

type registered struct {
	id   string
	pool *pool
}

// New returns an Engine with no providers. Call Close to release
// connections.
func New(log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}

	e := &Engine{timeout: 60 * time.Second, backoff: dialBackoff, log: log.With("engine", "usenet"), stop: make(chan struct{}), stopped: make(chan struct{})}

	go e.janitor()

	return e
}

// SetProvider adds or replaces provider id. Providers are tried in the
// order they were first added. Downloads in progress keep the pool they
// started with until they finish.
func (e *Engine) SetProvider(id string, p Provider) {
	pl := newPool(p, e.timeout, e.log)
	pl.backoff = e.backoff

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, r := range e.providers {
		if r.id == id {
			r.pool.close()
			r.pool = pl

			return
		}
	}

	e.providers = append(e.providers, &registered{id: id, pool: pl})
}

// RemoveProvider removes provider id.
func (e *Engine) RemoveProvider(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, r := range e.providers {
		if r.id == id {
			r.pool.close()
			e.providers = append(e.providers[:i], e.providers[i+1:]...)

			return
		}
	}
}

// Close closes every provider's connections.
func (e *Engine) Close() error {
	e.mu.Lock()
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}

	for _, r := range e.providers {
		r.pool.close()
	}
	e.mu.Unlock()

	<-e.stopped

	return nil
}

func (e *Engine) janitor() {
	defer close(e.stopped)

	t := time.NewTicker(janitorInterval)
	defer t.Stop()

	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.mu.Lock()
			for _, r := range e.providers {
				r.pool.closeIdle()
			}
			e.mu.Unlock()
		}
	}
}

// route returns the pools to try for a job, in order: the job's own source,
// the other primaries, then backups.
func (e *Engine) route(sourceID string) []*pool {
	e.mu.Lock()
	defer e.mu.Unlock()

	var first, primaries, backups []*pool

	for _, r := range e.providers {
		switch {
		case r.id == sourceID && !r.pool.p.Backup:
			first = append(first, r.pool)
		case r.pool.p.Backup:
			backups = append(backups, r.pool)
		default:
			primaries = append(primaries, r.pool)
		}
	}

	return append(append(first, primaries...), backups...)
}

// Protocol implements engine.Engine.
func (e *Engine) Protocol() pb.Protocol { return pb.Protocol_PROTOCOL_USENET }

// Available implements engine.Engine: at least one primary provider exists.
func (e *Engine) Available() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, r := range e.providers {
		if !r.pool.p.Backup {
			return true
		}
	}

	return false
}

type workItem struct {
	file, seg int
	segment   Segment
}

// Download implements engine.Engine.
func (e *Engine) Download(ctx context.Context, job engine.Job, progress func(engine.Progress)) (*engine.Result, error) {
	route := e.route(job.SourceID)

	workers := 0
	for _, pl := range route {
		if !pl.p.Backup {
			workers += max(pl.p.Connections, 1)
		}
	}

	if workers == 0 {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, false, "%v", ErrNoProvider)
	}

	root, err := os.OpenRoot(job.Dir)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "open work dir: %v", err)
	}

	defer func() { _ = root.Close() }()

	nzb, err := e.readNZB(root, job.PayloadPath)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "%v", err)
	}

	ckpt, err := loadCheckpoint(root)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "read checkpoint: %v", err)
	}

	// Article data goes in its own directory so that no NZB file name can
	// collide with the payload or the checkpoint.
	if err := root.MkdirAll(dataDir, 0o750); err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "create data dir: %v", err)
	}

	data, err := root.OpenRoot(dataDir)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "open data dir: %v", err)
	}

	defer func() { _ = data.Close() }()

	d := &download{
		e: e, root: root, data: data, nzb: nzb, route: route, workers: workers, ckpt: ckpt,
		files: map[int]*os.File{},
		sizes: map[int]int64{},
	}

	for i := range nzb.Files {
		if s := ckpt.size(i); s > 0 {
			d.sizes[i] = s
		}
	}

	return d.run(ctx, progress)
}

// dataDir holds downloaded files inside the work directory.
const dataDir = "data"

// absMaxFileSize bounds any single file when the NZB gives no sizes.
const absMaxFileSize = 1 << 40

// errBadPart marks an article whose yEnc headers contradict the NZB or
// other parts of the same file. Refetching cannot fix it.
var errBadPart = errors.New("yenc part inconsistent with its file")

// maxFileSize bounds file i's decoded size. Encoded article bytes always
// exceed decoded bytes, so the NZB total plus slack is a safe ceiling.
func (d *download) maxFileSize(i int) int64 {
	n := d.nzb.Files[i].Bytes()
	if n <= 0 {
		return absMaxFileSize
	}

	return min(n+n/8+1<<20, absMaxFileSize)
}

// claimSize records file i's size from its first decoded part and checks
// every later part against it.
func (d *download) claimSize(i int, size int64) error {
	if size > d.maxFileSize(i) {
		return fmt.Errorf("%w: declared size %d exceeds %d", errBadPart, size, d.maxFileSize(i))
	}

	d.sizesMu.Lock()
	defer d.sizesMu.Unlock()

	if have, ok := d.sizes[i]; ok && have != size {
		return fmt.Errorf("%w: declared size %d, earlier parts said %d", errBadPart, size, have)
	}

	d.sizes[i] = size

	return nil
}

func (e *Engine) readNZB(root *os.Root, payloadPath string) (*NZB, error) {
	f, err := root.Open(baseName(payloadPath))
	if err != nil {
		return nil, fmt.Errorf("open nzb: %w", err)
	}

	defer func() { _ = f.Close() }()

	return ParseNZB(f)
}

// download is one run of one job.
type download struct {
	e       *Engine
	root    *os.Root // work dir: payload and checkpoint
	data    *os.Root // work dir/data: downloaded files only
	nzb     *NZB
	route   []*pool
	workers int
	ckpt    *checkpoint

	filesMu sync.Mutex
	files   map[int]*os.File

	sizesMu sync.Mutex
	sizes   map[int]int64

	doneBytes atomic.Int64
	missing   atomic.Int64
}

func (d *download) run(ctx context.Context, progress func(engine.Progress)) (*engine.Result, error) {
	var (
		work     []workItem
		total    int64
		result   engine.Result
		included []int
	)

	for i, f := range d.nzb.Files {
		if IsPAR2Volume(f.Name) {
			result.Deferred = append(result.Deferred, f.Name)
			continue
		}

		included = append(included, i)

		for s, seg := range f.Segments {
			total += seg.Bytes

			if d.ckpt.isDone(i, s) {
				d.doneBytes.Add(seg.Bytes)
				continue
			}

			work = append(work, workItem{file: i, seg: s, segment: seg})
		}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	items := make(chan workItem)
	var wg sync.WaitGroup

	workers := min(d.workers, max(len(work), 1))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.worker(ctx, cancel, items)
		}()
	}

	// Progress and checkpoint tickers run until the workers finish.
	stop := make(chan struct{})
	var bg sync.WaitGroup

	bg.Add(1)
	go func() {
		defer bg.Done()
		d.background(stop, total, progress)
	}()

feed:
	for _, it := range work {
		select {
		case items <- it:
		case <-ctx.Done():
			break feed
		}
	}

	close(items)
	wg.Wait()
	close(stop)
	bg.Wait()

	if err := d.flush(); err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "flush: %v", err)
	}

	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return nil, cause
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, i := range included {
		name := d.nzb.Files[i].Name
		size := d.ckpt.size(i)

		if size > 0 {
			if err := d.truncate(i, size); err != nil {
				return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "size %s: %v", name, err)
			}
		}

		result.Files = append(result.Files, engine.File{Path: path.Join(dataDir, name), Size: size})
	}

	result.MissingArticles = int(d.missing.Load())

	if err := d.closeFiles(); err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "close: %v", err)
	}

	return &result, nil
}

// worker processes segments until items is closed or the run is cancelled.
// Connections come from the shared pools, one per article.
func (d *download) worker(ctx context.Context, fatal context.CancelCauseFunc, items <-chan workItem) {
	for it := range items {
		err := d.fetch(ctx, it)

		var f *engine.Failure
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return
		case errors.As(err, &f):
			fatal(f)
			return
		default:
			d.missing.Add(1)
		}
	}
}

// fetch tries the article on each provider in route order. It returns nil
// when written, a *engine.Failure when the download cannot continue, or
// another error when no provider could supply the article.
func (d *download) fetch(ctx context.Context, it workItem) error {
	var (
		lastErr    error
		primaryOK  bool
		primaryErr error
	)

	for _, pl := range d.route {
		err := d.fetchFrom(ctx, pl, it)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		var de *diskError
		if errors.As(err, &de) {
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "write: %v", de.err)
		}

		switch {
		case errors.Is(err, ErrAuth) && !pl.p.Backup:
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, false, "usenet provider %s rejected the credentials", pl.p.Host)
		case errors.Is(err, netguard.ErrBlocked) && !pl.p.Backup:
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, false, "usenet provider %s is on a local network address, which sources from the web app may not use", pl.p.Host)
		case errors.Is(err, ErrAuth), errors.Is(err, netguard.ErrBlocked), errors.Is(err, ErrProviderDown), errors.Is(err, errPoolClosed):
			// Unusable provider: try the next. A down primary only fails the
			// download once every primary is down.
			if !pl.p.Backup {
				primaryErr = err
			}
		default:
			if !pl.p.Backup {
				primaryOK = true
			}

			if errors.Is(err, errDamaged) {
				d.e.log.Debug("damaged article; trying next provider", "message_id", it.segment.MessageID, "provider", pl.p.Host, "err", err)
			}
		}

		lastErr = err
	}

	if !primaryOK && primaryErr != nil && d.allPrimariesDown() {
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, true, "cannot reach any usenet provider: %v", primaryErr)
	}

	if !errors.Is(lastErr, ErrArticleNotFound) {
		d.e.log.Warn("giving up on article", "message_id", it.segment.MessageID, "err", lastErr)
	}

	return lastErr
}

func (d *download) allPrimariesDown() bool {
	for _, pl := range d.route {
		if !pl.p.Backup && !pl.down() {
			return false
		}
	}

	return true
}

// errDamaged marks an article that arrived but failed its checksum or
// contradicts the NZB. Another provider may hold a good copy.
var errDamaged = errors.New("damaged article")

// fetchFrom fetches one article from one provider, retrying only errors
// that leave the connection in an unknown state.
func (d *download) fetchFrom(ctx context.Context, pl *pool, it workItem) error {
	var err error

	for range segmentAttempts {
		var c *conn

		c, err = pl.get(ctx)
		if err != nil {
			return err
		}

		// Closing the socket unblocks a read stuck on a cancelled run.
		stop := context.AfterFunc(ctx, func() { _ = c.nc.Close() })

		var body []byte
		body, err = c.body(it.segment.MessageID)
		if err == nil {
			err = d.write(body, it)
		}

		pl.put(c, stop() && !isConnErr(err))

		var de *diskError
		switch {
		case err == nil, errors.Is(err, ErrArticleNotFound), errors.Is(err, errDamaged), errors.As(err, &de):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		}
	}

	return err
}

// isConnErr reports whether err leaves the connection in an unknown state.
// A missing or damaged article, or a local write error, does not.
func isConnErr(err error) bool {
	var de *diskError

	return err != nil && !errors.Is(err, ErrArticleNotFound) && !errors.Is(err, errDamaged) && !errors.As(err, &de)
}

// diskError marks a local write failure, which no retry can fix.
type diskError struct{ err error }

func (e *diskError) Error() string { return e.err.Error() }

func (e *diskError) Unwrap() error { return e.err }

// write decodes one article and writes it at its offset.
func (d *download) write(body []byte, it workItem) error {
	part, err := DecodeYEnc(bytesReader(body))
	if err != nil {
		return fmt.Errorf("%w: %w", errDamaged, err)
	}

	if err := d.claimSize(it.file, part.FileSize); err != nil {
		return fmt.Errorf("%w: %w", errDamaged, err)
	}

	f, err := d.file(it.file)
	if err != nil {
		return &diskError{err}
	}

	if _, err := f.WriteAt(part.Data, part.Begin-1); err != nil {
		return &diskError{err}
	}

	d.ckpt.markDone(it.file, it.seg, part.FileSize)
	d.doneBytes.Add(it.segment.Bytes)

	return nil
}

func (d *download) file(i int) (*os.File, error) {
	d.filesMu.Lock()
	defer d.filesMu.Unlock()

	if f, ok := d.files[i]; ok {
		return f, nil
	}

	f, err := d.data.OpenFile(d.nzb.Files[i].Name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}

	d.files[i] = f

	return f, nil
}

func (d *download) truncate(i int, size int64) error {
	f, err := d.file(i)
	if err != nil {
		return err
	}

	return f.Truncate(size)
}

// flush snapshots the checkpoint, fsyncs open files, then stores the
// snapshot. Every segment in the snapshot was written before the snapshot
// and is therefore covered by the sync, so the checkpoint never claims data
// that is not on disk.
func (d *download) flush() error {
	raw, err := d.ckpt.snapshot()
	if err != nil {
		return err
	}

	d.filesMu.Lock()
	for _, f := range d.files {
		if err := f.Sync(); err != nil {
			d.filesMu.Unlock()
			return err
		}
	}
	d.filesMu.Unlock()

	return writeCheckpoint(d.root, raw)
}

func (d *download) closeFiles() error {
	d.filesMu.Lock()
	defer d.filesMu.Unlock()

	var first error
	for i, f := range d.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}

		delete(d.files, i)
	}

	return first
}

func (d *download) background(stop <-chan struct{}, total int64, progress func(engine.Progress)) {
	pt := time.NewTicker(progressInterval)
	defer pt.Stop()

	ct := time.NewTicker(checkpointInterval)
	defer ct.Stop()

	last, lastAt := d.doneBytes.Load(), time.Now()

	for {
		select {
		case <-stop:
			return
		case <-ct.C:
			if err := d.flush(); err != nil {
				d.e.log.Warn("save checkpoint", "err", err)
			}
		case now := <-pt.C:
			cur := d.doneBytes.Load()
			speed := int64(float64(cur-last) / now.Sub(lastAt).Seconds())
			last, lastAt = cur, now

			if progress != nil {
				progress(engine.Progress{Bytes: cur, Total: total, SpeedBps: speed})
			}
		}
	}
}
