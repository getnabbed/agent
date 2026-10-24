package usenet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/getnabbed/agent/internal/netguard"
)

// HealthState is what the agent last saw of a provider.
type HealthState int

// HealthState values.
const (
	// HealthUnknown means nothing has been tried yet.
	HealthUnknown HealthState = iota
	// HealthOK means the last connection succeeded.
	HealthOK
	// HealthFailing means the provider cannot be used right now.
	HealthFailing
)

// Health is a provider's state with a reason safe to show users: it never
// contains credentials.
type Health struct {
	State  HealthState
	Detail string
}

// maxDetail bounds a health detail; server messages are untrusted.
const maxDetail = 256

// setHealth records h and reports whether it changed. Call with mu held.
func (pl *pool) setHealth(h Health) bool {
	if pl.health == h {
		return false
	}

	pl.health = h

	return true
}

// notify tells the engine the health changed. Call without mu held.
func (pl *pool) notify() {
	if pl.onChange != nil {
		pl.onChange()
	}
}

func (pl *pool) currentHealth() Health {
	pl.mu.Lock()
	defer pl.mu.Unlock()

	return pl.health
}

// check dials one connection now and returns why it failed, ignoring dial
// back-off. Open connections that are working already prove the provider
// works, so a busy pool is not dialled past its limit. A successful
// connection is kept for downloads.
func (pl *pool) check(ctx context.Context) error {
	for {
		pl.mu.Lock()

		switch {
		case pl.closed:
			pl.mu.Unlock()
			return errPoolClosed
		case pl.fatalErr != nil:
			err := pl.fatalErr
			pl.mu.Unlock()

			return err
		case pl.health.State == HealthOK && pl.open > 0:
			pl.mu.Unlock()
			return nil
		case pl.open >= pl.limit:
			wait := pl.changed
			pl.mu.Unlock()

			if err := waitFor(ctx, wait, 0); err != nil {
				return err
			}

			continue
		}

		pl.open++
		pl.mu.Unlock()

		c, err := pl.dial(ctx)

		pl.mu.Lock()
		changed := false

		if err == nil {
			pl.failures = 0
			changed = pl.setHealth(Health{State: HealthOK})
			pl.idle = append(pl.idle, idleConn{c: c, used: time.Now()})
		} else {
			pl.open--

			if errors.Is(err, ErrAuth) || errors.Is(err, netguard.ErrBlocked) {
				pl.fatalErr = err
			}

			if ctx.Err() == nil {
				changed = pl.setHealth(Health{State: HealthFailing, Detail: Describe(err)})
			}
		}

		pl.signal()
		pl.mu.Unlock()

		if changed {
			pl.notify()
		}

		return err
	}
}

// Describe explains a dial or login failure in words for the user.
func Describe(err error) string {
	var (
		dns  *net.DNSError
		cert *tls.CertificateVerificationError
		host x509.HostnameError
		ua   x509.UnknownAuthorityError
	)

	var s string

	switch {
	case errors.Is(err, ErrAuth):
		s = "the provider rejected the username or password"
		if i := strings.Index(err.Error(), "(code "); i >= 0 {
			s += " " + err.Error()[i:]
		}
	case errors.Is(err, netguard.ErrBlocked):
		s = "the host resolves to a local network address, which sources from the web app may not use"
	case errors.Is(err, errCapacity):
		s = "the provider refused the connection: too many connections are open for this account"
	case errors.As(err, &dns):
		s = fmt.Sprintf("host %q not found", dns.Name)
		if !dns.IsNotFound {
			s = fmt.Sprintf("could not look up host %q", dns.Name)
		}
	case errors.As(err, &host):
		s = "the TLS certificate is for a different host: " + host.Error()
	case errors.As(err, &ua):
		s = "the TLS certificate is not from a trusted authority"
	case errors.As(err, &cert):
		s = "the TLS certificate is not valid: " + cert.Err.Error()
	case errors.Is(err, syscall.ECONNREFUSED):
		s = "connection refused: check the host and port"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		s = "timed out connecting: check the host, port and TLS setting"
	case errors.Is(err, ErrProviderDown):
		s = "the provider could not be reached after repeated attempts"
	default:
		s = err.Error()
	}

	s = strings.ToValidUTF8(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}

		return r
	}, s), "?")

	if len(s) > maxDetail {
		s = strings.ToValidUTF8(s[:maxDetail], "")
	}

	return s
}
