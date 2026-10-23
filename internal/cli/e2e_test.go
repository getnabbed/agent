package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/getnabbed/agent/internal/identity"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

type e2eGateway struct {
	pb.UnimplementedAgentGatewayServiceServer
	mu    sync.Mutex
	pub   ed25519.PublicKey
	hello chan *pb.Hello
}

func (g *e2eGateway) Connect(srv pb.AgentGatewayService_ConnectServer) error {
	md, _ := metadata.FromIncomingContext(srv.Context())
	if a := md.Get("authorization"); len(a) != 1 || a[0] != "Bearer "+e2eJWT {
		return nil
	}

	m, err := srv.Recv()
	if err != nil {
		return err
	}

	h := m.GetHello()
	g.mu.Lock()
	pub := g.pub
	g.mu.Unlock()
	if h == nil || !identity.Verify(pub, identity.HelloSigningInput("jti-1", h.GetInstanceId(), h.GetSignedAtMs()), h.GetSignature()) {
		return nil
	}

	g.hello <- h
	_ = srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Welcome{Welcome: &pb.Welcome{SessionId: "s1", HeartbeatMs: 1000}}})

	// One command, answered and recorded before the agent is revoked.
	_ = srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Command{Command: &pb.Command{Id: "cmd-e2e", Kind: &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{RootId: "r"}}}}})

	for {
		m, err := srv.Recv()
		if err != nil {
			return err
		}

		if m.GetCommandResult().GetCommandId() == "cmd-e2e" {
			break
		}
	}

	return srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_GoAway{GoAway: &pb.GoAway{Reason: pb.GoAwayReason_GO_AWAY_REASON_REVOKED}}})
}

var e2eJWT = func() string {
	now := time.Now().Unix()
	c, _ := json.Marshal(map[string]any{"jti": "jti-1", "iat": now, "exp": now + 1800})
	return "e30." + base64.RawURLEncoding.EncodeToString(c) + ".s"
}()

func TestRunPairsConnectsAndStopsOnRevoke(t *testing.T) {
	gw := &e2eGateway{hello: make(chan *pb.Hello, 1)}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	gs := grpc.NewServer()
	pb.RegisterAgentGatewayServiceServer(gs, gw)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/device/authorize", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PublicKeys struct{ Ed25519 string } `json:"public_keys"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)
		pub, _ := base64.RawURLEncoding.DecodeString(req.PublicKeys.Ed25519)
		gw.mu.Lock()
		gw.pub = pub
		gw.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "WXYZ-1234", "verification_uri": "https://app.example/device", "interval": 1})
	})
	mux.HandleFunc("POST /api/device/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": "agent-9", "tenant_id": "t-9", "access_token": e2eJWT, "refresh_token": "rt", "gateway_url": "http://" + lis.Addr().String()})
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	code := Run(ctx, []string{"--data-dir", dir, "--log-format", "json", "run", "--api-url", api.URL, "--name", "e2e"}, &out, &errb)

	if code != ExitReauth || !strings.Contains(errb.String(), "pair it again") {
		t.Fatalf("exit=%d\nstderr=%s", code, errb.String())
	}

	if !strings.Contains(out.String(), "WXYZ-1234") || !strings.Contains(errb.String(), `"user_code":"WXYZ-1234"`) {
		t.Fatalf("pairing code not shown on stdout and in logs\nstdout=%s\nstderr=%s", out.String(), errb.String())
	}

	select {
	case h := <-gw.hello:
		if !h.GetFresh() || h.GetVersion() == "" {
			t.Fatalf("unexpected hello %+v", h)
		}

		if !slices.Contains(h.GetCapabilities(), "download.torrent") || slices.Contains(h.GetCapabilities(), "download.usenet") {
			t.Fatalf("capabilities = %v, want torrent only (no usenet provider)", h.GetCapabilities())
		}
	default:
		t.Fatal("gateway never received a verified Hello")
	}

	out.Reset()
	if code := Run(context.Background(), []string{"--data-dir", dir, "status"}, &out, &errb); code != ExitOK {
		t.Fatalf("status exit=%d", code)
	}

	if !strings.Contains(out.String(), "agent-9") || strings.Contains(out.String(), "rt") && strings.Contains(out.String(), "refresh_token") {
		t.Fatalf("status output wrong or leaks secrets:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "1 commands applied, last cmd-e2e") {
		t.Fatalf("status does not show the recorded command:\n%s", out.String())
	}
}
