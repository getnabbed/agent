// Package link owns the agent's single Connect() stream to cloud-gateway:
// handshake, heartbeats, send lanes, dispatch, GoAway handling and
// reconnection with jittered backoff.
//
// One goroutine reads the stream and one writes it. Neither blocks on
// application work: commands go to an ordered worker, queries to a bounded
// pool.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"slices"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/getnabbed/agent/internal/auth"
	"github.com/getnabbed/agent/internal/clock"
	"github.com/getnabbed/agent/internal/identity"
	"github.com/getnabbed/agent/internal/ids"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// maxRecvMsgBytes bounds one ControlMessage on the wire.
const maxRecvMsgBytes = 4 << 20

// ErrRevoked means the cloud revoked this agent. Reconnecting cannot help.
var ErrRevoked = errors.New("agent was revoked by the control plane")

// Tokens supplies access tokens. *auth.TokenSource implements it.
type Tokens interface {
	Token(ctx context.Context) (auth.AccessToken, error)
	Invalidate(raw string)
}

// Signer signs with the agent identity key. *identity.Identity implements it.
type Signer interface {
	Sign(msg []byte) []byte
}

// LocalState reports what the agent already has, for Hello. *store.Store
// implements it.
type LocalState interface {
	HelloState(ctx context.Context) (fresh bool, lastCommandID string, pendingEvents uint32, err error)
}

// Outbox is the durable queue of events awaiting EventAck. *store.Store
// implements it. Each session replays every unacknowledged event from the
// start, in order; the cloud deduplicates by event id.
type Outbox interface {
	// PendingEvents returns up to limit events with seq > afterSeq, oldest
	// first, and the seq of the last one returned.
	PendingEvents(ctx context.Context, afterSeq int64, limit int) (eventIDs []string, events []*pb.Event, lastSeq int64, err error)
	// EventsAdded is signalled when new events are committed.
	EventsAdded() <-chan struct{}
}

// Handler applies inbound work. Methods are called from link-owned
// goroutines; HandleCommand calls are strictly sequential and in order.
type Handler interface {
	HandleCommand(ctx context.Context, cmd *pb.Command) *pb.CommandResult
	HandleQuery(ctx context.Context, q *pb.Query) *pb.QueryResult
	HandleEventAck(ctx context.Context, ack *pb.EventAck)
}

// Config configures a Client.
type Config struct {
	// GatewayURL is https://host[:port] (http only for loopback).
	GatewayURL string
	// TransportCredentials for https gateways. Nil uses system roots.
	TransportCredentials credentials.TransportCredentials
	// DialOptions are appended last; tests use them to inject bufconn.
	DialOptions []grpc.DialOption

	Tokens  Tokens
	Signer  Signer
	State   LocalState
	Handler Handler
	// Outbox is optional; without it no events are sent.
	Outbox Outbox
	Clock  *clock.Clock
	Logger *slog.Logger

	Version string
	OS      string
	// Capabilities lists what the agent can do now. It is read for every
	// Hello, and a change after a command reconnects so the cloud sees it.
	Capabilities func() []string
	// Sources lists the control-plane sources the agent holds, read for every
	// Hello. Setting it adds the "sources.inventory" capability, which tells
	// the cloud the list is complete.
	Sources func(context.Context) ([]*pb.LocalSourceSummary, error)

	MinBackoff   time.Duration // default 1s
	MaxBackoff   time.Duration // default 60s
	HelloTimeout time.Duration // default 15s
	// StableAfter resets backoff once a session has lasted this long.
	StableAfter time.Duration // default 60s
	// QueryConcurrency bounds in-flight queries. Default 4.
	QueryConcurrency int
}

// Client maintains the session.
type Client struct {
	cfg        Config
	conn       *grpc.ClientConn
	rpc        pb.AgentGatewayServiceClient
	instanceID string
	log        *slog.Logger

	// current is the live session, nil between streams.
	current atomic.Pointer[session]

	// sentCaps are the capabilities in the latest Hello. Only the Run
	// goroutine and its session touch them, one after the other.
	sentCaps []string
}

// New validates cfg and creates the underlying gRPC channel. No network I/O
// happens until Run.
func New(cfg Config) (*Client, error) {
	if cfg.Tokens == nil || cfg.Signer == nil || cfg.Handler == nil || cfg.State == nil {
		return nil, errors.New("link: Tokens, Signer, State and Handler are required")
	}

	if cfg.Clock == nil {
		cfg.Clock = clock.New()
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	cfg.MinBackoff = cmp(cfg.MinBackoff, time.Second)
	cfg.MaxBackoff = cmp(cfg.MaxBackoff, 60*time.Second)
	cfg.HelloTimeout = cmp(cfg.HelloTimeout, 15*time.Second)
	cfg.StableAfter = cmp(cfg.StableAfter, 60*time.Second)
	if cfg.QueryConcurrency <= 0 {
		cfg.QueryConcurrency = 4
	}

	target, creds, err := dialTarget(cfg.GatewayURL, cfg.TransportCredentials)
	if err != nil {
		return nil, err
	}

	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithUserAgent("nabbed-agent/" + cfg.Version),
		// Explicit, so a library default change cannot raise it silently.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsgBytes)),
	}, cfg.DialOptions...)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("link: %w", err)
	}

	return &Client{
		cfg:        cfg,
		conn:       conn,
		rpc:        pb.NewAgentGatewayServiceClient(conn),
		instanceID: newID(),
		log:        cfg.Logger.With("component", "link"),
	}, nil
}

