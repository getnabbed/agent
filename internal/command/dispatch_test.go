package command

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/getnabbed/agent/internal/clock"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

func newTestApplier(t *testing.T, clk *clock.Clock) *Applier {
	t.Helper()

	s, err := store.Open(context.Background(), t.TempDir(), "agent-1", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return New(Config{Store: s, Clock: clk, Log: slog.New(slog.DiscardHandler)})
}

// Every Command kind in the proto must reach its own handler. A kind added
// to the proto without a dispatch case falls through to "unknown command
// kind" and fails here.
func TestEveryCommandKindIsDispatched(t *testing.T) {
	a := newTestApplier(t, nil)

	od := (&pb.Command{}).ProtoReflect().Descriptor().Oneofs().ByName("kind")
	if od == nil || od.Fields().Len() == 0 {
		t.Fatal("Command has no kind oneof")
	}

	for i := range od.Fields().Len() {
		fd := od.Fields().Get(i)
		kind := string(fd.Name())

		t.Run(kind, func(t *testing.T) {
			cmd := &pb.Command{Id: "cmd-" + kind}
			m := cmd.ProtoReflect()
			m.Set(fd, protoreflect.ValueOfMessage(m.NewField(fd).Message()))

			res := a.HandleCommand(context.Background(), cmd)

			if res.GetCommandId() != cmd.GetId() {
				t.Fatalf("command_id = %q, want %q", res.GetCommandId(), cmd.GetId())
			}

			if res.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_REJECTED ||
				res.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED {
				t.Fatalf("result = %v, want REJECTED/UNSUPPORTED", res)
			}

			if !strings.HasPrefix(res.GetMessage(), kind+" ") {
				t.Fatalf("message = %q, want it to name %q (no dispatch case?)", res.GetMessage(), kind)
			}
		})
	}
}

func TestExpiredDeadlineIsRejected(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	a := newTestApplier(t, clock.NewFixed(func() time.Time { return now }))

	cmd := &pb.Command{
		Id:         "late",
		DeadlineMs: now.Add(-time.Second).UnixMilli(),
		Kind:       &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{RootId: "r"}},
	}

	res := a.HandleCommand(context.Background(), cmd)
	if res.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_DEADLINE_EXCEEDED {
		t.Fatalf("result = %v, want DEADLINE_EXCEEDED", res)
	}

	cmd.Id = "on-time"
	cmd.DeadlineMs = now.Add(time.Minute).UnixMilli()

	if res := a.HandleCommand(context.Background(), cmd); res.GetErrorCode() == pb.CommandErrorCode_COMMAND_ERROR_CODE_DEADLINE_EXCEEDED {
		t.Fatalf("command within its deadline was rejected: %v", res)
	}
}

func TestCommandWithoutKindIsRejected(t *testing.T) {
	a := newTestApplier(t, nil)

	res := a.HandleCommand(context.Background(), &pb.Command{Id: "empty"})
	if res.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED || !strings.Contains(res.GetMessage(), "no kind") {
		t.Fatalf("result = %v, want UNSUPPORTED no kind", res)
	}
}
