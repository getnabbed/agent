// Package torrent is the native BitTorrent engine, built on
// github.com/anacrolix/torrent (pure Go when built without cgo: Go uTP and a
// BoltDB piece-completion store).
//
// One client is shared by all jobs. Each job gets its own file storage and
// piece-completion database inside its work directory, so pausing, resuming
// and deleting a download only ever touch that directory.
package torrent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/netguard"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// dataDir is where torrent data lives inside a job's work directory.
const dataDir = "data"

// Config configures the engine.
type Config struct {
	// ListenPort for incoming peer connections. 0 picks a free port.
	ListenPort int
	// PortForwarding enables UPnP/NAT-PMP mapping on the user's router.
	// Off by default: changing router config is the host owner's call.
	PortForwarding bool
	// AllowLAN lets peers, trackers, web seeds and DHT nodes on the local
	// network be contacted. Off by default: torrent metadata is untrusted
	// and must not be able to make the agent reach LAN devices, the router
	// or cloud metadata endpoints.
	AllowLAN bool
	// MetadataTimeout bounds waiting for a magnet's metadata.
	MetadataTimeout time.Duration
	// StallTimeout fails a download that makes no progress for this long.
	StallTimeout time.Duration
	Log          *slog.Logger

	// Test hooks.
	noDHT     bool
	peerAddrs []string
}

// Engine is the native BitTorrent engine.
type Engine struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	client *torrent.Client
	closed bool
}

// New returns an Engine. The client starts on first use.
func New(cfg Config) *Engine {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	if cfg.MetadataTimeout <= 0 {
		cfg.MetadataTimeout = 30 * time.Minute
	}

	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 2 * time.Hour
	}

	return &Engine{cfg: cfg, log: cfg.Log.With("engine", "torrent")}
}

// Protocol implements engine.Engine.
func (e *Engine) Protocol() pb.Protocol { return pb.Protocol_PROTOCOL_TORRENT }

// Available implements engine.Engine. BitTorrent needs no provider.
func (e *Engine) Available() bool { return true }

// Close stops the shared client.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.closed = true
	if e.client == nil {
		return nil
	}

	errs := e.client.Close()
	e.client = nil

	return errors.Join(errs...)
}

func (e *Engine) clientLocked() (*torrent.Client, error) {
	if e.closed {
		return nil, errors.New("torrent engine closed")
	}

	if e.client != nil {
		return e.client, nil
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = e.cfg.ListenPort
	cfg.Seed = false
	cfg.NoDefaultPortForwarding = !e.cfg.PortForwarding
	cfg.NoDHT = e.cfg.noDHT
	cfg.Slogger = e.log
	// WebTorrent (WebRTC) is not needed for BitTorrent and adds a large attack
	// surface; keep it off.
	cfg.DisableWebtorrent = true

	policy := netguard.Policy{AllowLAN: e.cfg.AllowLAN}
	if !policy.AllowLAN {
		// Peers in and out, and DHT nodes.
		cfg.IPBlocklist = lanBlocklist{}
	}

	// Trackers and web seeds are checked at connect time, after DNS.
	cfg.TrackerDialContext = policy.Dialer(nil)
	cfg.TrackerListenPacket = policy.ListenPacket()
	cfg.HTTPDialContext = policy.Dialer(nil)

	// Every job brings its own storage; this default only guards against a
	// torrent ever being added without one, and writes nowhere persistent.
	cfg.DefaultStorage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   os.TempDir(),
		PieceCompletion: storage.NewMapPieceCompletion(),
	})

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	e.client = cl

	return cl, nil
}

// Download implements engine.Engine.
func (e *Engine) Download(ctx context.Context, job engine.Job, progress func(engine.Progress)) (*engine.Result, error) {
	spec, err := e.spec(job)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "%v", err)
	}

	pc, err := storage.NewBoltPieceCompletion(job.Dir)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "piece completion db: %v", err)
	}

	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   filepath.Join(job.Dir, dataDir),
		PieceCompletion: pc,
		FilePathMaker:   func(o storage.FilePathMakerOpts) string { return relPath(o.Info, o.File) },
		Logger:          e.log,
	})
	defer func() { _ = st.Close() }()

	spec.Storage = st
	spec.PeerAddrs = append(spec.PeerAddrs, e.cfg.peerAddrs...)

	t, err := e.add(spec)
	if err != nil {
		return nil, err
	}

	defer t.Drop()

	if err := e.awaitInfo(ctx, t); err != nil {
		return nil, err
	}

	files, err := safeFiles(t.Info())
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "%v", err)
	}

	t.DownloadAll()

	if err := e.await(ctx, t, progress); err != nil {
		return nil, err
	}

	return &engine.Result{Files: files}, nil
}

