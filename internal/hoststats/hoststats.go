// Package hoststats measures the free space where the agent writes, the
// downloads folder and each library root (design 0008 §3). Every minute it
// raises or resolves the disk.low notice and, while the cloud has turned
// host stats on with SetAgentSettings, sends the numbers as lossy
// HostStats telemetry.
package hoststats

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/getnabbed/agent/internal/notice"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	// DefaultInterval is how often the folders are checked.
	DefaultInterval = time.Minute
	// TelemetryKey is the telemetry key HostStats is sent under.
	TelemetryKey = "host_stats"

	// disk.low starts under lowPercent or lowBytes free, and clears only
	// above clearPercent and clearBytes, so a disk hovering near the line
	// doesn't flap.
	lowPercent   = 5
	lowBytes     = 10_000_000_000
	clearPercent = 8
	clearBytes   = 15_000_000_000

	// hostStatsKey is the meta key holding the host_stats setting.
	hostStatsKey = "host_stats"
)

// Usage is the space on one file system.
type Usage struct {
	Free  uint64
	Total uint64
}

// low reports whether u is low enough to raise disk.low.
func (u Usage) low() bool { return u.Free*100 < u.Total*lowPercent || u.Free < lowBytes }

// clear reports whether u is high enough to resolve disk.low.
func (u Usage) clear() bool { return u.Free*100 > u.Total*clearPercent && u.Free > clearBytes }

// Roots lists library roots. *library.Manager implements it.
type Roots interface {
	Roots(ctx context.Context) ([]*pb.LibraryRoot, error)
}

// Notifier reports agent notices. *notice.Reporter implements it.
type Notifier interface {
	Raise(ctx context.Context, code, subject string, sev pb.Severity, message string, attrs map[string]string) error
	Resolve(ctx context.Context, code, subject string) error
	Reconcile(ctx context.Context, code string, keep []string) error
}

// Telemetry sends lossy telemetry. *link.Client implements it.
type Telemetry interface {
	SendTelemetry(key string, t *pb.Telemetry)
}

// Config configures a Monitor.
type Config struct {
	Store *store.Store
	// DownloadsDir is the downloads folder. Its disk.low notice uses the
	// path as subject.
	DownloadsDir string
	// Roots is optional; without it only the downloads folder is checked.
	Roots Roots
	// Notifier is optional; without it no disk.low notices are raised.
	Notifier Notifier
	// Usage measures a folder; nil uses DiskUsage.
	Usage func(path string) (Usage, error)
	// Interval between checks; zero uses DefaultInterval.
	Interval time.Duration
	Log      *slog.Logger
}

// Monitor checks free space. It is safe for concurrent use.
type Monitor struct {
	cfg Config
	log *slog.Logger

	hostStats atomic.Bool
	telemetry atomic.Pointer[Telemetry]
	kick      chan struct{}
}

// New returns a Monitor with the host_stats setting read from the store.
func New(ctx context.Context, cfg Config) (*Monitor, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	if cfg.Usage == nil {
		cfg.Usage = DiskUsage
	}

	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}

	m := &Monitor{cfg: cfg, log: cfg.Log.With("component", "hoststats"), kick: make(chan struct{}, 1)}

	v, err := cfg.Store.Meta(ctx, hostStatsKey)
	if err != nil {
		return nil, fmt.Errorf("read host stats setting: %w", err)
	}

	m.hostStats.Store(v == "1")

	return m, nil
}

// SetTelemetry sets where HostStats goes. Until it is set, none is sent.
func (m *Monitor) SetTelemetry(t Telemetry) { m.telemetry.Store(&t) }

// HostStats reports whether HostStats telemetry is on.
func (m *Monitor) HostStats() bool { return m.hostStats.Load() }

