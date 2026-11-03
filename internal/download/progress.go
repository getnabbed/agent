package download

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// ProgressTelemetryKey is the telemetry key DownloadProgressBatch is sent
// under. Every batch carries every transferring download, so a newer batch
// can replace one not yet sent.
const ProgressTelemetryKey = "progress"

// progressBatch holds the latest progress of each download whose engine is
// running, and of each whose run just ended until that has gone out once.
// The zero value is ready to use.
type progressBatch struct {
	mu    sync.Mutex
	items map[string]progressItem
}

type progressItem struct {
	p *pb.DownloadProgress
	// ended means the engine run is over, so the item goes out once more
	// and is then dropped.
	ended bool
}

// set records p as the latest progress of its download. p must not be
// changed afterwards: batches waiting to be sent share it.
func (b *progressBatch) set(p *pb.DownloadProgress) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.items == nil {
		b.items = map[string]progressItem{}
	}

	b.items[p.GetDownloadId()] = progressItem{p: p}
}

// end marks the engine run of download id as over.
func (b *progressBatch) end(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if it, ok := b.items[id]; ok {
		it.ended = true
		b.items[id] = it
	}
}

// take returns the latest progress of every download, ordered by id, and
// drops the downloads whose run has ended.
func (b *progressBatch) take() []*pb.DownloadProgress {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) == 0 {
		return nil
	}

	out := make([]*pb.DownloadProgress, 0, len(b.items))
	for _, id := range slices.Sorted(maps.Keys(b.items)) {
		it := b.items[id]
		out = append(out, it.p)

		if it.ended {
			delete(b.items, id)
		}
	}

	return out
}

// progress records p as the latest progress of download id, for the next
// batch.
func (m *Manager) progress(id string, p engine.Progress) {
	dp := &pb.DownloadProgress{DownloadId: id, Bytes: uint64(max(p.Bytes, 0)), SpeedBps: uint64(max(p.SpeedBps, 0))}
	if p.Total > 0 {
		dp.Percent = float32(100 * float64(p.Bytes) / float64(p.Total))
	}

	if p.SpeedBps > 0 && p.Total > p.Bytes {
		dp.EtaS = uint64((p.Total - p.Bytes) / p.SpeedBps)
	}

	m.batch.set(dp)
}

// sendProgress sends a progress batch every m.progressEvery until ctx ends.
func (m *Manager) sendProgress(ctx context.Context) {
	t := time.NewTicker(m.progressEvery)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.flushProgress()
		}
	}
}

// flushProgress sends one DownloadProgressBatch with the latest progress of
// every transferring download, and of those whose run just ended. It sends
// nothing when there are none.
func (m *Manager) flushProgress() {
	downloads := m.batch.take()
	if len(downloads) == 0 {
		return
	}

	t := m.telemetry.Load()
	if t == nil {
		return
	}

	batch := &pb.DownloadProgressBatch{Downloads: downloads}
	(*t).SendTelemetry(ProgressTelemetryKey, &pb.Telemetry{Kind: &pb.Telemetry_DownloadProgressBatch{DownloadProgressBatch: batch}})
}
