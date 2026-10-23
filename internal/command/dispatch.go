package command

import (
	"context"
	"fmt"
	"time"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// apply runs inside the command's transaction. It rejects commands whose
// deadline has passed, then dispatches on the command kind to a typed
// handler. A handler returns a result for any outcome the cloud should see,
// including rejections, and an error only for unexpected failures, which
// roll back and are not recorded.
func (a *Applier) apply(ctx context.Context, tx *store.Tx, cmd *pb.Command) (*pb.CommandResult, error) {
	if d := cmd.GetDeadlineMs(); d > 0 && a.clock.Now().After(time.UnixMilli(d)) {
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_DEADLINE_EXCEEDED, "command arrived after its deadline"), nil
	}

	switch k := cmd.GetKind().(type) {
	// Downloads.
	case *pb.Command_StartDownload:
		return a.startDownload(ctx, tx, k.StartDownload)
	case *pb.Command_UpdateDownload:
		return a.updateDownload(ctx, tx, k.UpdateDownload)
	case *pb.Command_PauseDownload:
		return a.pauseDownload(ctx, tx, k.PauseDownload)
	case *pb.Command_ResumeDownload:
		return a.resumeDownload(ctx, tx, k.ResumeDownload)
	case *pb.Command_RetryDownload:
		return a.retryDownload(ctx, tx, k.RetryDownload)
	case *pb.Command_CancelDownload:
		return a.cancelDownload(ctx, tx, k.CancelDownload)

	// Library.
	case *pb.Command_PutLibraryRoot:
		return a.putLibraryRoot(ctx, tx, k.PutLibraryRoot)
	case *pb.Command_RemoveLibraryRoot:
		return a.removeLibraryRoot(ctx, tx, k.RemoveLibraryRoot)
	case *pb.Command_SetImportPolicy:
		return a.setImportPolicy(ctx, tx, k.SetImportPolicy)
	case *pb.Command_RescanLibrary:
		return a.rescanLibrary(ctx, tx, k.RescanLibrary)

	// Sources.
	case *pb.Command_PutSource:
		return a.putSource(ctx, tx, k.PutSource)
	case *pb.Command_RemoveSource:
		return a.removeSource(ctx, tx, k.RemoveSource)

	// Settings.
	case *pb.Command_SetAgentSettings:
		return a.setAgentSettings(ctx, tx, k.SetAgentSettings)

	case nil:
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED, "command has no kind"), nil
	default:
		// A kind added to the proto after this build: reject so the cloud
		// can tell the user to upgrade the agent.
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED, fmt.Sprintf("unknown command kind %T", k)), nil
	}
}
