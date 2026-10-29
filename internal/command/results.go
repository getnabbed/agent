package command

import (
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// applied is the result for a command that took effect as asked.
func applied(msg string) *pb.CommandResult {
	return &pb.CommandResult{Outcome: pb.CommandOutcome_COMMAND_OUTCOME_APPLIED, Message: msg}
}

// rejected is the result for a command the agent refuses, such as one that
// breaks local policy. Rejections are recorded and replayed on redelivery.
func rejected(code pb.CommandErrorCode, msg string) *pb.CommandResult {
	return &pb.CommandResult{Outcome: pb.CommandOutcome_COMMAND_OUTCOME_REJECTED, ErrorCode: code, Message: msg}
}

// notImplemented rejects a command kind this build has a handler slot for
// but no behaviour yet.
func notImplemented(kind string) *pb.CommandResult {
	return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED, kind+" is not implemented in this agent build")
}

// capped is the result for a command applied only in part, or with other
// values than asked, because of a local limit. msg says what is in effect.
func capped(msg string) *pb.CommandResult {
	return &pb.CommandResult{Outcome: pb.CommandOutcome_COMMAND_OUTCOME_APPLIED_CAPPED, Message: msg}
}
