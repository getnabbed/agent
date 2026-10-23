package command

import (
	"context"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Download commands drive the per-download state machine (design 0001 §7).

// startDownload queues a cloud-selected release for download: QUEUED,
// then FETCHING once the payload is pulled.
func (a *Applier) startDownload(_ context.Context, _ *store.Tx, _ *pb.StartDownload) (*pb.CommandResult, error) {
	return notImplemented("start_download"), nil
}

// updateDownload changes the priority or source of a download that has
// not finished.
func (a *Applier) updateDownload(_ context.Context, _ *store.Tx, _ *pb.UpdateDownload) (*pb.CommandResult, error) {
	return notImplemented("update_download"), nil
}

// pauseDownload pauses a QUEUED, FETCHING or DOWNLOADING download.
func (a *Applier) pauseDownload(_ context.Context, _ *store.Tx, _ *pb.PauseDownload) (*pb.CommandResult, error) {
	return notImplemented("pause_download"), nil
}

// resumeDownload resumes a PAUSED download from where it stopped.
func (a *Applier) resumeDownload(_ context.Context, _ *store.Tx, _ *pb.ResumeDownload) (*pb.CommandResult, error) {
	return notImplemented("resume_download"), nil
}

// retryDownload re-adds a FAILED download's same release to its client;
// choosing another release is the cloud's job.
func (a *Applier) retryDownload(_ context.Context, _ *store.Tx, _ *pb.RetryDownload) (*pb.CommandResult, error) {
	return notImplemented("retry_download"), nil
}

// cancelDownload cancels a download from any non-terminal state and
// removes its partial data under download_dirs.
func (a *Applier) cancelDownload(_ context.Context, _ *store.Tx, _ *pb.CancelDownload) (*pb.CommandResult, error) {
	return notImplemented("cancel_download"), nil
}
