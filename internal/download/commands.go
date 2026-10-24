package download

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Errors returned by command operations; the command layer maps them to
// CommandErrorCodes.
var (
	ErrInvalidArgument     = errors.New("invalid argument")
	ErrUnknownDownload     = errors.New("unknown download")
	ErrInvalidTransition   = errors.New("invalid transition")
	ErrProtocolUnavailable = errors.New("protocol unavailable")
	ErrLimitExceeded       = errors.New("limit exceeded")
)

// ErrAlreadyExists is returned by Start for a download id already known.
// Start is idempotent, so the command still succeeds.
var ErrAlreadyExists = errors.New("download already exists")

const (
	maxIDLen = 128
	// maxSpecBytes bounds a stored download spec. Real specs are a few KB.
	maxSpecBytes = 64 << 10
	// maxUnfinished bounds downloads that are not DONE or CANCELLED.
	maxUnfinished = 1000
)

// Start records a new download as QUEUED inside the command's transaction.
// The runner picks it up after the transaction commits and Kick is called.
func (m *Manager) Start(ctx context.Context, tx *store.Tx, sd *pb.StartDownload) error {
	d := sd.GetDownload()
	if err := validate(d); err != nil {
		return err
	}

	p := d.GetRelease().GetProtocol()
	if e, ok := m.engines[p]; !ok || !e.Available() {
		return fmt.Errorf("%w: %s", ErrProtocolUnavailable, protocolName(p))
	}

	if _, err := get(ctx, tx, d.GetId()); err == nil {
		return ErrAlreadyExists
	} else if !errors.Is(err, errNotFound) {
		return err
	}

	n, err := countUnfinished(ctx, tx)
	if err != nil {
		return err
	}

	if n >= maxUnfinished {
		return fmt.Errorf("%w: %d unfinished downloads (limit %d); cancel some first", ErrLimitExceeded, n, maxUnfinished)
	}

	return insert(ctx, tx, d)
}

func validate(d *pb.Download) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
	}

	if d == nil {
		return bad("download is missing")
	}

	if size := proto.Size(d); size > maxSpecBytes {
		return bad("download spec is %d bytes, limit %d", size, maxSpecBytes)
	}

	if d.GetId() == "" || len(d.GetId()) > maxIDLen {
		return bad("download id must be 1-%d bytes", maxIDLen)
	}

	rel := d.GetRelease()
	if rel == nil {
		return bad("release is missing")
	}

	ref := rel.GetPayloadRef()

	switch rel.GetProtocol() {
	case pb.Protocol_PROTOCOL_USENET:
		if ref == nil || ref.GetKind() != pb.PayloadKind_PAYLOAD_KIND_NZB {
			return bad("usenet release needs an NZB payload")
		}
	case pb.Protocol_PROTOCOL_TORRENT:
		if ref == nil && !strings.HasPrefix(rel.GetMagnet(), "magnet:?") {
			return bad("torrent release needs a .torrent payload or a magnet link")
		}

		if ref != nil && ref.GetKind() != pb.PayloadKind_PAYLOAD_KIND_TORRENT {
			return bad("torrent release payload must be a .torrent")
		}
	default:
		return bad("unknown protocol %s", rel.GetProtocol())
	}

	if ref != nil {
		if ref.GetId() == "" || len(ref.GetSha256()) != 32 || ref.GetSize() == 0 || ref.GetSize() > maxPayloadBytes {
			return bad("payload ref needs an id, a 32-byte sha256 and a size up to %d bytes", maxPayloadBytes)
		}
	}

	return nil
}

// Act applies a pause, resume, retry or cancel inside the command's
// transaction.
func (m *Manager) Act(ctx context.Context, tx *store.Tx, action Action, id string) error {
	r, err := get(ctx, tx, id)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownDownload, id)
	}

	if err != nil {
		return err
	}

	to, ok := Target(action, r.State)
	if !ok {
		return fmt.Errorf("%w: cannot %s a download that is %s", ErrInvalidTransition, action, stateName(r.State))
	}

	if action == Retry {
		var failedFrom int32
		if err := tx.QueryRowContext(ctx, "SELECT failed_from FROM downloads WHERE id = ?", id).Scan(&failedFrom); err != nil {
			return err
		}

		// A failed import or assembly is retried where it failed, with the
		// files already on disk: nothing is fetched or downloaded again.
		reset := ", result = ''"
		if s := State(failedFrom); (s == Assembling || s == Importing) && r.Result != nil {
			to, reset = s, ""
		}

		if _, err := tx.ExecContext(ctx, "UPDATE downloads SET attempts = attempts + 1"+reset+" WHERE id = ?", id); err != nil {
			return err
		}
	}

	if _, err := transition(ctx, tx, id, r.State, to, nil); err != nil {
		return err
	}

	// Stop the runner as soon as the command commits. Left to the next
	// reconcile, a resume landing first would put the download back in a
	// running state and the paused runner would carry on unnoticed.
	if action == Pause || action == Cancel {
		tx.AfterCommit(func() { m.stop(id) })
	}

	return nil
}

// Update changes priority or source of a download that has not finished.
func (m *Manager) Update(ctx context.Context, tx *store.Tx, u *pb.UpdateDownload) error {
	r, err := get(ctx, tx, u.GetDownloadId())
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("%w: %s", ErrUnknownDownload, u.GetDownloadId())
	}

	if err != nil {
		return err
	}

	if r.State == Done || r.State == Cancelled {
		return fmt.Errorf("%w: cannot update a download that is %s", ErrInvalidTransition, stateName(r.State))
	}

	if u.Priority != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE downloads SET priority = ?, updated_at = ? WHERE id = ?", u.GetPriority(), time.Now().UnixMilli(), r.ID); err != nil {
			return err
		}
	}

	if u.SourceId != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE downloads SET source_id = ?, updated_at = ? WHERE id = ?", u.GetSourceId(), time.Now().UnixMilli(), r.ID); err != nil {
			return err
		}
	}

	return nil
}

func stateName(s State) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "DOWNLOAD_STATE_"))
}
