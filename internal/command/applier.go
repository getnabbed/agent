// Package command applies control-plane commands to local state, exactly
// once and in order, and answers queries. It implements link.Handler on top
// of store.Store.
package command

import (
	"context"
	"log/slog"
	"time"

	"github.com/getnabbed/agent/internal/clock"
	"github.com/getnabbed/agent/internal/download"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// resultRetention matches the cloud's AGENT_COMMANDS stream retention
// (7 days); a command older than that cannot be redelivered.
const resultRetention = 7 * 24 * time.Hour

// Downloads is the download state machine as seen by command handlers.
// *download.Manager implements it.
type Downloads interface {
	Start(ctx context.Context, tx *store.Tx, sd *pb.StartDownload) error
	Act(ctx context.Context, tx *store.Tx, action download.Action, id string) error
	Update(ctx context.Context, tx *store.Tx, u *pb.UpdateDownload) error
	// Kick wakes the scheduler after a command commits.
	Kick()
}

// Config configures an Applier.
type Config struct {
	Store *store.Store
	// Clock supplies control-plane-corrected time for command deadlines;
	// nil uses the host clock.
	Clock *clock.Clock
	// Downloads is optional; without it download commands are rejected as
	// not implemented.
	Downloads Downloads
	Log       *slog.Logger
}

// Applier dispatches commands to their handlers inside a store transaction.
type Applier struct {
	store     *store.Store
	clock     *clock.Clock
	downloads Downloads
	log       *slog.Logger
}

// New returns an Applier.
func New(cfg Config) *Applier {
	if cfg.Clock == nil {
		cfg.Clock = clock.New()
	}

	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	return &Applier{store: cfg.Store, clock: cfg.Clock, downloads: cfg.Downloads, log: cfg.Log.With("component", "command")}
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

	if a.downloads != nil {
		a.downloads.Kick()
	}

	return res
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