// InstanceID is random per process and sent in every Hello.
func (c *Client) InstanceID() string { return c.instanceID }

// Close releases the gRPC channel.
func (c *Client) Close() error { return c.conn.Close() }

func dialTarget(raw string, tc credentials.TransportCredentials) (string, credentials.TransportCredentials, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", nil, fmt.Errorf("link: invalid gateway url %q", raw)
	}

	port := u.Port()
	switch u.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}

		if tc == nil {
			tc = credentials.NewTLS(nil)
		}
	case "http":
		if !auth.IsLoopback(u.Hostname()) {
			return "", nil, errors.New("link: plaintext gateway only allowed on loopback")
		}

		if port == "" {
			port = "80"
		}

		tc = insecure.NewCredentials()
	default:
		return "", nil, fmt.Errorf("link: unsupported gateway scheme %q", u.Scheme)
	}

	return "dns:///" + net.JoinHostPort(u.Hostname(), port), tc, nil
}

// Run keeps a session open until ctx is cancelled, the agent is revoked or
// its credentials are rejected for good. It returns nil on cancellation.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		started := time.Now()
		established, err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}

		if established && time.Since(started) >= c.cfg.StableAfter {
			attempt = 0
		}

		delay := c.backoff(attempt)
		var ga *goAwayError
		switch {
		case errors.Is(err, ErrRevoked), errors.Is(err, auth.ErrReauthRequired):
			return err
		case errors.As(err, &ga):
			if ga.reason == pb.GoAwayReason_GO_AWAY_REASON_REVOKED {
				return ErrRevoked
			}

			// The server chose when we come back; add up to 20% jitter so a
			// drained pod's agents do not reconnect in lockstep.
			if ga.after > 0 {
				delay = ga.after + rand.N(ga.after/5+1)
			} else if ga.reason == pb.GoAwayReason_GO_AWAY_REASON_TOKEN_EXPIRING || ga.reason == pb.GoAwayReason_GO_AWAY_REASON_DRAIN {
				delay = rand.N(c.cfg.MinBackoff + 1)
			}

			attempt = 0
		case errors.Is(err, errStreamAged), errors.Is(err, errCapabilitiesChanged):
			delay = 0
			attempt = 0
		default:
			attempt++
		}

		lvl := slog.LevelWarn
		if ga != nil || errors.Is(err, errStreamAged) || errors.Is(err, errCapabilitiesChanged) {
			lvl = slog.LevelInfo
		}

		c.log.Log(ctx, lvl, "session ended; reconnecting", "err", err, "in", delay.Round(time.Millisecond))
		if err := sleep(ctx, delay); err != nil {
			return nil
		}
	}
}

// backoff returns a delay in [min, cap] where cap doubles per attempt up to
// MaxBackoff ("full jitter" above a floor).
func (c *Client) backoff(attempt int) time.Duration {
	ceil := c.cfg.MinBackoff << min(attempt, 16)
	if ceil > c.cfg.MaxBackoff || ceil <= 0 {
		ceil = c.cfg.MaxBackoff
	}

	return c.cfg.MinBackoff + rand.N(ceil-c.cfg.MinBackoff+1)
}

type goAwayError struct {
	reason pb.GoAwayReason
	after  time.Duration
	msg    string
}

func (e *goAwayError) Error() string {
	return fmt.Sprintf("gateway sent GoAway %s %s", e.reason, e.msg)
}

var (
	errStreamAged          = errors.New("stream reached max age")
	errCapabilitiesChanged = errors.New("capabilities changed; reconnecting to send a new Hello")
	errHeartbeatTimeout    = errors.New("missed 3 heartbeat acks")
)

