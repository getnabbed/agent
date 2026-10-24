package command

import (
	"context"
	"errors"

	"github.com/getnabbed/agent/internal/source"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Source commands manage download providers. Their configuration is sealed
// to the agent's X25519 key and opened only in memory.

// putSource upserts a source after checking it can be unsealed and that its
// host is not on a local network.
func (a *Applier) putSource(ctx context.Context, tx *store.Tx, c *pb.PutSource) (*pb.CommandResult, error) {
	if a.sources == nil {
		return notImplemented("put_source"), nil
	}

	if c.GetSource() == nil {
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_ARGUMENT, "put_source has no source"), nil
	}

	return sourceResult(a.sources.Put(ctx, tx, c.GetSource()))
}

// removeSource removes a source. Downloads using it fall back to the other
// providers, or fail as retryable if none remain.
func (a *Applier) removeSource(ctx context.Context, tx *store.Tx, c *pb.RemoveSource) (*pb.CommandResult, error) {
	if a.sources == nil {
		return notImplemented("remove_source"), nil
	}

	return sourceResult(a.sources.Remove(ctx, tx, c.GetSourceId()))
}

// sourceResult maps source errors to typed rejections. Other errors abort
// the transaction and are reported as INTERNAL.
func sourceResult(err error) (*pb.CommandResult, error) {
	codes := []struct {
		err  error
		code pb.CommandErrorCode
	}{
		{source.ErrKindUnsupported, pb.CommandErrorCode_COMMAND_ERROR_CODE_SOURCE_KIND_UNSUPPORTED},
		{source.ErrUnsealable, pb.CommandErrorCode_COMMAND_ERROR_CODE_SOURCE_UNSEALABLE},
		{source.ErrHostBlocked, pb.CommandErrorCode_COMMAND_ERROR_CODE_SOURCE_HOST_BLOCKED},
		{source.ErrInvalid, pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_ARGUMENT},
		{source.ErrLimit, pb.CommandErrorCode_COMMAND_ERROR_CODE_LIMIT_EXCEEDED},
	}

	if err == nil {
		return applied(""), nil
	}

	for _, c := range codes {
		if errors.Is(err, c.err) {
			return rejected(c.code, err.Error()), nil
		}
	}

	return nil, err
}
