package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// ApplyFunc applies a command inside tx and returns the result to record.
// Returning an error rolls everything back and records nothing, so a
// redelivery tries again; return a result with outcome FAILED or REJECTED
// for deterministic failures that should be remembered.
type ApplyFunc func(ctx context.Context, tx *Tx, cmd *pb.Command) (*pb.CommandResult, error)

// ApplyCommand applies cmd exactly once. If cmd.Id was applied before, the
// stored result is returned with replayed=true and apply is not called.
// Otherwise apply runs and its result is recorded in the same transaction
// as its state changes.
func (s *Store) ApplyCommand(ctx context.Context, cmd *pb.Command, apply ApplyFunc) (res *pb.CommandResult, replayed bool, err error) {
	if cmd.GetId() == "" {
		return nil, false, errors.New("store: command has no id")
	}

	var enqueued bool

	err = s.inTx(ctx, func(sqlTx *sql.Tx) error {
		var stored []byte

		err := sqlTx.QueryRowContext(ctx, "SELECT result FROM applied_commands WHERE id = ?", cmd.GetId()).Scan(&stored)
		if err == nil {
			res = &pb.CommandResult{}
			replayed = true

			return proto.Unmarshal(stored, res)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		tx := &Tx{Tx: sqlTx}

		res, err = apply(ctx, tx, cmd)
		if err != nil {
			return err
		}

		if res == nil {
			return errors.New("store: apply returned no result")
		}

		res.CommandId = cmd.GetId()

		blob, err := proto.Marshal(res)
		if err != nil {
			return err
		}

		_, err = sqlTx.ExecContext(ctx,
			"INSERT INTO applied_commands(id, kind, outcome, result, applied_at) VALUES(?, ?, ?, ?, ?)",
			cmd.GetId(), oneofName(cmd), int64(res.GetOutcome()), blob, time.Now().UnixMilli())
		if err != nil {
			return fmt.Errorf("record command result: %w", err)
		}

		enqueued = tx.enqueued

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	if enqueued {
		s.signalEvents()
	}

	return res, replayed, nil
}

// PruneCommands deletes recorded results older than before, always keeping
// the newest row so Hello.last_command_id survives. The cloud's command log
// keeps entries for 7 days, so results older than that cannot be
// redelivered.
func (s *Store) PruneCommands(ctx context.Context, before time.Time) (int64, error) {
	r, err := s.db.ExecContext(ctx, `
		DELETE FROM applied_commands
		WHERE applied_at < ?
		  AND seq < (SELECT max(seq) FROM applied_commands)`, before.UnixMilli())
	if err != nil {
		return 0, err
	}

	return r.RowsAffected()
}

// oneofName returns the populated "kind" oneof field name of m, such as
// "start_download", for readable rows and logs.
func oneofName(m proto.Message) string {
	r := m.ProtoReflect()

	od := r.Descriptor().Oneofs().ByName("kind")
	if od == nil {
		return ""
	}

	if f := r.WhichOneof(od); f != nil {
		return string(f.Name())
	}

	return ""
}
