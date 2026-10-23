package command

import (
	"context"
	"errors"

	"github.com/getnabbed/agent/internal/download"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Download commands drive the per-download state machine (design 0001 §7).
// The state change and its DownloadUpdate event commit with the command's
// result; the scheduler picks up the change after commit.

// startDownload queues a cloud-selected release for download: QUEUED, then
// FETCHING once a runner is free.
func (a *Applier) startDownload(ctx context.Context, tx *store.Tx, c *pb.StartDownload) (*pb.CommandResult, error) {
	if a.downloads == nil {
		return notImplemented("start_download"), nil
	}

	return downloadResult(a.downloads.Start(ctx, tx, c))
}

// updateDownload changes the priority or source of a download that has not
// finished.
func (a *Applier) updateDownload(ctx context.Context, tx *store.Tx, c *pb.UpdateDownload) (*pb.CommandResult, error) {
	if a.downloads == nil {
		return notImplemented("update_download"), nil
	}

	return downloadResult(a.downloads.Update(ctx, tx, c))
}

// pauseDownload pauses a QUEUED, FETCHING or DOWNLOADING download.
func (a *Applier) pauseDownload(ctx context.Context, tx *store.Tx, c *pb.PauseDownload) (*pb.CommandResult, error) {
	return a.act(ctx, tx, "pause_download", download.Pause, c.GetDownloadId())
}

// resumeDownload resumes a PAUSED download from its partial data.
func (a *Applier) resumeDownload(ctx context.Context, tx *store.Tx, c *pb.ResumeDownload) (*pb.CommandResult, error) {
	return a.act(ctx, tx, "resume_download", download.Resume, c.GetDownloadId())
}

// retryDownload re-runs a FAILED download with the same release; choosing
// another release is the cloud's job.
func (a *Applier) retryDownload(ctx context.Context, tx *store.Tx, c *pb.RetryDownload) (*pb.CommandResult, error) {
	return a.act(ctx, tx, "retry_download", download.Retry, c.GetDownloadId())
}

// cancelDownload cancels a download that is not DONE; its work directory
// is removed.
func (a *Applier) cancelDownload(ctx context.Context, tx *store.Tx, c *pb.CancelDownload) (*pb.CommandResult, error) {
	return a.act(ctx, tx, "cancel_download", download.Cancel, c.GetDownloadId())
}

func (a *Applier) act(ctx context.Context, tx *store.Tx, kind string, action download.Action, id string) (*pb.CommandResult, error) {
	if a.downloads == nil {
		return notImplemented(kind), nil
	}

	return downloadResult(a.downloads.Act(ctx, tx, action, id))
}

// downloadResult maps download errors to command outcomes. Unclassified
// errors are returned so the transaction rolls back and nothing is
// recorded.
func downloadResult(err error) (*pb.CommandResult, error) {
	switch {
	case err == nil:
		return applied(""), nil
	case errors.Is(err, download.ErrAlreadyExists):
		return applied("download already exists"), nil
	case errors.Is(err, download.ErrInvalidArgument):
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_ARGUMENT, err.Error()), nil
	case errors.Is(err, download.ErrUnknownDownload):
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_UNKNOWN_ID, err.Error()), nil
	case errors.Is(err, download.ErrInvalidTransition):
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_TRANSITION, err.Error()), nil
	case errors.Is(err, download.ErrProtocolUnavailable):
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_PROTOCOL_UNAVAILABLE, err.Error()), nil
	default:
		return nil, err
	}
}
