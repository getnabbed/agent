package usenet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	segmentAttempts    = 4
	maxDialFailures    = 5
	checkpointInterval = 5 * time.Second
	progressInterval   = time.Second
)

// ResolveFunc returns the provider for a download's source id.
type ResolveFunc func(ctx context.Context, sourceID string) (Provider, error)

// ErrNoProvider means no usenet provider is configured for the source.
var ErrNoProvider = errors.New("no usenet provider configured")

// Engine is the native usenet engine.
type Engine struct {
	resolve   ResolveFunc
	available func() bool
	timeout   time.Duration
	log       *slog.Logger
}

// New returns an Engine. available reports whether any provider exists,
// for Hello capabilities.
func New(resolve ResolveFunc, available func() bool, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}

	return &Engine{resolve: resolve, available: available, timeout: 60 * time.Second, log: log.With("engine", "usenet")}
}

// Protocol implements engine.Engine.
func (e *Engine) Protocol() pb.Protocol { return pb.Protocol_PROTOCOL_USENET }

// Available implements engine.Engine.
func (e *Engine) Available() bool { return e.available != nil && e.available() }

type workItem struct {
	file, seg int
	segment   Segment
}

// Download implements engine.Engine.
func (e *Engine) Download(ctx context.Context, job engine.Job, progress func(engine.Progress)) (*engine.Result, error) {
	root, err := os.OpenRoot(job.Dir)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "open work dir: %v", err)
	}

	defer func() { _ = root.Close() }()

	nzb, err := e.readNZB(root, job.PayloadPath)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "%v", err)
	}

	provider, err := e.resolve(ctx, job.SourceID)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, false, "%v", err)
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
		e: e, root: root, data: data, nzb: nzb, provider: provider, ckpt: ckpt,
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
	e        *Engine
	root     *os.Root // work dir: payload and checkpoint
	data     *os.Root // work dir/data: downloaded files only
	nzb      *NZB
	provider Provider
	ckpt     *checkpoint

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

	workers := min(max(d.provider.Connections, 1), max(len(work), 1))
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

// worker owns one NNTP connection and processes segments until items is
// closed or the run is cancelled.
func (d *download) worker(ctx context.Context, fatal context.CancelCauseFunc, items <-chan workItem) {
	var (
		c         *conn
		stopClose func() bool
		dialFails int
	)

	disconnect := func() {
		if c != nil {
			stopClose()
			_ = c.close()
			c = nil
		}
	}

	defer disconnect()

	for it := range items {
		for attempt := 0; attempt < segmentAttempts; attempt++ {
			if ctx.Err() != nil {
				return
			}

			if c == nil {
				var err error

				c, err = dial(ctx, d.provider, d.e.timeout)
				if errors.Is(err, ErrAuth) {
					fatal(engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, false, "usenet provider rejected the credentials"))
					return
				}

				if err != nil {
					dialFails++
					if dialFails >= maxDialFailures {
						fatal(engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, true, "cannot connect to usenet provider: %v", err))
						return
					}

					sleep(ctx, time.Duration(dialFails)*time.Second)

					continue
				}

				dialFails = 0
				// Closing the socket unblocks a read stuck on a cancelled run.
				conn := c
				stopClose = context.AfterFunc(ctx, func() { _ = conn.nc.Close() })
			}

			done, err := d.fetch(c, it)
			if done {
				break
			}

			if errors.Is(err, ErrArticleNotFound) || errors.Is(err, errBadPart) {
				if errors.Is(err, errBadPart) {
					d.e.log.Warn("discarding article", "message_id", it.segment.MessageID, "err", err)
				}

				d.missing.Add(1)
				break
			}

			var de *diskError
			if errors.As(err, &de) {
				fatal(engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "write: %v", de.err))
				return
			}

			var ne net.Error
			if errors.As(err, &ne) || errors.Is(err, ErrAuth) || isConnErr(err) {
				disconnect()
			}

			if attempt == segmentAttempts-1 {
				d.e.log.Warn("giving up on article", "message_id", it.segment.MessageID, "err", err)
				d.missing.Add(1)
			}
		}
	}
}

// isConnErr reports whether err leaves the connection in an unknown state.
// A bad checksum or a missing article does not.
func isConnErr(err error) bool {
	return err != nil && !errors.Is(err, ErrYEncCRC) && !errors.Is(err, ErrArticleNotFound) && !errors.Is(err, errBadPart)
}

// diskError marks a local write failure, which no retry can fix.
type diskError struct{ err error }

func (e *diskError) Error() string { return e.err.Error() }

func (e *diskError) Unwrap() error { return e.err }

// fetch downloads, decodes and writes one segment.
func (d *download) fetch(c *conn, it workItem) (bool, error) {
	body, err := c.body(it.segment.MessageID)
	if err != nil {
		return false, err
	}

	part, err := DecodeYEnc(bytesReader(body))
	if err != nil {
		return false, err
	}

	if err := d.claimSize(it.file, part.FileSize); err != nil {
		return false, err
	}

	f, err := d.file(it.file)
	if err != nil {
		return false, &diskError{err}
	}

	if _, err := f.WriteAt(part.Data, part.Begin-1); err != nil {
		return false, &diskError{err}
	}

	d.ckpt.markDone(it.file, it.seg, part.FileSize)
	d.doneBytes.Add(it.segment.Bytes)

	return true, nil
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

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
