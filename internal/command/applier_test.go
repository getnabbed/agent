package command

import (
	"context"
	"log/slog"
	"testing"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

func TestUnsupportedCommandIsRecordedAndReplayed(t *testing.T) {
	ctx := context.Background()

	s, err := store.Open(ctx, t.TempDir(), "agent-1", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = s.Close() }()

	a := New(s, slog.New(slog.DiscardHandler))
	cmd := &pb.Command{Id: "c1", Kind: &pb.Command_PauseDownload{PauseDownload: &pb.PauseDownload{DownloadId: "d"}}}

	r1 := a.HandleCommand(ctx, cmd)
	if r1.GetCommandId() != "c1" || r1.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED {
		t.Fatalf("first result: %v", r1)
	}

	r2 := a.HandleCommand(ctx, cmd)
	if r2.GetOutcome() != r1.GetOutcome() || r2.GetMessage() != r1.GetMessage() {
		t.Fatalf("replayed result differs: %v vs %v", r2, r1)
	}

	fresh, last, _, err := s.HelloState(ctx)
	if err != nil || fresh || last != "c1" {
		t.Fatalf("HelloState: fresh=%v last=%q err=%v", fresh, last, err)
	}
}
