package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/getnabbed/agent/internal/clock"
	"github.com/getnabbed/agent/internal/credstore"
	"github.com/getnabbed/agent/internal/identity"
)

// ErrReauthRequired means the agent's refresh token was rejected for good:
// the agent was revoked, the token expired after 90 days offline, or reuse
// was detected. The user must pair again.
var ErrReauthRequired = errors.New("agent credentials were rejected; pair the agent again")

// AccessToken is a short-lived agent JWT held only in memory.
type AccessToken struct {
	Raw string
	JTI string
	// Expiry is in local wall-clock time, computed from the token lifetime
	// (exp - iat) added to when it was received, so host clock error does not
	// shift it.
	Expiry time.Time
	// Lifetime is exp - iat.
	Lifetime time.Duration
}

// refreshMargin is how long before expiry a token is treated as stale.
func (t AccessToken) refreshMargin() time.Duration {
	return min(5*time.Minute, t.Lifetime/4)
}

// Fresh reports whether the token can still be used at now.
func (t AccessToken) Fresh(now time.Time) bool {
	return t.Raw != "" && now.Add(t.refreshMargin()).Before(t.Expiry)
}

// ParseAccessToken reads the claims the agent needs from a JWT without
// verifying it. The agent is not the token's audience; the gateway verifies.
func ParseAccessToken(raw string, receivedAt time.Time) (AccessToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return AccessToken{}, errors.New("access token is not a JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return AccessToken{}, fmt.Errorf("access token payload: %w", err)
	}

	var claims struct {
		JTI string `json:"jti"`
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
	}

	if err := json.Unmarshal(payload, &claims); err != nil {
		return AccessToken{}, fmt.Errorf("access token claims: %w", err)
	}

	if claims.Exp == 0 || claims.JTI == "" {
		return AccessToken{}, errors.New("access token lacks exp or jti")
	}

	t := AccessToken{Raw: raw, JTI: claims.JTI}
	if claims.Iat > 0 && claims.Exp > claims.Iat {
		t.Lifetime = time.Duration(claims.Exp-claims.Iat) * time.Second
		t.Expiry = receivedAt.Add(t.Lifetime)
	} else {
		t.Expiry = time.Unix(claims.Exp, 0)
		t.Lifetime = t.Expiry.Sub(receivedAt)
	}

	return t, nil
}

// TokenSource hands out access tokens, refreshing lazily when the current
// one is missing or near expiry. There is deliberately no background timer:
// Go timers run on the monotonic clock, which pauses during system sleep on
// Linux and macOS, so a timer-driven refresh fires late after a laptop wakes.
// Checking wall-clock freshness on every use is correct on every OS.
//
// Refresh is crash-safe. Before spending the refresh token the source
// persists a PendingRefresh{nonce}. If the process dies after the server
// rotated the token but before the new one was saved, the next attempt
// re-sends the old token with the same nonce, which the server answers
// idempotently with the same new pair instead of revoking the agent for
// reuse.
type TokenSource struct {
	client *Client
	store  *credstore.Store
	id     *identity.Identity
	clock  *clock.Clock
	log    *slog.Logger

	mu      sync.Mutex
	creds   *credstore.Credentials
	current AccessToken
}

// NewTokenSource returns a TokenSource for already-loaded credentials.
func NewTokenSource(c *Client, store *credstore.Store, id *identity.Identity, creds *credstore.Credentials, clk *clock.Clock, log *slog.Logger) *TokenSource {
	if clk == nil {
		clk = clock.New()
	}

	if log == nil {
		log = slog.Default()
	}

	return &TokenSource{client: c, store: store, id: id, clock: clk, log: log, creds: creds.Clone()}
}

// Seed installs an access token obtained elsewhere, e.g. from pairing.
func (ts *TokenSource) Seed(raw string) error {
	t, err := ParseAccessToken(raw, ts.clock.Local())
	if err != nil {
		return err
	}

	ts.mu.Lock()
	ts.current = t
	ts.mu.Unlock()

	return nil
}

