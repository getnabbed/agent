package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/identity"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// fakeTokens hands out tok-N, advancing N on Invalidate.
type fakeTokens struct {
	mu  sync.Mutex
	gen int
}

func (f *fakeTokens) Token(context.Context) (auth.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return auth.AccessToken{Raw: fmt.Sprintf("tok-%d", f.gen), JTI: fmt.Sprintf("jti-%d", f.gen), Expiry: time.Now().Add(time.Hour)}, nil
}

func (f *fakeTokens) Invalidate(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if raw == fmt.Sprintf("tok-%d", f.gen) {
		f.gen++
	}
}

type connInfo struct {
	token string
	hello *pb.Hello
}

// gateway is a scripted AgentGatewayService. script runs per stream after a
// verified Hello; it returns when the stream should end.
type gateway struct {
	pb.UnimplementedAgentGatewayServiceServer
	t      *testing.T
	id     *identity.Identity
	conns  chan connInfo
	script func(n int, srv pb.AgentGatewayService_ConnectServer) error

	mu sync.Mutex
	n  int
}

func (g *gateway) Connect(srv pb.AgentGatewayService_ConnectServer) error {
	md, _ := metadata.FromIncomingContext(srv.Context())
	authz := md.Get("authorization")
	if len(authz) != 1 || !strings.HasPrefix(authz[0], "Bearer ") {
		return status.Error(codes.Unauthenticated, "missing bearer")
	}
	tok := strings.TrimPrefix(authz[0], "Bearer ")
	first, err := srv.Recv()
	if err != nil {
		return err
	}
	h := first.GetHello()
	if h == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	jti := strings.Replace(tok, "tok-", "jti-", 1)
	if !identity.Verify(g.id.SignPublic(), identity.HelloSigningInput(jti, h.GetInstanceId(), h.GetSignedAtMs()), h.GetSignature()) {
		return status.Error(codes.Unauthenticated, "bad hello signature")
	}
	g.mu.Lock()
	g.n++
	n := g.n
	g.mu.Unlock()
	g.conns <- connInfo{token: tok, hello: h}
	return g.script(n, srv)
}

func welcome(hbMs uint32) *pb.ControlMessage {
	return &pb.ControlMessage{Kind: &pb.ControlMessage_Welcome{Welcome: &pb.Welcome{SessionId: "s", HeartbeatMs: hbMs}}}
}

func goAway(r pb.GoAwayReason) *pb.ControlMessage {
	return &pb.ControlMessage{Kind: &pb.ControlMessage_GoAway{GoAway: &pb.GoAway{Reason: r, ReconnectAfterMs: 1}}}
}

// ackHeartbeats echoes acks until ctx ends or stop is closed.
func ackHeartbeats(srv pb.AgentGatewayService_ConnectServer, onOther func(*pb.AgentMessage)) error {
	for {
		m, err := srv.Recv()
		if err != nil {
			return err
		}
		if hb := m.GetHeartbeat(); hb != nil {
			if err := srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_HeartbeatAck{HeartbeatAck: &pb.HeartbeatAck{Seq: hb.GetSeq(), ServerTimeMs: time.Now().UnixMilli()}}}); err != nil {
				return err
			}
			continue
		}
		if onOther != nil {
			onOther(m)
		}
	}
}

func start(t *testing.T, script func(int, pb.AgentGatewayService_ConnectServer) error) (*Client, *gateway, *fakeTokens) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{t: t, id: id, conns: make(chan connInfo, 16), script: script}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterAgentGatewayServiceServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	toks := &fakeTokens{}
	c, err := New(Config{
		GatewayURL:  "http://localhost:1",
		DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })},
		Tokens:      toks, Signer: id, State: StatelessState{}, Handler: UnsupportedHandler{},
		Logger:  slog.New(slog.DiscardHandler),
		Version: "test", OS: "test",
		MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, HelloTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, g, toks
}

func nextConn(t *testing.T, g *gateway) connInfo {
	t.Helper()
	select {
	case ci := <-g.conns:
		return ci
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a connection")
		return connInfo{}
	}
}

