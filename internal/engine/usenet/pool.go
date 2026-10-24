package usenet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/getnabbed/agent/internal/netguard"
)

const (
	// idleTimeout closes connections nobody has used for this long. Providers
	// count idle connections against the account limit, so holding them
	// would starve the user's other clients.
	idleTimeout = 60 * time.Second
	// maxDialFailures consecutive failed dials mark a provider down for
	// downCooldown, so downloads fail fast instead of each retrying alone.
	maxDialFailures = 5
	downCooldown    = time.Minute
	maxDialBackoff  = 30 * time.Second
)

var (
	// ErrProviderDown means the provider could not be reached after
	// repeated attempts.
	ErrProviderDown = errors.New("nntp: provider unreachable")
	// errCapacity means the provider refused a connection because the
	// account has too many open. The pool lowers its limit and waits.
	errCapacity   = errors.New("nntp: provider connection limit reached")
	errPoolClosed = errors.New("nntp: pool closed")
)

// pool shares one provider's connections between every download, so the
// provider's connection limit applies to the agent as a whole.
type pool struct {
	p       Provider
	timeout time.Duration
	log     *slog.Logger
	dial    func(context.Context) (*conn, error)
	backoff func(failures int) time.Duration

	mu       sync.Mutex
	idle     []idleConn
	open     int // in use, idle or being dialled
	limit    int
	failures int       // consecutive failed dials
	retryAt  time.Time // no dials before this
	fatalErr error     // sticky: bad credentials or a blocked address do not fix themselves
	learned  bool      // limit was lowered from the configured value
	closed   bool
	changed  chan struct{}
}

type idleConn struct {
	c    *conn
	used time.Time
}

func newPool(p Provider, timeout time.Duration, log *slog.Logger) *pool {
	pl := &pool{
		p: p, timeout: timeout, log: log.With("provider", p.Host),
		limit:   max(p.Connections, 1),
		backoff: dialBackoff,
		changed: make(chan struct{}),
	}
	pl.dial = func(ctx context.Context) (*conn, error) { return dial(ctx, p, timeout) }

	return pl
}

// get returns a connection, reusing an idle one or dialling a new one when
// under the limit. It blocks while the pool is full.
func (pl *pool) get(ctx context.Context) (*conn, error) {
	for {
		pl.mu.Lock()

		c, wait, err := pl.tryGet()
		if c != nil || err != nil {
			pl.mu.Unlock()
			return c, err
		}

		if wait == nil {
			// A dial slot is ours.
			pl.mu.Unlock()

			c, err := pl.dialSlot(ctx)
			if c != nil || err != nil {
				return c, err
			}

			continue
		}

		// Waiting out a dial back-off needs a timer: nothing signals its end.
		delay := time.Until(pl.retryAt)
		if pl.open >= pl.limit {
			delay = 0
		}

		pl.mu.Unlock()

		if err := waitFor(ctx, wait, delay); err != nil {
			return nil, err
		}
	}
}

// tryGet must be called with mu held. It returns a connection, an error,
// a channel to wait on, or none of these when the caller may dial.
func (pl *pool) tryGet() (*conn, <-chan struct{}, error) {
	switch {
	case pl.closed:
		return nil, nil, errPoolClosed
	case pl.fatalErr != nil:
		return nil, nil, pl.fatalErr
	}

	now := time.Now()

	if pl.failures >= maxDialFailures {
		if now.Before(pl.retryAt) {
			return nil, nil, fmt.Errorf("%w: %s", ErrProviderDown, pl.p.Host)
		}

		// The cooldown is over: let one dial probe the provider.
		pl.failures = maxDialFailures - 1
	}

	for len(pl.idle) > 0 {
		ic := pl.idle[len(pl.idle)-1]
		pl.idle = pl.idle[:len(pl.idle)-1]

		if now.Sub(ic.used) > idleTimeout {
			pl.open--
			go func() { _ = ic.c.close() }()

			continue
		}

		return ic.c, nil, nil
	}

	if pl.open < pl.limit && !now.Before(pl.retryAt) {
		pl.open++
		return nil, nil, nil
	}

	return nil, pl.changed, nil
}

// dialSlot dials for a slot already counted in open. A nil connection and
// nil error mean the caller should try again.
func (pl *pool) dialSlot(ctx context.Context) (*conn, error) {
	c, err := pl.dial(ctx)

	pl.mu.Lock()
	defer pl.mu.Unlock()
	defer pl.signal()

	if err == nil {
		pl.failures = 0
		return c, nil
	}

	pl.open--

	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, ErrAuth), errors.Is(err, netguard.ErrBlocked):
		pl.fatalErr = err
		pl.log.Warn("usenet provider unusable", "err", err)

		return nil, err
	case errors.Is(err, errCapacity) && pl.open > 0:
		// The account allows fewer connections than configured, or another
		// client shares it. Keep what works.
		if !pl.learned || pl.open < pl.limit {
			pl.log.Warn("usenet provider allows fewer connections than configured; using fewer",
				"configured", pl.p.Connections, "using", pl.open)
		}

		pl.limit, pl.learned = pl.open, true
		pl.retryAt = time.Now().Add(time.Second)

		return nil, nil
	}

	pl.failures++
	pl.retryAt = time.Now().Add(pl.backoff(pl.failures))

	if pl.failures >= maxDialFailures {
		pl.retryAt = time.Now().Add(downCooldown)
		pl.log.Warn("usenet provider unreachable", "err", err)

		return nil, fmt.Errorf("%w: %s: %w", ErrProviderDown, pl.p.Host, err)
	}

	pl.log.Debug("usenet dial failed; retrying", "err", err, "failures", pl.failures)

	return nil, nil
}

func dialBackoff(failures int) time.Duration {
	return min(time.Duration(1<<min(failures-1, 5))*time.Second, maxDialBackoff)
}

// put returns a connection. Unhealthy ones, whose protocol state is
// unknown, are closed.
func (pl *pool) put(c *conn, healthy bool) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	defer pl.signal()

	if !healthy || pl.closed || pl.open > pl.limit {
		pl.open--
		go func() { _ = c.close() }()

		return
	}

	pl.idle = append(pl.idle, idleConn{c: c, used: time.Now()})
}

// down reports whether the provider is unusable right now. A closed pool
// counts: its provider was replaced or removed, and the download should be
// retried with the new configuration rather than lose its articles.
func (pl *pool) down() bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	return pl.closed || pl.fatalErr != nil || (pl.failures >= maxDialFailures && time.Now().Before(pl.retryAt))
}

// closeIdle closes connections idle for longer than idleTimeout.
func (pl *pool) closeIdle() {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	now := time.Now()
	kept := pl.idle[:0]

	for _, ic := range pl.idle {
		if now.Sub(ic.used) > idleTimeout {
			pl.open--
			go func() { _ = ic.c.close() }()

			continue
		}

		kept = append(kept, ic)
	}

	pl.idle = kept
}

// close closes idle connections and makes in-use ones close on return.
func (pl *pool) close() {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	defer pl.signal()

	pl.closed = true

	for _, ic := range pl.idle {
		pl.open--
		go func() { _ = ic.c.close() }()
	}

	pl.idle = nil
}

// signal wakes waiters. Call with mu held.
func (pl *pool) signal() {
	close(pl.changed)
	pl.changed = make(chan struct{})
}

// waitFor blocks until wake closes, delay passes (when positive) or ctx
// ends.
func waitFor(ctx context.Context, wake <-chan struct{}, delay time.Duration) error {
	var timer <-chan time.Time
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		timer = t.C
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
	case <-timer:
	}

	return nil
}
