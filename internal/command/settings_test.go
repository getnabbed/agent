package command

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/hoststats"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

func settingsCommand(id string, s *pb.SetAgentSettings) *pb.Command {
	return &pb.Command{Id: id, Kind: &pb.Command_SetAgentSettings{SetAgentSettings: s}}
}

func TestSetAgentSettings(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	s, err := store.Open(ctx, t.TempDir(), "agent-1", log)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	// Without settings support the command is refused as before.
	res := New(Config{Store: s, Log: log}).HandleCommand(ctx, settingsCommand("c0", &pb.SetAgentSettings{HostStats: true}))
	if res.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_REJECTED || res.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED {
		t.Fatalf("got %v, want REJECTED UNSUPPORTED without settings", res)
	}

	mon, err := hoststats.New(ctx, hoststats.Config{Store: s, DownloadsDir: t.TempDir(), Log: log})
	if err != nil {
		t.Fatal(err)
	}

	a := New(Config{Store: s, Settings: mon, Log: log})

	steps := []struct {
		id      string
		cmd     *pb.SetAgentSettings
		outcome pb.CommandOutcome
		on      bool
		msg     string
	}{
		{"c1", &pb.SetAgentSettings{HostStats: true}, pb.CommandOutcome_COMMAND_OUTCOME_APPLIED, true, "host stats on"},
		{"c2", &pb.SetAgentSettings{HostStats: false}, pb.CommandOutcome_COMMAND_OUTCOME_APPLIED, false, "host stats off"},
		{"c3", &pb.SetAgentSettings{HostStats: true, MaxConcurrent: 5}, pb.CommandOutcome_COMMAND_OUTCOME_APPLIED_CAPPED, true, "not supported"},
		{"c4", &pb.SetAgentSettings{MaxBps: 1_000_000}, pb.CommandOutcome_COMMAND_OUTCOME_APPLIED_CAPPED, false, "not supported"},
	}

	for _, st := range steps {
		res := a.HandleCommand(ctx, settingsCommand(st.id, st.cmd))
		if res.GetOutcome() != st.outcome || !strings.Contains(res.GetMessage(), st.msg) {
			t.Fatalf("%s: got %v, want %v mentioning %q", st.id, res, st.outcome, st.msg)
		}

		if mon.HostStats() != st.on {
			t.Fatalf("%s: got host stats %v, want %v", st.id, mon.HostStats(), st.on)
		}
	}

	// The setting is stored: a restarted monitor reads it back.
	if res := a.HandleCommand(ctx, settingsCommand("c5", &pb.SetAgentSettings{HostStats: true})); res.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_APPLIED {
		t.Fatalf("got %v, want APPLIED", res)
	}

	again, err := hoststats.New(ctx, hoststats.Config{Store: s, DownloadsDir: t.TempDir(), Log: log})
	if err != nil {
		t.Fatal(err)
	}

	if !again.HostStats() {
		t.Fatalf("got host stats off in a new monitor, want the stored setting on")
	}
}
