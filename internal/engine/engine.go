// Package engine defines the interface between the download state machine
// and the native download engines (usenet, BitTorrent). An engine moves
// bytes into a job's work directory; it never chooses releases, renames
// into a library or touches anything outside that directory.
package engine

import (
	"context"
	"errors"
	"fmt"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Job is one download handed to an engine.
type Job struct {
	// ID is the cloud's download id.
	ID string
	// Dir is the job's private work directory. Engines must create files
	// only inside it, through os.Root, never by joining untrusted names.
	Dir string
	// PayloadPath is the verified NZB or .torrent inside Dir, or "" for a
	// magnet link.
	PayloadPath string
	// Magnet is set instead of PayloadPath for magnet releases.
	Magnet string
	// SourceID selects the provider for engines that need one.
	SourceID string
	// Title is the release title from the control plane. It is untrusted
	// and only used, sanitised, to name files whose own names are
	// obfuscated.
	Title string
	// Notes collects what the stage did worth showing the user. It may
	// be nil.
	Notes *Notes
}

// Progress is reported by engines while downloading, at most about once a
// second.
type Progress struct {
	Bytes    int64
	Total    int64
	SpeedBps int64
}

// File is one file an engine produced, relative to Job.Dir.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Result is what an engine produced. It is persisted so the assemble stage
// can resume after a restart.
type Result struct {
	Files []File `json:"files"`
	// MissingArticles counts usenet articles that could not be fetched.
	// Assembly decides whether PAR2 can cover them.
	MissingArticles int `json:"missing_articles,omitempty"`
	// Deferred lists files deliberately not downloaded, such as PAR2
	// recovery volumes fetched only if repair needs them.
	Deferred []string `json:"deferred,omitempty"`
	// Stats summarise the transfer, for DownloadUpdate.stats.
	Stats *Stats `json:"stats,omitempty"`
}

// Stats summarise a finished transfer.
type Stats struct {
	// Bytes is the payload downloaded, including any from earlier runs.
	Bytes int64 `json:"bytes"`
	// Duration is the time the final run took, in milliseconds.
	DurationMs int64 `json:"duration_ms"`
	// RunBytes is what the final run transferred, for the average speed.
	RunBytes int64 `json:"run_bytes"`
	// SourceID is the usenet source that served most articles.
	SourceID string `json:"source_id,omitempty"`
	// Peers counts distinct torrent peers seen.
	Peers int `json:"peers,omitempty"`
}

// Proto converts s for the wire, with the average speed of the final run
// and missing, the usenet articles that couldn't be fetched.
func (s *Stats) Proto(missing int) *pb.DownloadStats {
	if s == nil {
		return nil
	}

	ps := &pb.DownloadStats{
		Bytes: uint64(max(s.Bytes, 0)), DurationMs: uint64(max(s.DurationMs, 0)), SourceId: s.SourceID,
		MissingArticles: uint32(max(missing, 0)), Peers: uint32(max(s.Peers, 0)),
	}

	if s.DurationMs > 0 {
		ps.AvgBps = uint64(max(s.RunBytes, 0) * 1000 / s.DurationMs)
	}

	return ps
}

// Engine downloads one protocol.
type Engine interface {
	Protocol() pb.Protocol
	// Available reports whether the engine can take jobs, for example
	// whether a usenet provider is configured. It drives Hello capabilities.
	Available() bool
	// Download fetches the job's data into job.Dir, resuming any partial
	// data already there. It returns ctx.Err() when cancelled, a *Failure
	// for classified failures, or any other error for unexpected ones.
	Download(ctx context.Context, job Job, progress func(Progress)) (*Result, error)
}

// Failure is a classified download failure reported to the cloud as
// DownloadUpdate{FAILED, failure}.
type Failure struct {
	Class     pb.FailureClass
	Retryable bool
	Detail    string
}

func (f *Failure) Error() string {
	return fmt.Sprintf("%s: %s", f.Class, f.Detail)
}

// Fail returns a *Failure.
func Fail(class pb.FailureClass, retryable bool, format string, args ...any) *Failure {
	return &Failure{Class: class, Retryable: retryable, Detail: fmt.Sprintf(format, args...)}
}

// AsFailure classifies err, mapping unclassified errors to class with
// retryable set, since an unexpected error may well be transient.
func AsFailure(err error, class pb.FailureClass) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}

	return &Failure{Class: class, Retryable: true, Detail: err.Error()}
}

// Proto converts f for the wire.
func (f *Failure) Proto() *pb.Failure {
	return &pb.Failure{Class: f.Class, Retryable: f.Retryable, Detail: f.Detail}
}
