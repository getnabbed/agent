package link

import (
	"context"
	"sync"
	"time"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// maxInFlightEvents bounds events sent but not yet acknowledged (0001 §6).
const maxInFlightEvents = 256

// window tracks events sent on this session and awaiting EventAck.
type window struct {
	mu       sync.Mutex
	inflight map[string]struct{}
	freed    chan struct{}
}

func newWindow() *window {
	return &window{inflight: map[string]struct{}{}, freed: make(chan struct{}, 1)}
}

func (w *window) room() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return maxInFlightEvents - len(w.inflight)
}

func (w *window) add(id string) {
	w.mu.Lock()
	w.inflight[id] = struct{}{}
	w.mu.Unlock()
}

// ack releases acknowledged ids and wakes the pump if any were in flight.
func (w *window) ack(eventIDs []string) {
	w.mu.Lock()

	released := 0
	for _, id := range eventIDs {
		if _, ok := w.inflight[id]; ok {
			delete(w.inflight, id)
			released++
		}
	}

	w.mu.Unlock()

	if released > 0 {
		select {
		case w.freed <- struct{}{}:
		default:
		}
	}
}

// pump streams the outbox to the gateway: every unacknowledged event, in
// seq order, starting from the beginning on each session, with at most
// maxInFlightEvents awaiting acks. It sleeps until events are added or acks
// free the window.
func (s *session) pump(ctx context.Context) {
	ob := s.c.cfg.Outbox

	var cursor int64

	for {
		if room := s.window.room(); room > 0 {
			eventIDs, events, last, err := ob.PendingEvents(ctx, cursor, room)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				s.c.log.Error("read outbox", "err", err)

				if sleep(ctx, time.Second) != nil {
					return
				}

				continue
			}

			for i, ev := range events {
				h := s.c.header("")
				h.Id = eventIDs[i]

				s.window.add(eventIDs[i])

				select {
				case s.events <- &pb.AgentMessage{Header: h, Kind: &pb.AgentMessage_Event{Event: ev}}:
				case <-ctx.Done():
					return
				}
			}

			cursor = last

			// A full page may mean more is waiting.
			if len(events) == room {
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ob.EventsAdded():
		case <-s.window.freed:
		}
	}
}