// SetHostStats stores the host_stats setting as part of tx. It takes
// effect once tx commits, with a check right away when turned on.
func (m *Monitor) SetHostStats(ctx context.Context, tx *store.Tx, on bool) error {
	v := "0"
	if on {
		v = "1"
	}

	if err := tx.SetMeta(ctx, hostStatsKey, v); err != nil {
		return fmt.Errorf("store host stats setting: %w", err)
	}

	tx.AfterCommit(func() {
		if m.hostStats.Swap(on) != on && on {
			select {
			case m.kick <- struct{}{}:
			default:
			}
		}
	})

	return nil
}

// Run checks now and then every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context) error {
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()

	for {
		m.tick(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-m.kick:
		}
	}
}

func (m *Monitor) tick(ctx context.Context) {
	hs := m.Check(ctx)
	if ctx.Err() != nil || !m.hostStats.Load() {
		return
	}

	if t := m.telemetry.Load(); t != nil {
		(*t).SendTelemetry(TelemetryKey, &pb.Telemetry{Kind: &pb.Telemetry_HostStats{HostStats: hs}})
	}
}

// folder is one place the agent writes.
type folder struct {
	subject string // root id, or the downloads folder's path
	rootID  string
	path    string
}

// Check measures each folder, raises or resolves disk.low, and returns the
// numbers. A folder that can't be measured is left out and keeps its
// notice state. CPU and memory stay unset: they are only for a host owner
// who opts in locally, which this agent doesn't offer yet.
func (m *Monitor) Check(ctx context.Context) *pb.HostStats {
	folders := []folder{{subject: m.cfg.DownloadsDir, path: m.cfg.DownloadsDir}}

	if m.cfg.Roots != nil {
		roots, err := m.cfg.Roots.Roots(ctx)
		if err != nil {
			m.log.Warn("list library roots", "err", err)
		}

		for _, r := range roots {
			folders = append(folders, folder{subject: r.GetId(), rootID: r.GetId(), path: r.GetPath()})
		}
	}

	hs := &pb.HostStats{}
	keep := make([]string, 0, len(folders))

	for _, f := range folders {
		keep = append(keep, f.subject)

		u, err := m.cfg.Usage(f.path)
		if err != nil {
			m.log.Debug("measure free space", "path", f.path, "err", err)
			continue
		}

		hs.DiskFree = append(hs.DiskFree, &pb.RootDiskFree{
			RootId: f.rootID, FreeBytes: u.Free, TotalBytes: u.Total, Downloads: f.rootID == "",
		})

		m.notice(ctx, f, u)
	}

	if m.cfg.Notifier != nil && ctx.Err() == nil {
		// Removed roots, or a moved downloads folder, are no longer low.
		if err := m.cfg.Notifier.Reconcile(ctx, notice.CodeDiskLow, keep); err != nil {
			m.log.Warn("report notice", "code", notice.CodeDiskLow, "err", err)
		}
	}

	return hs
}

// notice raises disk.low when f is low and resolves it when f is clear.
// In between, the state stays as it was.
func (m *Monitor) notice(ctx context.Context, f folder, u Usage) {
	n := m.cfg.Notifier
	if n == nil {
		return
	}

	var err error

	switch {
	case u.low():
		where := "the downloads folder"
		attrs := map[string]string{"free_bytes": strconv.FormatUint(u.Free, 10), "total_bytes": strconv.FormatUint(u.Total, 10)}

		if f.rootID != "" {
			where = "library root " + f.rootID
			attrs["root_id"] = f.rootID
		} else {
			attrs["downloads"] = "true"
		}

		err = n.Raise(ctx, notice.CodeDiskLow, f.subject, pb.Severity_SEVERITY_WARNING,
			fmt.Sprintf("free space is low in %s: %s free of %s", where, bytesString(u.Free), bytesString(u.Total)), attrs)
	case u.clear():
		err = n.Resolve(ctx, notice.CodeDiskLow, f.subject)
	}

	if err != nil && ctx.Err() == nil {
		m.log.Warn("report notice", "code", notice.CodeDiskLow, "subject_id", f.subject, "err", err)
	}
}

// bytesString formats n in decimal units, such as "9.5 GB".
func bytesString(n uint64) string {
	const unit = 1000

	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}

	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
