package source

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	// checkTimeout bounds the connection check made when a source is
	// applied. Dialling alone may take up to the engine's 60s timeout.
	checkTimeout = 90 * time.Second
	// testTimeout bounds a TestSource query, which a user is waiting on.
	testTimeout = 30 * time.Second
	// A failing source is checked again after recheckBase, doubling up to
	// recheckMax, so its status recovers without a download needing it. The
	// cap keeps failed logins rare enough not to get an account locked.
	recheckBase = time.Minute
	recheckMax  = time.Hour
	recheckTick = 15 * time.Second
)

// Health reporting: the engine calls onHealth when a provider's health
// changes, and Run turns each change into a SourceHealth event in the
// outbox, so the cloud sees it even if the stream is down at the time.
// Only changes are sent; a provider failing over and over sends one event.

// onHealth is the engine's hook. It must not block.
func (m *Manager) onHealth(id string) {
	m.mu.Lock()
	m.changed[id] = true
	m.mu.Unlock()

	m.signal()
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run reports health changes and runs the check queued for each applied
// source, until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	tick := time.NewTicker(m.recheck.tick)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.wake:
		case <-tick.C:
		}

		m.mu.Lock()
		m.dueRechecks(time.Now())
		checks, changed := m.toCheck, m.changed
		m.toCheck, m.changed = map[string]bool{}, map[string]bool{}
		m.mu.Unlock()

		for id := range checks {
			wg.Go(func() {
				cctx, cancel := context.WithTimeout(ctx, checkTimeout)
				defer cancel()

				if err := m.engine.Check(cctx, id); err != nil && ctx.Err() == nil {
					m.log.Info("usenet source check failed", "source_id", id, "err", err)
				}
			})
		}

		for id := range changed {
			m.report(ctx, id)
		}
	}
}

// report sends id's health if it differs from what was last sent.
func (m *Manager) report(ctx context.Context, id string) {
	h, ok := m.engine.Health(id)
	if !ok || h.State == usenet.HealthUnknown {
		return
	}

	m.mu.Lock()
	_, ours := m.applied[id] // not a provider configured on the agent itself
	last, sent := m.reported[id]

	if ours {
		m.schedule(id, h, time.Now())
	}
	m.mu.Unlock()

	if !ours || (sent && last == h) {
		return
	}

	ev := &pb.Event{Kind: &pb.Event_SourceHealth{SourceHealth: &pb.SourceHealth{
		SourceId: id, Healthy: h.State == usenet.HealthOK, Detail: h.Detail,
	}}}

	err := m.store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Enqueue(ctx, ev)
		return err
	})
	if err != nil {
		m.log.Warn("cannot queue source health", "source_id", id, "err", err)
		return
	}

	m.mu.Lock()
	m.reported[id] = h
	m.mu.Unlock()

	m.log.Info("usenet source health", "source_id", id, "healthy", h.State == usenet.HealthOK, "detail", h.Detail)
}

// queueCheck asks Run to check id. Call with mu held.
func (m *Manager) queueCheck(id string) {
	m.toCheck[id] = true
	delete(m.reported, id) // a new configuration starts a new history
	delete(m.recheck.due, id)

	m.signal()
}

// ErrUnknownSource means the agent holds no source with that id.
var ErrUnknownSource = errors.New("unknown source")

// ErrDisabled means the source is stored but disabled.
var ErrDisabled = errors.New("source is disabled")

// Test connects to source id now, for TestSource. It returns nil when the
// provider accepted a connection and login, or an error whose message is
// safe to show users.
func (m *Manager) Test(ctx context.Context, id string) error {
	m.mu.Lock()
	_, live := m.applied[id]
	unusable := m.unusable[id]
	m.mu.Unlock()

	if !live {
		var enabled bool

		err := m.store.DB().QueryRowContext(ctx, "SELECT enabled FROM sources WHERE id = ?", id).Scan(&enabled)

		switch {
		case err != nil:
			return ErrUnknownSource
		case unusable:
			return ErrUnsealable
		case !enabled:
			return ErrDisabled
		}

		return ErrUnknownSource
	}

	tctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	err := m.engine.Check(tctx, id)
	if err == nil {
		return nil
	}

	if tctx.Err() != nil && ctx.Err() == nil {
		return errors.New("timed out testing the connection")
	}

	return errors.New(usenet.Describe(err))
}

// recheck holds the schedule for checking failing sources again.
type recheck struct {
	base, max, tick time.Duration
	due             map[string]*retry
}

type retry struct {
	next  time.Time
	delay time.Duration
}

// schedule starts or stops rechecking id after a health observation. Call
// with mu held.
func (m *Manager) schedule(id string, h usenet.Health, now time.Time) {
	if h.State != usenet.HealthFailing {
		delete(m.recheck.due, id)
		return
	}

	if _, ok := m.recheck.due[id]; !ok {
		m.recheck.due[id] = &retry{next: now.Add(m.recheck.base), delay: m.recheck.base}
	}
}

// dueRechecks queues a check for each failing source whose time has come
// and pushes its next one back. Call with mu held.
func (m *Manager) dueRechecks(now time.Time) {
	for id, r := range m.recheck.due {
		if _, live := m.applied[id]; !live {
			delete(m.recheck.due, id)
			continue
		}

		if now.Before(r.next) {
			continue
		}

		m.toCheck[id] = true
		r.delay = min(2*r.delay, m.recheck.max)
		r.next = now.Add(r.delay)
	}
}
