package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/getnabbed/agent/internal/identity"
)

// ErrPairingExpired means the user did not approve before the code expired.
var ErrPairingExpired = errors.New("pairing code expired before it was approved")

// ErrPairingDenied means the user rejected the pairing.
var ErrPairingDenied = errors.New("pairing was denied")

// PairOptions configure Pair.
type PairOptions struct {
	ClientName string
	OS         string
	Version    string
	// Prompt is called once the user code is known. It must not block.
	Prompt func(*AuthorizeResponse)
	// Sleep waits between polls; nil uses a context-aware timer.
	Sleep func(context.Context, time.Duration) error
}

// Pair runs the RFC 8628 device flow for id and returns the issued tokens.
// The caller validates GatewayURL and persists the result.
func Pair(ctx context.Context, c *Client, id *identity.Identity, o PairOptions) (*TokenResponse, error) {
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	ar, err := c.Authorize(ctx, AuthorizeRequest{
		ClientName: o.ClientName,
		PublicKeys: PublicKeys{
			Ed25519: base64.RawURLEncoding.EncodeToString(id.SignPublic()),
			X25519:  base64.RawURLEncoding.EncodeToString(id.SealPublic()),
		},
		OS:      o.OS,
		Version: o.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("start pairing: %w", err)
	}
	if o.Prompt != nil {
		o.Prompt(ar)
	}

	interval := time.Duration(ar.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second // RFC 8628 §3.2 default
	}
	for {
		if err := o.Sleep(ctx, interval); err != nil {
			return nil, err
		}
		tr, err := c.Token(ctx, ar.DeviceCode)
		if err == nil {
			return tr, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			// Network trouble: keep polling until the code expires.
			continue
		}
		switch apiErr.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second // RFC 8628 §3.5
		case "expired_token":
			return nil, ErrPairingExpired
		case "access_denied":
			return nil, ErrPairingDenied
		default:
			if apiErr.Permanent() || apiErr.Status/100 == 4 {
				return nil, fmt.Errorf("pairing failed: %w", apiErr)
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
