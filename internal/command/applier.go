// Package command applies control-plane commands to local state, exactly
// once and in order, and answers queries. It implements link.Handler on top
// of store.Store.
package command

import (
	"context"
	"log/slog"
	"time"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// resultRetention matches the cloud's AGENT_COMMANDS stream retention
// (7 days); a command older than that cannot be redelivered.
const resultRetention = 7 * 24 * time.Hour

// Applier dispatches commands to their appliers inside a store transaction.
type Applier struct {
	store *store.Store
	log   *slog.Logger
}

// New returns an Applier backed by s.
func New(s *store.Store, log *slog.Logger) *Applier {
	if log == nil {
		log = slog.Default()
	}

	return &Applier{store: s, log: log.With("component", "command")}
}

// HandleCommand implements link.Handler. The state change and the recorded
// result commit together; a redelivered command gets the stored result.
func (a *Applier) HandleCommand(ctx context.Context, cmd *pb.Command) *pb.CommandResult {
	res, replayed, err := a.store.ApplyCommand(ctx, cmd, a.apply)
	if err != nil {
		// Nothing was recorded, so a redelivery will try again.
		a.log.Error("apply command", "command_id", cmd.GetId(), "err", err)

		return &pb.CommandResult{
			CommandId: cmd.GetId(),
			Outcome:   pb.CommandOutcome_COMMAND_OUTCOME_FAILED,
			ErrorCode: pb.CommandErrorCode_COMMAND_ERROR_CODE_INTERNAL,
			Message:   "agent could not apply the command",
		}
	}

	if replayed {
		a.log.Debug("replayed stored command result", "command_id", cmd.GetId(), "outcome", res.GetOutcome())
	}

	return res
}

// apply runs inside the command's transaction. No command kinds are
// implemented yet; each is rejected as UNSUPPORTED, which is recorded so a
// redelivery gets the same answer.
func (a *Applier) apply(_ context.Context, _ *store.Tx, _ *pb.Command) (*pb.CommandResult, error) {
	return &pb.CommandResult{
		Outcome:   pb.CommandOutcome_COMMAND_OUTCOME_REJECTED,
		ErrorCode: pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED,
		Message:   "this agent build does not implement this command yet",
	}, nil
}

// HandleQuery implements link.Handler. Queries are read-only and never
// touch the command log.
func (a *Applier) HandleQuery(_ context.Context, q *pb.Query) *pb.QueryResult {
	return &pb.QueryResult{QueryId: q.GetId(), Error: "unsupported"}
}

// HandleEventAck implements link.Handler by deleting acknowledged events
// from the outbox.
func (a *Applier) HandleEventAck(ctx context.Context, ack *pb.EventAck) {
	if _, err := a.store.AckEvents(ctx, ack.GetIds()); err != nil {
		// The events stay in the outbox and are resent next session; the
		// cloud deduplicates them.
		a.log.Error("delete acknowledged events", "err", err)
	}
}

// Prune drops command results older than the cloud can redeliver.
func (a *Applier) Prune(ctx context.Context) {
	n, err := a.store.PruneCommands(ctx, time.Now().Add(-resultRetention))
	if err != nil {
		a.log.Warn("prune command results", "err", err)
		return
	}

	if n > 0 {
		a.log.Debug("pruned command results", "count", n)
	}
}