// session runs one stream. established reports whether Welcome arrived.
func (c *Client) session(parent context.Context) (established bool, err error) {
	tok, err := c.cfg.Tokens.Token(parent)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	sctx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok.Raw)
	stream, err := c.rpc.Connect(sctx)
	if err != nil {
		return false, c.classify(err, tok)
	}

	hello, err := c.hello(ctx, tok)
	if err != nil {
		return false, fmt.Errorf("build hello: %w", err)
	}

	if err := stream.Send(hello); err != nil {
		return false, c.classify(err, tok)
	}

	in := make(chan *pb.ControlMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}

			select {
			case in <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Handshake: Welcome or GoAway, within HelloTimeout.
	var welcome *pb.Welcome
	timer := time.NewTimer(c.cfg.HelloTimeout)
	defer timer.Stop()
	select {
	case m := <-in:
		switch k := m.GetKind().(type) {
		case *pb.ControlMessage_Welcome:
			welcome = k.Welcome
		case *pb.ControlMessage_GoAway:
			return false, c.goAway(k.GoAway, tok)
		default:
			return false, fmt.Errorf("protocol: expected Welcome, got %T", k)
		}
	case err := <-recvErr:
		return false, c.classify(err, tok)
	case <-timer.C:
		return false, errors.New("timed out waiting for Welcome")
	case <-ctx.Done():
		return false, ctx.Err()
	}

	s := newSession(c, stream, welcome)
	c.current.Store(s)
	defer c.current.CompareAndSwap(s, nil)
	c.log.Info("connected", "session_id", welcome.GetSessionId(), "heartbeat", s.heartbeat, "max_age", s.maxAge)

	return true, s.run(ctx, in, recvErr, tok)
}

func (c *Client) hello(ctx context.Context, tok auth.AccessToken) (*pb.AgentMessage, error) {
	fresh, last, pending, err := c.cfg.State.HelloState(ctx)
	if err != nil {
		return nil, err
	}

	var sources []*pb.LocalSourceSummary
	if c.cfg.Sources != nil {
		// An incomplete list would read as sources missing, so a failed read
		// fails the Hello and the session is retried.
		sources, err = c.cfg.Sources(ctx)
		if err != nil {
			return nil, fmt.Errorf("list sources for hello: %w", err)
		}
	}

	c.sentCaps = c.capabilities()
	c.log.Debug("sending hello", "capabilities", c.sentCaps, "fresh", fresh, "last_command_id", last, "sources", len(sources))

	signedAt := c.cfg.Clock.Now().UnixMilli()
	sig := c.cfg.Signer.Sign(identity.HelloSigningInput(tok.JTI, c.instanceID, signedAt))

	// The header carries the signed time too, so a gateway that verifies
	// against header.timestamp_ms rather than signed_at_ms sees the same value.
	h := c.header("")
	h.TimestampMs = signedAt

	return &pb.AgentMessage{
		Header: h,
		Kind: &pb.AgentMessage_Hello{Hello: &pb.Hello{
			Version:           c.cfg.Version,
			Capabilities:      c.sentCaps,
			InstanceId:        c.instanceID,
			Os:                c.cfg.OS,
			Fresh:             fresh,
			LastCommandId:     last,
			PendingEventCount: pending,
			LocalSources:      sources,
			SignedAtMs:        signedAt,
			Signature:         sig,
		}},
	}, nil
}

func (c *Client) header(correlationID string) *pb.Header {
	return &pb.Header{Id: newID(), CorrelationId: correlationID, TimestampMs: c.cfg.Clock.Now().UnixMilli()}
}

// classify maps a stream error to what Run should do, invalidating the token
// when the gateway rejected it.
func (c *Client) classify(err error, tok auth.AccessToken) error {
	switch status.Code(err) {
	case codes.Unauthenticated:
		c.cfg.Tokens.Invalidate(tok.Raw)
	case codes.PermissionDenied:
		return fmt.Errorf("%w: %w", ErrRevoked, err)
	}

	return err
}

func (c *Client) goAway(g *pb.GoAway, tok auth.AccessToken) error {
	if g.GetReason() == pb.GoAwayReason_GO_AWAY_REASON_TOKEN_EXPIRING {
		c.cfg.Tokens.Invalidate(tok.Raw)
	}

	if g.GetReason() == pb.GoAwayReason_GO_AWAY_REASON_SUPERSEDED {
		c.log.Warn("another process connected with this agent's identity; is the data directory shared or copied?")
	}

	return &goAwayError{reason: g.GetReason(), after: time.Duration(g.GetReconnectAfterMs()) * time.Millisecond, msg: g.GetMessage()}
}

func cmp(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}

	return v
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newID() string { return ids.NewV7() }

// SendTelemetry records the latest value for key (for example
// "progress/<download_id>"). Telemetry is lossy: it is dropped when no
// session is live and only the newest value per key is sent.
func (c *Client) SendTelemetry(key string, t *pb.Telemetry) {
	if s := c.current.Load(); s != nil {
		s.telemetry.put(key, &pb.AgentMessage{Header: c.header(""), Kind: &pb.AgentMessage_Telemetry{Telemetry: t}})
	}
}

// CapSourcesInventory says Hello.local_sources is the complete list.
const CapSourcesInventory = "sources.inventory"

func (c *Client) capabilities() []string {
	var caps []string
	if c.cfg.Capabilities != nil {
		caps = c.cfg.Capabilities()
	}

	if c.cfg.Sources != nil {
		caps = append(slices.Clip(caps), CapSourcesInventory)
	}

	return caps
}