// Token returns a fresh access token, refreshing if needed. Concurrent
// callers share a single refresh.
func (ts *TokenSource) Token(ctx context.Context) (AccessToken, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.current.Fresh(ts.clock.Local()) {
		return ts.current, nil
	}

	return ts.refreshLocked(ctx)
}

// Invalidate drops raw if it is still the current token, forcing the next
// Token call to refresh. Call it on Unauthenticated or GoAway{TOKEN_EXPIRING}.
func (ts *TokenSource) Invalidate(raw string) {
	ts.mu.Lock()
	if ts.current.Raw == raw {
		ts.current = AccessToken{}
	}
	ts.mu.Unlock()
}

func (ts *TokenSource) refreshLocked(ctx context.Context) (AccessToken, error) {
	pr := ts.creds.PendingRefresh
	if pr == nil || pr.RefreshToken != ts.creds.RefreshToken {
		nonce, err := newNonce()
		if err != nil {
			return AccessToken{}, err
		}

		pr = &credstore.PendingRefresh{Nonce: nonce, RefreshToken: ts.creds.RefreshToken, StartedAt: ts.clock.Local().UTC()}
		next := ts.creds.Clone()
		next.PendingRefresh = pr
		// The intent must be durable before the token is spent.
		if err := ts.store.Save(next); err != nil {
			return AccessToken{}, fmt.Errorf("persist refresh intent: %w", err)
		}

		ts.creds = next
	} else {
		ts.log.Info("retrying interrupted token refresh", "started_at", pr.StartedAt)
	}

	tsMs := ts.clock.Now().UnixMilli()
	sig := ts.id.Sign(identity.RefreshSigningInput(ts.client.Host(), pr.RefreshToken, tsMs, pr.Nonce))
	resp, err := ts.client.Refresh(ctx, RefreshRequest{
		RefreshToken: pr.RefreshToken,
		TimestampMs:  tsMs,
		Nonce:        pr.Nonce,
		Signature:    base64.RawURLEncoding.EncodeToString(sig),
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Permanent() {
			return AccessToken{}, fmt.Errorf("%w: %w", ErrReauthRequired, apiErr)
		}

		return AccessToken{}, fmt.Errorf("refresh access token: %w", err)
	}

	// The rotated refresh token must be durable before anything from this
	// response is used. Using the new access token ends the server's
	// idempotent-retry window for the old refresh token; if the new one were
	// then lost, the next start would replay the old one and be revoked for
	// reuse. If the save keeps failing, nothing is used: the intent on disk
	// still allows the same idempotent retry later.
	next := ts.creds.Clone()
	next.RefreshToken = resp.RefreshToken
	next.PendingRefresh = nil
	next.RefreshedAt = ts.clock.Local().UTC()

	if err := ts.saveDurably(ctx, next); err != nil {
		return AccessToken{}, fmt.Errorf("persist rotated refresh token: %w", err)
	}

	ts.creds = next

	tok, err := ParseAccessToken(resp.AccessToken, ts.clock.Local())
	if err != nil {
		return AccessToken{}, err
	}

	ts.current = tok
	ts.log.Debug("access token refreshed", "jti", tok.JTI, "expires_in", tok.Expiry.Sub(ts.clock.Local()).Round(time.Second))

	return tok, nil
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

// saveDurably retries Save with backoff: a transient failure (a Windows
// sharing violation, a momentarily full disk) must not lose a rotated token.
func (ts *TokenSource) saveDurably(ctx context.Context, c *credstore.Credentials) error {
	var err error

	for attempt := range 5 {
		if err = ts.store.Save(c); err == nil {
			return nil
		}

		ts.log.Warn("saving rotated refresh token failed; retrying", "attempt", attempt+1, "err", err)

		t := time.NewTimer(time.Duration(100<<attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}

	return err
}
