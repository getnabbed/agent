package download

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// row is one downloads table row.
type row struct {
	ID       string
	State    State
	Protocol pb.Protocol
	Spec     *pb.Download
	SourceID string
	Result   *engine.Result
	Attempts int
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var errNotFound = errors.New("download not found")

func get(ctx context.Context, q querier, id string) (*row, error) {
	var (
		r       row
		spec    []byte
		result  string
		state   int32
		proto32 int32
	)

	err := q.QueryRowContext(ctx,
		"SELECT id, state, protocol, spec, source_id, result, attempts FROM downloads WHERE id = ?", id).
		Scan(&r.ID, &state, &proto32, &spec, &r.SourceID, &result, &r.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}

	if err != nil {
		return nil, err
	}

	r.State, r.Protocol = State(state), pb.Protocol(proto32)

	r.Spec = &pb.Download{}
	if err := proto.Unmarshal(spec, r.Spec); err != nil {
		return nil, fmt.Errorf("decode download %s: %w", id, err)
	}

	if result != "" {
		r.Result = &engine.Result{}
		if err := json.Unmarshal([]byte(result), r.Result); err != nil {
			return nil, fmt.Errorf("decode result of %s: %w", id, err)
		}
	}

	return &r, nil
}

func insert(ctx context.Context, tx *store.Tx, d *pb.Download) error {
	spec, err := proto.Marshal(d)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO downloads (id, acquisition_id, state, protocol, spec, priority, source_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.GetId(), d.GetAcquisitionId(), int32(Queued), int32(d.GetRelease().GetProtocol()), spec,
		d.GetPriority(), d.GetSourceId(), now, now)
	if err != nil {
		return err
	}

	return emit(ctx, tx, d.GetId(), Queued, nil, extra{})
}

// extra is what a DownloadUpdate carries beyond its state and failure.
type extra struct {
	notes []*pb.Note
	stats *pb.DownloadStats
}

// transition moves id from one state to another only if it is still in
// from, records the failure (or clears it) and emits DownloadUpdate. It
// reports whether the row changed.
func transition(ctx context.Context, tx *store.Tx, id string, from, to State, f *engine.Failure, x extra) (bool, error) {
	var (
		class     int32
		retryable bool
		detail    string
	)

	if f != nil {
		class, retryable, detail = int32(f.Class), f.Retryable, f.Detail
	}

	r, err := tx.ExecContext(ctx, `
		UPDATE downloads
		SET state = ?, failure_class = ?, failure_retryable = ?, failure_detail = ?, updated_at = ?,
			failed_from = CASE WHEN ? THEN ? ELSE failed_from END
		WHERE id = ? AND state = ?`,
		int32(to), class, retryable, detail, time.Now().UnixMilli(), to == Failed, int32(from), id, int32(from))
	if err != nil {
		return false, err
	}

	if n, _ := r.RowsAffected(); n == 0 {
		return false, nil
	}

	var pf *pb.Failure
	if f != nil {
		pf = f.Proto()
	}

	return true, emit(ctx, tx, id, to, pf, x)
}

func emit(ctx context.Context, tx *store.Tx, id string, s State, f *pb.Failure, x extra) error {
	_, err := tx.Enqueue(ctx, &pb.Event{Kind: &pb.Event_DownloadUpdate{DownloadUpdate: &pb.DownloadUpdate{
		DownloadId: id, State: s, Failure: f, Notes: x.notes, Stats: x.stats,
	}}})

	return err
}

func setResult(ctx context.Context, tx *store.Tx, id string, res *engine.Result) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, "UPDATE downloads SET result = ? WHERE id = ?", string(raw), id)

	return err
}

// candidate is a download a runner could work on.
type candidate struct {
	ID    string
	State State
}

// runnable lists downloads in running states: interrupted ones (past
// QUEUED) first so a restart finishes work before starting new work, then
// by priority and age.
func runnable(ctx context.Context, db *sql.DB, limit int) ([]candidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, state FROM downloads
		WHERE state IN (?, ?, ?, ?, ?)
		ORDER BY state = ? ASC, priority DESC, created_at ASC
		LIMIT ?`,
		int32(Queued), int32(Fetching), int32(Downloading), int32(Assembling), int32(Importing), int32(Queued), limit)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []candidate

	for rows.Next() {
		var (
			c candidate
			s int32
		)

		if err := rows.Scan(&c.ID, &s); err != nil {
			return nil, err
		}

		c.State = State(s)
		out = append(out, c)
	}

	return out, rows.Err()
}

func uncleanedCancelled(ctx context.Context, db *sql.DB, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id FROM downloads WHERE state = ? AND data_removed = 0 LIMIT ?", int32(Cancelled), limit)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		out = append(out, id)
	}

	return out, rows.Err()
}

func markDataRemoved(ctx context.Context, db *sql.DB, id string) error {
	_, err := db.ExecContext(ctx, "UPDATE downloads SET data_removed = 1 WHERE id = ? AND state = ?", id, int32(Cancelled))

	return err
}

func gcFinished(ctx context.Context, s *store.Store, cancelledBefore, doneBefore time.Time) (int64, error) {
	var n int64

	err := s.Update(ctx, func(tx *store.Tx) error {
		r, err := tx.ExecContext(ctx, `
			DELETE FROM downloads
			WHERE (state = ? AND data_removed = 1 AND updated_at < ?)
			   OR (state = ? AND updated_at < ?)`,
			int32(Cancelled), cancelledBefore.UnixMilli(), int32(Done), doneBefore.UnixMilli())
		if err != nil {
			return err
		}

		n, err = r.RowsAffected()

		return err
	})

	return n, err
}

// countUnfinished counts downloads that are not DONE or CANCELLED.
func countUnfinished(ctx context.Context, q querier) (int, error) {
	var n int

	err := q.QueryRowContext(ctx, "SELECT count(*) FROM downloads WHERE state NOT IN (?, ?)",
		int32(Done), int32(Cancelled)).Scan(&n)

	return n, err
}
