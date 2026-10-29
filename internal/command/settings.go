package command

import (
	"context"
	"fmt"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Settings commands adjust runtime limits within local ceilings.

// setAgentSettings applies the host_stats flag. Concurrency and bandwidth
// limits aren't supported yet (design 0008 §3): asking for them still
// applies host_stats, which was recorded, so the answer is APPLIED_CAPPED
// rather than REJECTED, which would say nothing took effect. Zero means
// not asked, since proto3 can't tell it apart from unset.
func (a *Applier) setAgentSettings(ctx context.Context, tx *store.Tx, s *pb.SetAgentSettings) (*pb.CommandResult, error) {
	if a.settings == nil {
		return notImplemented("set_agent_settings"), nil
	}

	if err := a.settings.SetHostStats(ctx, tx, s.GetHostStats()); err != nil {
		return nil, err
	}

	state := "off"
	if s.GetHostStats() {
		state = "on"
	}

	if s.GetMaxConcurrent() != 0 || s.GetMaxBps() != 0 {
		return capped(fmt.Sprintf("host stats %s; max_concurrent and max_bps are not supported by this agent yet, so its local limits stay in effect", state)), nil
	}

	return applied("host stats " + state), nil
}
