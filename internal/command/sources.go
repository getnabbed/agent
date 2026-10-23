package command

import (
	"context"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Source commands manage download clients and providers. Their configuration
// is sealed to the agent's X25519 key and opened only in memory.

// putSource upserts a source after checking it can be unsealed and that
// its target passes the SSRF guard; may answer PENDING_APPROVAL when
// local approval is required.
func (a *Applier) putSource(_ context.Context, _ *store.Tx, _ *pb.PutSource) (*pb.CommandResult, error) {
	return notImplemented("put_source"), nil
}

// removeSource removes a source that no live download uses.
func (a *Applier) removeSource(_ context.Context, _ *store.Tx, _ *pb.RemoveSource) (*pb.CommandResult, error) {
	return notImplemented("remove_source"), nil
}
