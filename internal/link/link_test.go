package link

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/command"
	"github.com/getnabbed/agent/internal/identity"
	"github.com/getnabbed/agent/internal/store"
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

func start(t *testing.T, script func(int, pb.AgentGatewayService_ConnectServer) error) (*Client, *gateway, *fakeTokens, *store.Store) {
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

	st, err := store.Open(context.Background(), t.TempDir(), "agent-1", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	toks := &fakeTokens{}
	c, err := New(Config{
		GatewayURL:  "http://localhost:1",
		DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })},
		Tokens:      toks, Signer: id, State: st, Handler: command.New(command.Config{Store: st, Log: slog.New(slog.DiscardHandler)}), Outbox: st,
		Logger:  slog.New(slog.DiscardHandler),
		Version: "test", OS: "test",
		MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, HelloTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c, g, toks, st
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
	c, g, _, _ := start(t, func(n int, srv pb.AgentGatewayService_ConnectServer) error {
		if err := srv.Send(welcome(20)); err != nil {
			return err
		}

		if n > 1 {
			return ackHeartbeats(srv, nil)
		}

		if err := srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Command{Command: &pb.Command{Id: "cmd-1", Kind: &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{RootId: "r"}}}}}); err != nil {
			return err
		}

		// Ack heartbeats while waiting: at 20 ms the agent drops a stream
		// after three unanswered ones, which a slow runner reaches before the
		// result is sent, and the result is then lost with the stream.
		for {
			m, err := srv.Recv()
			if err != nil {
				return err
			}

			if hb := m.GetHeartbeat(); hb != nil {
				ack := &pb.HeartbeatAck{Seq: hb.GetSeq(), ServerTimeMs: time.Now().UnixMilli()}
				if err := srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_HeartbeatAck{HeartbeatAck: ack}}); err != nil {
					return err
				}

				continue
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
	for _, tc := range []struct {
		name        string
		sendWelcome bool
	}{
		{"after welcome", true},
		{"instead of welcome", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conns atomic.Int32
			c, _, _, _ := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
				conns.Add(1)
				if tc.sendWelcome {
					_ = srv.Send(welcome(1000))
				}

				return srv.Send(goAway(pb.GoAwayReason_GO_AWAY_REASON_REVOKED))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Run(ctx); !errors.Is(err, ErrRevoked) {
				t.Fatalf("Run err = %v, want ErrRevoked", err)
			}

			// No reconnect: a revoked agent must not hammer the gateway.
			if n := conns.Load(); n != 1 {
				t.Fatalf("got %d connections, want 1", n)
			}
		})
	}
}

// closedStream is a stream the gateway has closed: every Send reports
// io.EOF, as gRPC's does.
type closedStream struct {
	pb.AgentGatewayService_ConnectClient
}

func (closedStream) Send(*pb.AgentMessage) error { return io.EOF }