func TestHandshakeCommandAndTokenExpiringReconnect(t *testing.T) {
	results := make(chan *pb.CommandResult, 1)
	c, g, _ := start(t, func(n int, srv pb.AgentGatewayService_ConnectServer) error {
		if err := srv.Send(welcome(20)); err != nil {
			return err
		}
		if n > 1 {
			return ackHeartbeats(srv, nil)
		}
		if err := srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Command{Command: &pb.Command{Id: "cmd-1", Kind: &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{RootId: "r"}}}}}); err != nil {
			return err
		}
		for {
			m, err := srv.Recv()
			if err != nil {
				return err
			}
			if r := m.GetCommandResult(); r != nil {
				results <- r
				return srv.Send(goAway(pb.GoAwayReason_GO_AWAY_REASON_TOKEN_EXPIRING))
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	first := nextConn(t, g)
	if !first.hello.GetFresh() || first.hello.GetInstanceId() != c.InstanceID() {
		t.Fatalf("unexpected hello: %+v", first.hello)
	}
	select {
	case r := <-results:
		if r.GetCommandId() != "cmd-1" || r.GetOutcome() != pb.CommandOutcome_COMMAND_OUTCOME_REJECTED || r.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_UNSUPPORTED {
			t.Fatalf("unexpected result: %v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no CommandResult")
	}
	second := nextConn(t, g)
	if second.token == first.token {
		t.Fatal("expected a new token after GoAway{TOKEN_EXPIRING}")
	}
	if second.hello.GetInstanceId() != first.hello.GetInstanceId() {
		t.Fatal("instance id must be stable for the process")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v after cancel", err)
	}
}

func TestRevokedStopsRun(t *testing.T) {
	c, _, _ := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
		_ = srv.Send(welcome(1000))
		return srv.Send(goAway(pb.GoAwayReason_GO_AWAY_REASON_REVOKED))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run err = %v, want ErrRevoked", err)
	}
}

func TestMissedHeartbeatsReconnect(t *testing.T) {
	c, g, _ := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
		_ = srv.Send(welcome(10))
		for { // read but never ack
			if _, err := srv.Recv(); err != nil {
				return err
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	nextConn(t, g)
	nextConn(t, g) // reconnected after missing acks
}

func TestUnauthenticatedInvalidatesToken(t *testing.T) {
	c, g, toks := start(t, func(n int, srv pb.AgentGatewayService_ConnectServer) error {
		if n == 1 {
			return status.Error(codes.Unauthenticated, "token expired")
		}
		_ = srv.Send(welcome(1000))
		return ackHeartbeats(srv, nil)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	a, b := nextConn(t, g), nextConn(t, g)
	if a.token == b.token {
		t.Fatal("token was not invalidated after Unauthenticated")
	}
	toks.mu.Lock()
	defer toks.mu.Unlock()
	if toks.gen != 1 {
		t.Fatalf("gen = %d, want 1", toks.gen)
	}
}

func TestBackoffBounds(t *testing.T) {
	c := &Client{cfg: Config{MinBackoff: time.Second, MaxBackoff: 60 * time.Second}}
	for attempt := range 30 {
		for range 50 {
			d := c.backoff(attempt)
			if d < time.Second || d > 60*time.Second {
				t.Fatalf("attempt %d: backoff %v out of [1s, 60s]", attempt, d)
			}
		}
	}
}

func TestSendEventAndTelemetry(t *testing.T) {
	got := make(chan *pb.AgentMessage, 8)
	c, g, _ := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
		_ = srv.Send(welcome(1000))
		return ackHeartbeats(srv, func(m *pb.AgentMessage) { got <- m })
	})
	if err := c.SendEvent(context.Background(), "e1", &pb.Event{}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("SendEvent before connect = %v, want ErrNotConnected", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	nextConn(t, g)
	var err error
	for range 100 { // session becomes current just after Welcome
		if err = c.SendEvent(ctx, "evt-1", &pb.Event{Kind: &pb.Event_AgentNotice{AgentNotice: &pb.AgentNotice{Code: "x"}}}); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		c.SendTelemetry("progress/d1", &pb.Telemetry{Kind: &pb.Telemetry_DownloadProgress{DownloadProgress: &pb.DownloadProgress{DownloadId: "d1", Percent: float32(i)}}})
	}
	deadline := time.After(5 * time.Second)
	var sawEvent bool
	for {
		select {
		case m := <-got:
			if m.GetEvent() != nil && m.GetHeader().GetId() == "evt-1" {
				sawEvent = true
			}
			if p := m.GetTelemetry().GetDownloadProgress(); p != nil && p.GetPercent() == 2 && sawEvent {
				return
			}
		case <-deadline:
			t.Fatal("did not receive event and latest telemetry value")
		}
	}
}
