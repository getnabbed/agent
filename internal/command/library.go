package command

import (
	"context"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Library commands configure where and how completed downloads are
// imported. Root paths must sit under a locally allowed root.

// putLibraryRoot upserts a library root and its naming templates after
// checking the path against local policy and the templates for traversal.
func (a *Applier) putLibraryRoot(_ context.Context, _ *store.Tx, _ *pb.PutLibraryRoot) (*pb.CommandResult, error) {
	return notImplemented("put_library_root"), nil
}

// removeLibraryRoot removes a library root that no live download targets.
func (a *Applier) removeLibraryRoot(_ context.Context, _ *store.Tx, _ *pb.RemoveLibraryRoot) (*pb.CommandResult, error) {
	return notImplemented("remove_library_root"), nil
}

// setImportPolicy sets the import mode, path sanitiser and extras.
func (a *Applier) setImportPolicy(_ context.Context, _ *store.Tx, _ *pb.SetImportPolicy) (*pb.CommandResult, error) {
	return notImplemented("set_import_policy"), nil
}

// rescanLibrary starts a scan of a root; results arrive as InventoryReport
// events.
func (a *Applier) rescanLibrary(_ context.Context, _ *store.Tx, _ *pb.RescanLibrary) (*pb.CommandResult, error) {
	return notImplemented("rescan_library"), nil
}