func (e *Engine) spec(job engine.Job) (*torrent.TorrentSpec, error) {
	var (
		spec *torrent.TorrentSpec
		err  error
	)

	if job.Magnet != "" {
		spec, err = torrent.TorrentSpecFromMagnetUri(job.Magnet)
	} else {
		var mi *metainfo.MetaInfo

		mi, err = metainfo.LoadFromFile(job.PayloadPath)
		if err != nil {
			return nil, fmt.Errorf("read .torrent: %w", err)
		}

		spec, err = torrent.TorrentSpecFromMetaInfoErr(mi)
	}

	if err != nil {
		return nil, err
	}

	// anacrolix panics on a zero v1 info hash; v2-only torrents have none.
	if spec.InfoHash == (metainfo.Hash{}) {
		return nil, errors.New("torrent has no BitTorrent v1 info hash")
	}

	return spec, nil
}

// add registers spec with the shared client. A torrent already running for
// another job is refused: anacrolix would return the existing torrent, ignore
// this job's storage, and let either job stop the other.
func (e *Engine) add(spec *torrent.TorrentSpec) (*torrent.Torrent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cl, err := e.clientLocked()
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, true, "start torrent client: %v", err)
	}

	if _, exists := cl.Torrent(spec.InfoHash); exists {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, true, "the same torrent is already downloading for another download")
	}

	t, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_PAYLOAD, false, "add torrent: %v", err)
	}

	return t, nil
}

func (e *Engine) awaitInfo(ctx context.Context, t *torrent.Torrent) error {
	timer := time.NewTimer(e.cfg.MetadataTimeout)
	defer timer.Stop()

	select {
	case <-t.GotInfo():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, true, "no metadata after %s (%d peers)", e.cfg.MetadataTimeout, t.Stats().ActivePeers)
	}
}

// await reports progress until every byte is verified, failing the job if
// nothing arrives for StallTimeout.
func (e *Engine) await(ctx context.Context, t *torrent.Torrent, progress func(engine.Progress)) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	total := t.Length()
	last, lastAt, lastGain := t.BytesCompleted(), time.Now(), time.Now()

	for {
		if t.BytesMissing() == 0 {
			if progress != nil {
				progress(engine.Progress{Bytes: total, Total: total})
			}

			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			done := t.BytesCompleted()
			speed := int64(float64(done-last) / now.Sub(lastAt).Seconds())

			if done > last {
				lastGain = now
			}

			last, lastAt = done, now

			if progress != nil {
				progress(engine.Progress{Bytes: done, Total: total, SpeedBps: max(speed, 0)})
			}

			if now.Sub(lastGain) > e.cfg.StallTimeout {
				return engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, true, "no progress for %s (%d peers, %.1f%% done)",
					e.cfg.StallTimeout, t.Stats().ActivePeers, 100*float64(done)/float64(max(total, 1)))
			}
		}
	}
}

// relPath is the on-disk path of a file relative to the data directory. It
// is used both by storage and for the result, so they cannot disagree.
func relPath(info *metainfo.Info, fi *metainfo.FileInfo) string {
	var parts []string
	if name := info.BestName(); name != metainfo.NoName {
		parts = append(parts, name)
	}

	return filepath.Join(append(parts, fi.BestPath()...)...)
}

// safeFiles lists the torrent's files, refusing any whose path could
// escape the data directory. Torrent metadata is untrusted input.
func safeFiles(info *metainfo.Info) ([]engine.File, error) {
	var out []engine.File

	for _, fi := range info.UpvertedFiles() {
		p := relPath(info, &fi)
		if !filepath.IsLocal(p) {
			return nil, fmt.Errorf("torrent contains unsafe path %q", p)
		}

		out = append(out, engine.File{Path: filepath.Join(dataDir, p), Size: fi.Length})
	}

	return out, nil
}

// lanBlocklist makes anacrolix refuse local-network peers and DHT nodes.
type lanBlocklist struct{}

func (lanBlocklist) Lookup(ip net.IP) (iplist.Range, bool) {
	a, ok := netip.AddrFromSlice(ip)
	if !ok || !netguard.IsLocal(a) {
		return iplist.Range{}, false
	}

	return iplist.Range{First: ip, Last: ip, Description: "local network (torrent.allow_lan is off)"}, true
}

func (lanBlocklist) NumRanges() int { return len(netguard.Ranges()) }
