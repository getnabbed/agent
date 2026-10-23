package command

import (
	"context"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Settings commands adjust runtime limits within local ceilings.

// setAgentSettings applies concurrency, bandwidth and host-stats settings,
// answering APPLIED_CAPPED with the values actually applied when a local
// ceiling binds.
func (a *Applier) setAgentSettings(_ context.Context, _ *store.Tx, _ *pb.SetAgentSettings) (*pb.CommandResult, error) {
	return notImplemented("set_agent_settings"), nil
}
