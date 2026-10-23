package store

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

var ctx = context.Background()

func openTest(t *testing.T, dir, agentID string) *Store {
	t.Helper()

	s, err := Open(ctx, dir, agentID, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func cmd(id string) *pb.Command {
	return &pb.Command{Id: id, Kind: &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{RootId: "r"}}}
}

func notice(code string) *pb.Event {
	return &pb.Event{Kind: &pb.Event_AgentNotice{AgentNotice: &pb.AgentNotice{Code: code}}}
}

func applied(_ context.Context, _ *Tx, _ *pb.Command) (*pb.CommandResult, error) {
	return &pb.CommandResult{Outcome: pb.CommandOutcome_COMMAND_OUTCOME_APPLIED}, nil
}

func TestFreshUntilFirstCommand(t *testing.T) {
	s := openTest(t, t.TempDir(), "agent-1")

	fresh, last, pending, err := s.HelloState(ctx)
	if err != nil || !fresh || last != "" || pending != 0 {
		t.Fatalf("new store: fresh=%v last=%q pending=%d err=%v", fresh, last, pending, err)
	}

	if _, _, err := s.ApplyCommand(ctx, cmd("c1"), applied); err != nil {
		t.Fatal(err)
	}

	fresh, last, _, _ = s.HelloState(ctx)
	if fresh || last != "c1" {
		t.Fatalf("after apply: fresh=%v last=%q, want false c1", fresh, last)
	}
}

func TestStatePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	s := openTest(t, dir, "agent-1")
	if _, _, err := s.ApplyCommand(ctx, cmd("c1"), applied); err != nil {
		t.Fatal(err)
	}

	_ = s.Close()

	s2 := openTest(t, dir, "agent-1")

	fresh, last, _, err := s2.HelloState(ctx)
	if err != nil || fresh || last != "c1" {
		t.Fatalf("after reopen: fresh=%v last=%q err=%v", fresh, last, err)
	}
}

func TestOtherAgentStateIsMovedAside(t *testing.T) {
	dir := t.TempDir()

	s := openTest(t, dir, "agent-old")
	if _, _, err := s.ApplyCommand(ctx, cmd("c1"), applied); err != nil {
		t.Fatal(err)
	}

	_ = s.Close()

	s2 := openTest(t, dir, "agent-new")

	fresh, _, _, _ := s2.HelloState(ctx)
	if !fresh {
		t.Fatal("state from a previous agent identity was reused")
	}

	matches, _ := filepath.Glob(filepath.Join(dir, FileName+".agent-old.*.bak"))
	if len(matches) != 1 {
		t.Fatalf("expected the old database to be kept as a backup, found %v", matches)
	}
}

func TestApplyCommandIsExactlyOnce(t *testing.T) {
	s := openTest(t, t.TempDir(), "a")

	calls := 0
	apply := func(ctx context.Context, tx *Tx, _ *pb.Command) (*pb.CommandResult, error) {
		calls++

		if _, err := tx.Enqueue(ctx, notice("applied")); err != nil {
			return nil, err
		}

		return &pb.CommandResult{Outcome: pb.CommandOutcome_COMMAND_OUTCOME_REJECTED, Message: "first"}, nil
	}

	r1, replayed, err := s.ApplyCommand(ctx, cmd("c1"), apply)
	if err != nil || replayed || r1.GetCommandId() != "c1" {
		t.Fatalf("first apply: %v replayed=%v err=%v", r1, replayed, err)
	}

	r2, replayed, err := s.ApplyCommand(ctx, cmd("c1"), apply)
	if err != nil || !replayed || r2.GetMessage() != "first" || r2.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_REJECTED {
		t.Fatalf("redelivery: %v replayed=%v err=%v", r2, replayed, err)
	}

	if calls != 1 {
		t.Fatalf("apply called %d times, want 1", calls)
	}

	_, _, pending, _ := s.HelloState(ctx)
	if pending != 1 {
		t.Fatalf("pending events = %d, want 1 (no duplicate on replay)", pending)
	}
}

