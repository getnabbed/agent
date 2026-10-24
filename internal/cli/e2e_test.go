package cli

import (
	"bytes"
	"context"
	"crypto/ecdh"
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
	"github.com/getnabbed/agent/internal/source"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

type e2eGateway struct {
	pb.UnimplementedAgentGatewayServiceServer
	mu     sync.Mutex
	pub    ed25519.PublicKey
	seal   *ecdh.PublicKey
	conns  int
	hello  chan *pb.Hello
	source chan *pb.CommandResult
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

	g.mu.Lock()
	g.conns++
	n, seal := g.conns, g.seal
	g.mu.Unlock()

	g.hello <- h
	_ = srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Welcome{Welcome: &pb.Welcome{SessionId: "s1", HeartbeatMs: 1000}}})

	// First stream: configure a usenet source the way the web app would,
	// sealed to the key from pairing. The agent should answer, then
	// reconnect with a Hello that includes usenet.
	if n == 1 {
		return g.putSource(srv, seal)
	}

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
	gw := &e2eGateway{hello: make(chan *pb.Hello, 2), source: make(chan *pb.CommandResult, 1)}
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
			PublicKeys struct{ Ed25519, X25519 string } `json:"public_keys"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)
		pub, _ := base64.RawURLEncoding.DecodeString(req.PublicKeys.Ed25519)
		x, _ := base64.RawURLEncoding.DecodeString(req.PublicKeys.X25519)
		seal, _ := ecdh.X25519().NewPublicKey(x)
		gw.mu.Lock()
		gw.pub, gw.seal = pub, seal
		gw.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "WXYZ-1234", "verification_uri": "http://" + r.Host + "/device", "interval": 1})
	})
	mux.HandleFunc("POST /api/device/token", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))

			return
		}

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

	if len(gw.hello) != 2 {
		t.Fatalf("got %d verified Hellos, want 2 (before and after PutSource)", len(gw.hello))
	}

	first, second := <-gw.hello, <-gw.hello
	if !first.GetFresh() || first.GetVersion() == "" {
		t.Fatalf("unexpected hello %+v", first)
	}

	if !slices.Contains(first.GetCapabilities(), "download.torrent") || slices.Contains(first.GetCapabilities(), "download.usenet") {
		t.Fatalf("got first capabilities %v, want torrent only (no usenet provider)", first.GetCapabilities())
	}

	if r := <-gw.source; r.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_APPLIED {
		t.Fatalf("got PutSource result %v, want APPLIED", r)
	}

	if !slices.Contains(second.GetCapabilities(), "download.usenet") || second.GetLastCommandId() != "cmd-src" {
		t.Fatalf("got second Hello capabilities %v, last command %q; want usenet and cmd-src", second.GetCapabilities(), second.GetLastCommandId())
	}

	if strings.Contains(errb.String(), `"password"`) || strings.Contains(errb.String(), `:"p"`) {
		t.Fatalf("logs may contain the source password:\n%s", errb.String())
	}

	out.Reset()
	if code := Run(context.Background(), []string{"--data-dir", dir, "status"}, &out, &errb); code != ExitOK {
		t.Fatalf("status exit=%d", code)
	}

	if !strings.Contains(out.String(), "agent-9") || strings.Contains(out.String(), "rt") && strings.Contains(out.String(), "refresh_token") {
		t.Fatalf("status output wrong or leaks secrets:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "2 commands applied, last cmd-e2e") {
		t.Fatalf("status does not show the recorded command:\n%s", out.String())
	}
}

func (g *e2eGateway) putSource(srv pb.AgentGatewayService_ConnectServer, seal *ecdh.PublicKey) error {
	cfg := `{"v":1,"host":"news.newshosting.com","tls":true,"username":"u","password":"p","connections":20}`

	sealed, err := source.Seal(seal, []byte(cfg))
	if err != nil {
		return err
	}

	src := &pb.Source{Id: "src-nh", Kind: source.KindNNTP, DisplayName: "Newshosting", SealedConfig: sealed, SealedToKey: seal.Bytes(), Enabled: true}
	cmd := &pb.Command{Id: "cmd-src", Kind: &pb.Command_PutSource{PutSource: &pb.PutSource{Source: src}}}

	if err := srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Command{Command: cmd}}); err != nil {
		return err
	}

	for {
		m, err := srv.Recv()
		if err != nil {
			return err
		}

		if r := m.GetCommandResult(); r != nil {
			g.source <- r

			// Keep reading until the agent ends the stream to send a new Hello.
			for {
				if _, err := srv.Recv(); err != nil {
					return nil
				}
			}
		}
	}
}
