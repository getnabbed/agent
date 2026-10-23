package store

import (
	"context"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/getnabbed/agent/internal/ids"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Enqueue adds ev to the outbox as part of tx and returns its id, which
// becomes Header.id on the wire and the cloud's dedup key. The event is
// visible to the sender only once tx commits.
func (tx *Tx) Enqueue(ctx context.Context, ev *pb.Event) (string, error) {
	blob, err := proto.Marshal(ev)
	if err != nil {
		return "", err
	}

	id := ids.NewV7()

	_, err = tx.ExecContext(ctx,
		"INSERT INTO outbox(id, kind, payload, created_at) VALUES(?, ?, ?, ?)",
		id, oneofName(ev), blob, time.Now().UnixMilli())
	if err != nil {
		return "", err
	}

	tx.enqueued = true

	return id, nil
}

// PendingEvents returns up to limit unacknowledged events with seq greater
// than afterSeq, oldest first, and the seq of the last one returned (or
// afterSeq if none).
func (s *Store) PendingEvents(ctx context.Context, afterSeq int64, limit int) (eventIDs []string, events []*pb.Event, lastSeq int64, err error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT seq, id, payload FROM outbox WHERE seq > ? ORDER BY seq LIMIT ?", afterSeq, limit)
	if err != nil {
		return nil, nil, afterSeq, err
	}

	defer func() { _ = rows.Close() }()

	lastSeq = afterSeq

	for rows.Next() {
		var (
			seq  int64
			id   string
			blob []byte
		)

		if err := rows.Scan(&seq, &id, &blob); err != nil {
			return nil, nil, afterSeq, err
		}

		ev := &pb.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return nil, nil, afterSeq, err
		}

		eventIDs = append(eventIDs, id)
		events = append(events, ev)
		lastSeq = seq
	}

	return eventIDs, events, lastSeq, rows.Err()
}

// AckEvents deletes acknowledged events. Unknown ids are ignored, since acks
// can repeat after a reconnect.
func (s *Store) AckEvents(ctx context.Context, eventIDs []string) (int64, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}

	args := make([]any, len(eventIDs))
	for i, id := range eventIDs {
		args[i] = id
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventIDs)), ",")

	r, err := s.db.ExecContext(ctx, "DELETE FROM outbox WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return 0, err
	}

	return r.RowsAffected()
}

// EventsAdded is signalled after a commit that enqueued events. It is
// buffered by one, so a burst of commits produces at least one wake-up.
func (s *Store) EventsAdded() <-chan struct{} { return s.events }

func (s *Store) signalEvents() {
	select {
	case s.events <- struct{}{}:
	default:
	}
}
