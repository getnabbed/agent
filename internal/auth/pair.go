package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"time"

	"github.com/getnabbed/agent/internal/identity"
)

// ErrPairingExpired means the user did not approve before the code expired.
var ErrPairingExpired = errors.New("pairing code expired before it was approved")

// ErrPairingDenied means the user rejected the pairing.
var ErrPairingDenied = errors.New("pairing was denied")

// ErrPairingRejected means the cloud kept rejecting fresh device codes with
// invalid_grant, so starting over again will not help.
var ErrPairingRejected = errors.New("pairing code was rejected by the cloud")

const (
	// maxPairRounds bounds how often Pair starts over with a new device code
	// after invalid_grant.
	maxPairRounds = 3
	// maxPollBackoff caps the poll delay after transport errors and 5xx.
	maxPollBackoff = time.Minute
	// defaultCodeLifetime is used when the cloud omits expires_in.
	defaultCodeLifetime = 15 * time.Minute
)

// errCodeRejected makes Pair start over with a new device code.
var errCodeRejected = errors.New("device code rejected")

// PairOptions configure Pair.
type PairOptions struct {
	ClientName string
	OS         string
	Version    string
	// Prompt is called each time a user code is issued. It must not block.
	Prompt func(*AuthorizeResponse)
	// Sleep waits between polls; nil uses a context-aware timer.
	Sleep func(context.Context, time.Duration) error
	// Log receives poll progress; nil uses slog.Default().
	Log *slog.Logger
}

// Pair runs the RFC 8628 device flow for id and returns the issued tokens.
// The caller validates GatewayURL and persists the result.
//
// Device codes are single use: once a poll has been answered with tokens,
// every later poll for that code gets invalid_grant. If that answer was lost
// in transit, the retry sees invalid_grant, so Pair discards the code and
// starts over with a new one, prompting the user again.
func Pair(ctx context.Context, c *Client, id *identity.Identity, o PairOptions) (*TokenResponse, error) {
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}

	if o.Log == nil {
		o.Log = slog.Default()
	}

	for round := 1; ; round++ {
		tr, err := pairOnce(ctx, c, id, o)
		if !errors.Is(err, errCodeRejected) {
			return tr, err
		}

		if round >= maxPairRounds {
			return nil, fmt.Errorf("%w: %w", ErrPairingRejected, err)
		}

		o.Log.Warn("pairing code is no longer valid; starting a new pairing", "err", err)
	}
}

// pairOnce gets one device code and polls it until it resolves. It returns
// errCodeRejected when the code must be discarded and pairing restarted.
func pairOnce(ctx context.Context, c *Client, id *identity.Identity, o PairOptions) (*TokenResponse, error) {
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

	if err := checkAuthorizeResponse(c.base, ar); err != nil {
		return nil, fmt.Errorf("refusing pairing response: %w", err)
	}

	if o.Prompt != nil {
		o.Prompt(ar)
	}

	interval := time.Duration(ar.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second // RFC 8628 §3.2 default
	}

	lifetime := time.Duration(ar.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultCodeLifetime
	}

	// Wall clock, checked at the point of use: the monotonic clock pauses
	// while the host sleeps.
	expires := time.Now().Add(lifetime)
	failures := 0 // consecutive transport errors and 5xx
	for {
		delay := interval
		if failures > 0 {
			delay = min(interval<<min(failures, 6), maxPollBackoff)
		}

		if err := o.Sleep(ctx, delay); err != nil {
			return nil, err
		}

		if time.Now().After(expires) {
			return nil, ErrPairingExpired
		}

		tr, err := c.Token(ctx, ar.DeviceCode)
		if err == nil {
			o.Log.Debug("pairing approved")
			return tr, nil
		}

		// The cloud answered with success but the body is unusable. The device
		// code is spent, so polling again would only get invalid_grant.
		if errors.Is(err, ErrBadResponse) {
			return nil, fmt.Errorf("pairing approved but %w", err)
		}

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			// Network trouble. The cloud may have answered and the answer been
			// lost; if so, the next poll gets invalid_grant and we start over.
			failures++
			o.Log.Warn("pairing poll failed; retrying", "err", err)
			continue
		}

		o.Log.Debug("pairing poll", "status", apiErr.Status, "code", apiErr.Code)

		if apiErr.Status/100 == 5 {
			failures++
			o.Log.Warn("pairing poll failed; retrying", "err", apiErr)
			continue
		}

		failures = 0
		switch apiErr.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second // RFC 8628 §3.5
		case "expired_token":
			return nil, ErrPairingExpired
		case "access_denied":
			return nil, ErrPairingDenied
		case "invalid_grant":
			// Already redeemed (perhaps by a poll whose answer was lost), or
			// the agent it approved was revoked. Never poll this code again.
			return nil, fmt.Errorf("%w: %w", errCodeRejected, apiErr)
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

var userCodePattern = regexp.MustCompile(`^[A-Za-z0-9-]{4,32}$`)

// checkAuthorizeResponse validates what the user will be shown: the code
// must be plain characters (no terminal escapes) and the links must point at
// the API's own site.
func checkAuthorizeResponse(api *url.URL, ar *AuthorizeResponse) error {
	if !userCodePattern.MatchString(ar.UserCode) {
		return errors.New("user_code must be 4-32 letters, digits or hyphens")
	}

	if err := ValidateVerificationURI(api, ar.VerificationURI); err != nil {
		return fmt.Errorf("verification_uri: %w", err)
	}

	if ar.VerificationURIComplete != "" {
		if err := ValidateVerificationURI(api, ar.VerificationURIComplete); err != nil {
			return fmt.Errorf("verification_uri_complete: %w", err)
		}
	}

	return nil
}
