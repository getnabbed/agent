package command

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/getnabbed/agent/internal/download"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

type fakeDownloads struct {
	err    error
	kicks  int
	action download.Action
}

func (f *fakeDownloads) Start(context.Context, *store.Tx, *pb.StartDownload) error { return f.err }

func (f *fakeDownloads) Act(_ context.Context, _ *store.Tx, a download.Action, _ string) error {
	f.action = a

	return f.err
}

func (f *fakeDownloads) Update(context.Context, *store.Tx, *pb.UpdateDownload) error { return f.err }
func (f *fakeDownloads) Kick()                                                       { f.kicks++ }

func TestDownloadErrorsMapToCodes(t *testing.T) {
	cases := []struct {
		err     error
		outcome pb.CommandOutcome
		code    pb.CommandErrorCode
	}{
		{nil, pb.CommandOutcome_COMMAND_OUTCOME_APPLIED, 0},
		{download.ErrAlreadyExists, pb.CommandOutcome_COMMAND_OUTCOME_APPLIED, 0},
		{fmt.Errorf("%w: x", download.ErrInvalidArgument), pb.CommandOutcome_COMMAND_OUTCOME_REJECTED, pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_ARGUMENT},
		{fmt.Errorf("%w: x", download.ErrUnknownDownload), pb.CommandOutcome_COMMAND_OUTCOME_REJECTED, pb.CommandErrorCode_COMMAND_ERROR_CODE_UNKNOWN_ID},
		{fmt.Errorf("%w: x", download.ErrInvalidTransition), pb.CommandOutcome_COMMAND_OUTCOME_REJECTED, pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_TRANSITION},
		{fmt.Errorf("%w: x", download.ErrProtocolUnavailable), pb.CommandOutcome_COMMAND_OUTCOME_REJECTED, pb.CommandErrorCode_COMMAND_ERROR_CODE_PROTOCOL_UNAVAILABLE},
		{errors.New("disk on fire"), pb.CommandOutcome_COMMAND_OUTCOME_FAILED, pb.CommandErrorCode_COMMAND_ERROR_CODE_INTERNAL},
	}

	for i, tc := range cases {
		fd := &fakeDownloads{err: tc.err}
		a := newTestApplier(t, nil)
		a.downloads = fd

		cmd := &pb.Command{Id: fmt.Sprintf("c%d", i), Kind: &pb.Command_CancelDownload{CancelDownload: &pb.CancelDownload{DownloadId: "d"}}}

		res := a.HandleCommand(context.Background(), cmd)
		if res.GetOutcome() != tc.outcome || res.GetErrorCode() != tc.code {
			t.Errorf("err %v: result %v, want %v/%v", tc.err, res, tc.outcome, tc.code)
		}

		if fd.action != download.Cancel {
			t.Errorf("err %v: action %v, want cancel", tc.err, fd.action)
		}

		wantKicks := 1
		if tc.outcome == pb.CommandOutcome_COMMAND_OUTCOME_FAILED {
			wantKicks = 0 // rolled back, nothing to schedule
		}

		if fd.kicks != wantKicks {
			t.Errorf("err %v: kicks = %d, want %d", tc.err, fd.kicks, wantKicks)
		}
	}
}
