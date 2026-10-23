package link

import (
	"context"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// UnsupportedHandler answers every command REJECTED/UNSUPPORTED and every
// query with an error. It is the placeholder until the command applier and
// query pool exist; rejecting (rather than staying silent) lets the cloud
// ack its command log and surface a typed outcome in the UI.
type UnsupportedHandler struct{}

// HandleCommand implements Handler.
func (UnsupportedHandler) HandleCommand(_ context.Context, cmd *pb.Command) *pb.CommandResult {
	return &pb.CommandResult{
		CommandId: cmd.GetId(),
		Outcome:   pb.CommandOutcome_COMMAND_OUTCOME_REJECTED,
		ErrorCode: pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED,
		Message:   "this agent build does not implement commands yet",
	}
}

// HandleQuery implements Handler.
func (UnsupportedHandler) HandleQuery(_ context.Context, q *pb.Query) *pb.QueryResult {
	return &pb.QueryResult{QueryId: q.GetId(), Error: "unsupported"}
}

// HandleEventAck implements Handler.
func (UnsupportedHandler) HandleEventAck(context.Context, *pb.EventAck) {}

// StatelessState is LocalState for an agent with no local store yet: always
// fresh, so the cloud rehydrates configuration on every connect.
type StatelessState struct{}

// Fresh implements LocalState.
func (StatelessState) Fresh() bool { return true }

// LastCommandID implements LocalState.
func (StatelessState) LastCommandID() string { return "" }

// PendingEventCount implements LocalState.
func (StatelessState) PendingEventCount() uint32 { return 0 }
