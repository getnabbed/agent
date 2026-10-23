package link

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/getnabbed/agent/internal/auth"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	defaultHeartbeat = 15 * time.Second
	missedAckLimit   = 3
	commandQueue     = 64
	eventQueue       = 256
	queryTimeout     = 15 * time.Second
)

// session is one live stream after Welcome.
type session struct {
	c         *Client
	stream    pb.AgentGatewayService_ConnectClient
	heartbeat time.Duration
	maxAge    time.Duration

	// Send lanes, drained by a single writer in priority order:
	// control (results, heartbeats) > events > telemetry.
	control   chan *pb.AgentMessage
	events    chan *pb.AgentMessage
	telemetry *latest

	commands chan *pb.Command
	queries  chan struct{} // semaphore
}

func newSession(c *Client, stream pb.AgentGatewayService_ConnectClient, w *pb.Welcome) *session {
	hb := time.Duration(w.GetHeartbeatMs()) * time.Millisecond
	if hb <= 0 {
		hb = defaultHeartbeat
	}

	return &session{
		c:         c,
		stream:    stream,
		heartbeat: hb,
		maxAge:    time.Duration(w.GetMaxStreamAgeMs()) * time.Millisecond,
		control:   make(chan *pb.AgentMessage, 64),
		events:    make(chan *pb.AgentMessage, eventQueue),
		telemetry: newLatest(),
		commands:  make(chan *pb.Command, commandQueue),
		queries:   make(chan struct{}, c.cfg.QueryConcurrency),
	}
}

func (s *session) run(ctx context.Context, in <-chan *pb.ControlMessage, recvErr <-chan error, tok auth.AccessToken) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Cancel first so the writer, worker and queries stop, then wait.
	defer func() {
		cancel()
		wg.Wait()
	}()

	writeErr := make(chan error, 1)
	wg.Add(2)
	go func() { defer wg.Done(); writeErr <- s.writer(ctx) }()
	go func() { defer wg.Done(); s.commandWorker(ctx) }()

	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	var age <-chan time.Time
	if s.maxAge > 0 {
		t := time.NewTimer(s.maxAge)
		defer t.Stop()
		age = t.C
	}

	var seq, acked uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			return s.c.classify(err, tok)
		case err := <-writeErr:
			return err
		case <-age:
			return errStreamAged
		case <-ticker.C:
			if seq-acked >= missedAckLimit {
				return errHeartbeatTimeout
			}

			seq++
			s.sendControl(ctx, &pb.AgentMessage{Header: s.c.header(""), Kind: &pb.AgentMessage_Heartbeat{Heartbeat: &pb.Heartbeat{Seq: seq}}})
		case m := <-in:
			switch k := m.GetKind().(type) {
			case *pb.ControlMessage_HeartbeatAck:
				if k.HeartbeatAck.GetSeq() > acked {
					acked = k.HeartbeatAck.GetSeq()
				}

				if st := k.HeartbeatAck.GetServerTimeMs(); st > 0 {
					now := s.c.cfg.Clock.Local()
					if off := s.c.cfg.Clock.Observe(time.UnixMilli(st), now); off > 2*time.Minute || off < -2*time.Minute {
						s.c.log.Warn("host clock differs from the control plane", "offset", off.Round(time.Second))
					}
				}
			case *pb.ControlMessage_GoAway:
				return s.c.goAway(k.GoAway, tok)
			case *pb.ControlMessage_Command:
				select {
				case s.commands <- k.Command:
				case <-ctx.Done():
					return ctx.Err()
				}
			case *pb.ControlMessage_Query:
				s.startQuery(ctx, m.GetHeader().GetId(), k.Query, &wg)
			case *pb.ControlMessage_EventAck:
				s.c.cfg.Handler.HandleEventAck(ctx, k.EventAck)
			case *pb.ControlMessage_Welcome:
				return errors.New("protocol: unexpected second Welcome")
			default:
				s.c.log.Debug("ignoring unknown control message", "type", k)
			}
		}
	}
}

// commandWorker applies commands strictly in arrival order. The gateway acks
// the durable log entry only once the CommandResult arrives, so a command
// lost with the stream is redelivered on the next session.
func (s *session) commandWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-s.commands:
			cctx, cancel := ctx, context.CancelFunc(func() {})
			if d := cmd.GetDeadlineMs(); d > 0 {
				cctx, cancel = context.WithDeadline(ctx, time.UnixMilli(d))
			}

			res := s.c.cfg.Handler.HandleCommand(cctx, cmd)
			cancel()

			if res == nil {
				res = &pb.CommandResult{Outcome: pb.CommandOutcome_COMMAND_OUTCOME_FAILED, ErrorCode: pb.CommandErrorCode_COMMAND_ERROR_CODE_INTERNAL}
			}

			res.CommandId = cmd.GetId()
			s.sendControl(ctx, &pb.AgentMessage{Header: s.c.header(cmd.GetId()), Kind: &pb.AgentMessage_CommandResult{CommandResult: res}})
		}
	}
}

func (s *session) startQuery(ctx context.Context, msgID string, q *pb.Query, wg *sync.WaitGroup) {
	select {
	case s.queries <- struct{}{}:
	default:
		s.sendControl(ctx, queryResult(s.c, msgID, &pb.QueryResult{QueryId: q.GetId(), Error: "agent busy"}))
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { <-s.queries }()
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		res := s.c.cfg.Handler.HandleQuery(qctx, q)
		if res == nil {
			res = &pb.QueryResult{Error: "no result"}
		}

		res.QueryId = q.GetId()
		s.sendControl(ctx, queryResult(s.c, msgID, res))
	}()
}

func queryResult(c *Client, corr string, r *pb.QueryResult) *pb.AgentMessage {
	return &pb.AgentMessage{Header: c.header(corr), Kind: &pb.AgentMessage_QueryResult{QueryResult: r}}
}

func (s *session) sendControl(ctx context.Context, m *pb.AgentMessage) {
	select {
	case s.control <- m:
	case <-ctx.Done():
	}
}

// writer is the only goroutine that calls stream.Send.
func (s *session) writer(ctx context.Context) error {
	send := func(m *pb.AgentMessage) error { return s.stream.Send(m) }
	for {
		// Control traffic always goes first.
		select {
		case m := <-s.control:
			if err := send(m); err != nil {
				return err
			}

			continue
		default:
		}

		select {
		case <-ctx.Done():
			return nil
		case m := <-s.control:
			if err := send(m); err != nil {
				return err
			}
		case m := <-s.events:
			if err := send(m); err != nil {
				return err
			}
		case <-s.telemetry.ready:
			for _, m := range s.telemetry.drain() {
				if err := send(m); err != nil {
					return err
				}
			}
		}
	}
}

// latest is a lossy lane: only the newest message per key is kept.
type latest struct {
	mu    sync.Mutex
	items map[string]*pb.AgentMessage
	order []string
	ready chan struct{}
}

func newLatest() *latest {
	return &latest{items: map[string]*pb.AgentMessage{}, ready: make(chan struct{}, 1)}
}

func (l *latest) put(key string, m *pb.AgentMessage) {
	l.mu.Lock()
	if _, ok := l.items[key]; !ok {
		l.order = append(l.order, key)
	}

	l.items[key] = m
	l.mu.Unlock()
	select {
	case l.ready <- struct{}{}:
	default:
	}
}

func (l *latest) drain() []*pb.AgentMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*pb.AgentMessage, 0, len(l.order))
	for _, k := range l.order {
		out = append(out, l.items[k])
	}

	l.items = map[string]*pb.AgentMessage{}
	l.order = l.order[:0]

	return out
}
