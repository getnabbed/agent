package command

import (
	"context"
	"errors"

	"github.com/getnabbed/agent/internal/library"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Library commands configure where and how completed downloads are
// imported. Root paths must sit under a locally allowed root.

// putLibraryRoot upserts a library root and its naming templates after
// checking the path against local policy and the templates for traversal.
func (a *Applier) putLibraryRoot(ctx context.Context, tx *store.Tx, c *pb.PutLibraryRoot) (*pb.CommandResult, error) {
	if a.library == nil {
		return notImplemented("put_library_root"), nil
	}

	if c.GetRoot() == nil {
		return rejected(pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_ARGUMENT, "put_library_root has no root"), nil
	}

	return libraryResult(a.library.Put(ctx, tx, c.GetRoot()))
}

// removeLibraryRoot removes a library root that no live download targets.
func (a *Applier) removeLibraryRoot(ctx context.Context, tx *store.Tx, c *pb.RemoveLibraryRoot) (*pb.CommandResult, error) {
	if a.library == nil {
		return notImplemented("remove_library_root"), nil
	}

	return libraryResult(a.library.Remove(ctx, tx, c.GetRootId()))
}

// setImportPolicy sets the import mode, path sanitiser and extras.
func (a *Applier) setImportPolicy(ctx context.Context, tx *store.Tx, c *pb.SetImportPolicy) (*pb.CommandResult, error) {
	if a.library == nil {
		return notImplemented("set_import_policy"), nil
	}

	return libraryResult(a.library.SetImportPolicy(ctx, tx, c))
}

// rescanLibrary starts a scan of a root; results arrive as InventoryReport
// events.
func (a *Applier) rescanLibrary(_ context.Context, _ *store.Tx, _ *pb.RescanLibrary) (*pb.CommandResult, error) {
	return notImplemented("rescan_library"), nil
}

// libraryResult maps library errors to typed rejections. Other errors abort
// the transaction and are reported as INTERNAL.
func libraryResult(err error) (*pb.CommandResult, error) {
	codes := []struct {
		err  error
		code pb.CommandErrorCode
	}{
		{library.ErrNotAllowed, pb.CommandErrorCode_COMMAND_ERROR_CODE_ROOT_NOT_ALLOWED},
		{library.ErrInUse, pb.CommandErrorCode_COMMAND_ERROR_CODE_ROOT_IN_USE},
		{library.ErrTemplateUnsafe, pb.CommandErrorCode_COMMAND_ERROR_CODE_TEMPLATE_UNSAFE},
		{library.ErrInvalid, pb.CommandErrorCode_COMMAND_ERROR_CODE_INVALID_ARGUMENT},
		{library.ErrLimit, pb.CommandErrorCode_COMMAND_ERROR_CODE_LIMIT_EXCEEDED},
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

// browseFilesystem answers BrowseFilesystem with a JSON library.BrowseResult
// in the payload. Only folders inside allowed roots are listed.
func (a *Applier) browseFilesystem(id string, q *pb.BrowseFilesystem) *pb.QueryResult {
	if a.library == nil {
		return &pb.QueryResult{QueryId: id, Error: "unsupported"}
	}

	payload, err := a.library.Policy().Browse(q.GetPath(), q.GetPageToken())
	if err != nil {
		return &pb.QueryResult{QueryId: id, Error: err.Error()}
	}

	return &pb.QueryResult{QueryId: id, Ok: true, Payload: payload}
}