func TestApplyErrorRollsBackEverything(t *testing.T) {
	s := openTest(t, t.TempDir(), "a")

	boom := errors.New("boom")
	_, _, err := s.ApplyCommand(ctx, cmd("c1"), func(ctx context.Context, tx *Tx, _ *pb.Command) (*pb.CommandResult, error) {
		if _, err := tx.Enqueue(ctx, notice("x")); err != nil {
			return nil, err
		}

		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}

	fresh, _, pending, _ := s.HelloState(ctx)
	if !fresh || pending != 0 {
		t.Fatalf("rollback incomplete: fresh=%v pending=%d", fresh, pending)
	}

	select {
	case <-s.EventsAdded():
		t.Fatal("events signalled for a rolled-back transaction")
	default:
	}
}

func TestOutboxOrderCursorAndAck(t *testing.T) {
	s := openTest(t, t.TempDir(), "a")

	err := s.Update(ctx, func(tx *Tx) error {
		for _, code := range []string{"one", "two", "three"} {
			if _, err := tx.Enqueue(ctx, notice(code)); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-s.EventsAdded():
	default:
		t.Fatal("commit with events did not signal")
	}

	idsA, evA, cursor, err := s.PendingEvents(ctx, 0, 2)
	if err != nil || len(evA) != 2 || evA[0].GetAgentNotice().GetCode() != "one" || evA[1].GetAgentNotice().GetCode() != "two" {
		t.Fatalf("first page: %v err=%v", evA, err)
	}

	_, evB, _, _ := s.PendingEvents(ctx, cursor, 10)
	if len(evB) != 1 || evB[0].GetAgentNotice().GetCode() != "three" {
		t.Fatalf("second page: %v", evB)
	}

	n, err := s.AckEvents(ctx, append(idsA, "unknown-id"))
	if err != nil || n != 2 {
		t.Fatalf("AckEvents = %d, %v; want 2", n, err)
	}

	_, rest, _, _ := s.PendingEvents(ctx, 0, 10)
	if len(rest) != 1 {
		t.Fatalf("after ack, pending = %d, want 1", len(rest))
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	s := openTest(t, t.TempDir(), "a")

	for _, id := range []string{"c1", "c2", "c3"} {
		if _, _, err := s.ApplyCommand(ctx, cmd(id), applied); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.PruneCommands(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneCommands = %d, %v; want 2", n, err)
	}

	_, last, _, _ := s.HelloState(ctx)
	if last != "c3" {
		t.Fatalf("last command after prune = %q, want c3", last)
	}
}

func TestNewerSchemaIsRejected(t *testing.T) {
	dir := t.TempDir()

	s := openTest(t, dir, "a")
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}

	_ = s.Close()

	_, err := Open(ctx, dir, "a", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "upgrade the agent") {
		t.Fatalf("err = %v, want a downgrade error", err)
	}
}

func TestDatabaseFileStaysInDataDir(t *testing.T) {
	dir := t.TempDir()
	openTest(t, dir, "a")

	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
}

func TestInspectIsReadOnly(t *testing.T) {
	dir := t.TempDir()

	if _, err := Inspect(ctx, dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect on empty dir err = %v, want not exist", err)
	}

	s := openTest(t, dir, "agent-1")
	if _, _, err := s.ApplyCommand(ctx, cmd("c1"), applied); err != nil {
		t.Fatal(err)
	}

	sum, err := Inspect(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	if sum.AgentID != "agent-1" || sum.LastCommandID != "c1" || sum.AppliedCount != 1 || sum.SchemaVersion < 1 {
		t.Fatalf("Inspect = %+v", sum)
	}

	// A read-only connection must refuse writes.
	db, err := sqlOpenRO(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(ctx, "DELETE FROM outbox"); err == nil {
		t.Fatal("read-only connection accepted a write")
	}
}
