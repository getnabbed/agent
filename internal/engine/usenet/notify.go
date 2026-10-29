package usenet

import (
	"context"
	"fmt"
	"strconv"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// codeConnectionsCapped is notice.CodeUsenetConnectionsCapped, repeated so
// the engine doesn't depend on the store.
const codeConnectionsCapped = "usenet.connections_capped"

// Notifier reports agent notices (design 0008 §2). *notice.Reporter
// implements it.
type Notifier interface {
	Raise(ctx context.Context, code, subject string, sev pb.Severity, message string, attrs map[string]string) error
	Resolve(ctx context.Context, code, subject string) error
}

// SetNotifier makes the engine raise usenet.connections_capped, with the
// source id as subject, when a provider allows fewer connections than
// configured, and resolve it once a pool for the source holds the
// configured number or the source is removed. nil, the default, reports
// none. Call it before adding providers.
func (e *Engine) SetNotifier(n Notifier) { e.notifier.Store(&n) }

func (e *Engine) notifierOrNil() Notifier {
	if p := e.notifier.Load(); p != nil {
		return *p
	}

	return nil
}

// watchLimit connects pl's limit hooks to the notifier.
func (e *Engine) watchLimit(id string, pl *pool) {
	pl.onCapped = func(configured, using int) {
		n := e.notifierOrNil()
		if n == nil {
			return
		}

		err := n.Raise(context.Background(), codeConnectionsCapped, id, pb.Severity_SEVERITY_INFO,
			fmt.Sprintf("usenet provider %s allows %d connections of the %d configured", pl.p.Host, using, configured),
			map[string]string{"configured": strconv.Itoa(configured), "using": strconv.Itoa(using)})
		if err != nil {
			e.log.Warn("report notice", "code", codeConnectionsCapped, "source_id", id, "err", err)
		}
	}

	pl.onFull = func() { e.resolveCapped(id) }
}

func (e *Engine) resolveCapped(id string) {
	n := e.notifierOrNil()
	if n == nil {
		return
	}

	if err := n.Resolve(context.Background(), codeConnectionsCapped, id); err != nil {
		e.log.Warn("report notice", "code", codeConnectionsCapped, "source_id", id, "err", err)
	}
}