func TestRevokeIsSeenWhenASendFailsFirst(t *testing.T) {
	c, _, _, _ := start(t, func(int, pb.AgentGatewayService_ConnectServer) error { return nil })

	s := newSession(c, closedStream{}, &pb.Welcome{HeartbeatMs: 60_000})
	// Something for the writer to send at once, so its io.EOF comes first.
	s.control <- &pb.AgentMessage{Kind: &pb.AgentMessage_Heartbeat{Heartbeat: &pb.Heartbeat{Seq: 1}}}

	in := make(chan *pb.ControlMessage)
	recvErr := make(chan error, 1)

	go func() {
		time.Sleep(50 * time.Millisecond)
		in <- goAway(pb.GoAwayReason_GO_AWAY_REASON_REVOKED)
		recvErr <- io.EOF
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var ga *goAwayError
	if err := s.run(ctx, in, recvErr, auth.AccessToken{}); !errors.As(err, &ga) || ga.reason != pb.GoAwayReason_GO_AWAY_REASON_REVOKED {
		t.Fatalf("got %v, want the revoking GoAway read after the failed send", err)
	}
}

func TestMissedHeartbeatsReconnect(t *testing.T) {
	c, g, _, _ := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
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
	c, g, toks, _ := start(t, func(n int, srv pb.AgentGatewayService_ConnectServer) error {
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

func notice(code string) *pb.Event {
	return &pb.Event{Kind: &pb.Event_AgentNotice{AgentNotice: &pb.AgentNotice{Code: code}}}
}

func enqueue(t *testing.T, st *store.Store, codes ...string) {
	t.Helper()

	err := st.Update(context.Background(), func(tx *store.Tx) error {
		for _, code := range codes {
			if _, err := tx.Enqueue(context.Background(), notice(code)); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Events queued while offline are sent in order on connect, events added
// during the session follow, and acked events leave the outbox.
func TestOutboxDeliveredInOrderAndDeletedOnAck(t *testing.T) {
	got := make(chan string, 16)

	c, g, _, st := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
		_ = srv.Send(welcome(1000))

		return ackHeartbeats(srv, func(m *pb.AgentMessage) {
			if ev := m.GetEvent(); ev != nil {
				got <- ev.GetAgentNotice().GetCode()

				_ = srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_EventAck{EventAck: &pb.EventAck{Ids: []string{m.GetHeader().GetId()}}}})
			}
		})
	})

	enqueue(t, st, "one", "two")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	first := nextConn(t, g)
	if first.hello.GetPendingEventCount() != 2 {
		t.Fatalf("hello pending_event_count = %d, want 2", first.hello.GetPendingEventCount())
	}

	enqueue(t, st, "three")

	for _, want := range []string{"one", "two", "three"} {
		select {
		case code := <-got:
			if code != want {
				t.Fatalf("got event %q, want %q", code, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %q", want)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, pending, err := st.HelloState(context.Background())
		if err == nil && pending == 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("outbox not drained after acks: pending=%d err=%v", pending, err)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// Events sent but not acked before the stream dies are resent next session.
func TestUnackedEventsReplayedAfterReconnect(t *testing.T) {
	type seen struct {
		conn int
		id   string
	}

	got := make(chan seen, 16)

	c, g, _, st := start(t, func(n int, srv pb.AgentGatewayService_ConnectServer) error {
		_ = srv.Send(welcome(1000))

		for {
			m, err := srv.Recv()
			if err != nil {
				return err
			}

			if m.GetEvent() == nil {
				continue
			}

			got <- seen{conn: n, id: m.GetHeader().GetId()}

			if n == 1 {
				return srv.Send(goAway(pb.GoAwayReason_GO_AWAY_REASON_DRAIN)) // drop without acking
			}
		}
	})

	enqueue(t, st, "only")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	nextConn(t, g)
	a := <-got

	nextConn(t, g)

	select {
	case b := <-got:
		if b.conn != 2 || b.id != a.id {
			t.Fatalf("replay = %+v, want same id %s on conn 2", b, a.id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unacked event was not replayed after reconnect")
	}
}

func TestTelemetryLatestWins(t *testing.T) {
	l := newLatest()
	for i := range 3 {
		l.put("progress/d1", &pb.AgentMessage{Kind: &pb.AgentMessage_Telemetry{Telemetry: &pb.Telemetry{Kind: &pb.Telemetry_DownloadProgress{DownloadProgress: &pb.DownloadProgress{Percent: float32(i)}}}}})
	}

	out := l.drain()
	if len(out) != 1 || out[0].GetTelemetry().GetDownloadProgress().GetPercent() != 2 {
		t.Fatalf("drain = %v, want only the latest value", out)
	}
}

// L3: an already-expired command must come back as a recorded
// DEADLINE_EXCEEDED, not an unrecorded internal failure.
func TestExpiredCommandIsRecordedAsDeadlineExceeded(t *testing.T) {
	results := make(chan *pb.CommandResult, 1)

	c, g, _, st := start(t, func(_ int, srv pb.AgentGatewayService_ConnectServer) error {
		_ = srv.Send(welcome(1000))
		_ = srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Command{Command: &pb.Command{
			Id: "late", DeadlineMs: time.Now().Add(-time.Minute).UnixMilli(),
			Kind: &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{}},
		}}})

		return ackHeartbeats(srv, func(m *pb.AgentMessage) {
			if r := m.GetCommandResult(); r != nil {
				results <- r
			}
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	nextConn(t, g)

	select {
	case r := <-results:
		if r.GetErrorCode() != pb.CommandErrorCode_COMMAND_ERROR_CODE_DEADLINE_EXCEEDED {
			t.Fatalf("result = %v, want DEADLINE_EXCEEDED", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}

	if _, last, _, _ := st.HelloState(context.Background()); last != "late" {
		t.Fatalf("last command = %q, want the expired command recorded", last)
	}
}

func TestImplausibleServerTimeIsIgnored(t *testing.T) {
	c, _, _, _ := start(t, func(int, pb.AgentGatewayService_ConnectServer) error { return nil })
	s := &session{c: c}

	s.observeClock(time.Now().Add(48 * time.Hour))
	if off := c.cfg.Clock.Offset(); off != 0 {
		t.Fatalf("offset = %v after an implausible server time, want 0", off)
	}

	s.observeClock(time.Now().Add(90 * time.Second))
	if off := c.cfg.Clock.Offset(); off < 80*time.Second {
		t.Fatalf("offset = %v, want a plausible correction applied", off)
	}
}

func TestCapabilityChangeSendsNewHelloAfterResult(t *testing.T) {
	var usenet atomic.Bool

	results := make(chan *pb.CommandResult, 1)
	c, g, _, _ := start(t, func(n int, srv pb.AgentGatewayService_ConnectServer) error {
		if err := srv.Send(welcome(20)); err != nil {
			return err
		}

		if n > 1 {
			return ackHeartbeats(srv, nil)
		}

		// Stands in for PutSource: the command makes usenet available.
		usenet.Store(true)

		if err := srv.Send(&pb.ControlMessage{Kind: &pb.ControlMessage_Command{Command: &pb.Command{Id: "cmd-src", Kind: &pb.Command_RescanLibrary{RescanLibrary: &pb.RescanLibrary{RootId: "r"}}}}}); err != nil {
			return err
		}

		return ackHeartbeats(srv, func(m *pb.AgentMessage) {
			if r := m.GetCommandResult(); r != nil {
				results <- r
			}
		})
	})

	c.cfg.Capabilities = func() []string {
		if usenet.Load() {
			return []string{"download.torrent", "download.usenet"}
		}

		return []string{"download.torrent"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	if first := nextConn(t, g); !slices.Equal(first.hello.GetCapabilities(), []string{"download.torrent"}) {
		t.Fatalf("got first Hello capabilities %v, want torrent only", first.hello.GetCapabilities())
	}

	select {
	case r := <-results:
		if r.GetCommandId() != "cmd-src" {
			t.Fatalf("got result %v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("got no CommandResult before the reconnect")
	}

	second := nextConn(t, g)
	if !slices.Contains(second.hello.GetCapabilities(), "download.usenet") {
		t.Fatalf("got second Hello capabilities %v, want usenet included", second.hello.GetCapabilities())
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run returned %v after cancel", err)
	}
}
