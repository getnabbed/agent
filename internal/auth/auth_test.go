package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/credstore"
	"github.com/getnabbed/agent/internal/identity"
)

var quiet = slog.New(slog.DiscardHandler)

func noSleep(context.Context, time.Duration) error { return nil }

// pairForTest runs pairing against f and returns a ready TokenSource.
func pairForTest(t *testing.T, f *fakeAPI) (*TokenSource, *credstore.Store, *identity.Identity) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	c := NewClient(f.url(), f.srv.Client(), "test")
	f.pendingPolls = 2
	var prompted *AuthorizeResponse
	tr, err := Pair(context.Background(), c, id, PairOptions{ClientName: "t", Prompt: func(a *AuthorizeResponse) { prompted = a }, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}

	if prompted == nil || prompted.UserCode != "ABCD-EFGH" {
		t.Fatalf("prompt not called with user code: %+v", prompted)
	}

	store, err := credstore.OpenWith(filepath.Join(t.TempDir(), "data"), credstore.PlaintextProtector{}, quiet)
	if err != nil {
		t.Fatal(err)
	}

	creds := &credstore.Credentials{AgentID: tr.AgentID, TenantID: tr.TenantID, APIURL: f.srv.URL, GatewayURL: tr.GatewayURL,
		SignSeed: id.SignSeed(), SealKey: id.SealKey(), RefreshToken: tr.RefreshToken, PairedAt: time.Now()}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}

	ts := NewTokenSource(c, store, id, creds, nil, quiet)
	if err := ts.Seed(tr.AccessToken); err != nil {
		t.Fatal(err)
	}

	return ts, store, id
}

func TestPairAndUseSeededToken(t *testing.T) {
	f := newFakeAPI(t)
	ts, _, _ := pairForTest(t, f)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if tok.JTI == "" || f.refreshes != 0 {
		t.Fatalf("expected seeded token without refresh, got jti=%q refreshes=%d", tok.JTI, f.refreshes)
	}
}

func TestRefreshRotatesAndPersists(t *testing.T) {
	f := newFakeAPI(t)
	ts, store, _ := pairForTest(t, f)
	first, _ := ts.Token(context.Background())
	ts.Invalidate(first.Raw)

	second, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if second.JTI == first.JTI {
		t.Fatal("expected a new access token")
	}

	creds, _ := store.Load()
	if creds.RefreshToken != f.current || creds.PendingRefresh != nil {
		t.Fatalf("store not updated: refresh=%q pending=%v (server current %q)", creds.RefreshToken, creds.PendingRefresh, f.current)
	}
}

// The response is lost after the server rotated the token, and the process
// "restarts" from disk. The retry must reuse the nonce and succeed rather
// than tripping reuse detection.
func TestRefreshSurvivesCrashAfterRotation(t *testing.T) {
	f := newFakeAPI(t)
	ts, store, id := pairForTest(t, f)
	first, _ := ts.Token(context.Background())
	ts.Invalidate(first.Raw)

	f.dropNext = true
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("expected transport error when response is dropped")
	}

	creds, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	if creds.PendingRefresh == nil {
		t.Fatal("refresh intent was not persisted before spending the token")
	}

	restarted := NewTokenSource(NewClient(f.url(), f.srv.Client(), "test"), store, id, creds, nil, quiet)
	if _, err := restarted.Token(context.Background()); err != nil {
		t.Fatalf("retry after crash failed: %v", err)
	}

	if f.revoked {
		t.Fatal("agent was revoked: retry was treated as refresh-token reuse")
	}

	creds, _ = store.Load()
	if creds.RefreshToken != f.current || creds.PendingRefresh != nil {
		t.Fatalf("store not converged: refresh=%q server=%q pending=%v", creds.RefreshToken, f.current, creds.PendingRefresh)
	}
}

// A cloned data dir spending the same token with a different nonce is reuse.
func TestReuseWithDifferentNonceRevokes(t *testing.T) {
	f := newFakeAPI(t)
	ts, store, id := pairForTest(t, f)
	snapshot, _ := store.Load()

	first, _ := ts.Token(context.Background())
	ts.Invalidate(first.Raw)
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}

	clone := NewTokenSource(NewClient(f.url(), f.srv.Client(), "test"), store, id, snapshot, nil, quiet)
	_, err := clone.Token(context.Background())
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("clone refresh err = %v, want ErrReauthRequired", err)
	}

	if !f.revoked {
		t.Fatal("server should have revoked the agent on reuse")
	}
}

func TestParseAccessTokenUsesLifetimeNotHostClock(t *testing.T) {
	// Token issued "in 2001" by a server whose clock disagrees with ours.
	raw := "e30.eyJqdGkiOiJqIiwiaWF0IjoxMDAwMDAwMDAwLCJleHAiOjEwMDAwMDE4MDB9.s"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tok, err := ParseAccessToken(raw, now)
	if err != nil {
		t.Fatal(err)
	}

	if tok.Lifetime != 30*time.Minute || !tok.Expiry.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("lifetime=%v expiry=%v", tok.Lifetime, tok.Expiry)
	}

	if !tok.Fresh(now.Add(20*time.Minute)) || tok.Fresh(now.Add(26*time.Minute)) {
		t.Fatal("freshness margin wrong: want refresh within the last 5 minutes")
	}
}

func TestValidateGatewayURL(t *testing.T) {
	api, _ := url.Parse("https://api.nabbed.co.uk")
	ok := []string{"https://gateway.nabbed.co.uk", "https://gw.eu.nabbed.co.uk:8443"}
	bad := []string{"http://gateway.nabbed.co.uk", "https://nabbed.evil.co.uk", "https://nabbedd.co.uk", "https://co.uk", "https://u:p@gw.nabbed.co.uk"}
	for _, s := range ok {
		if _, err := ValidateGatewayURL(api, s); err != nil {
			t.Errorf("%s: unexpected error %v", s, err)
		}
	}

	for _, s := range bad {
		if _, err := ValidateGatewayURL(api, s); err == nil {
			t.Errorf("%s: expected rejection", s)
		}
	}

	local, _ := url.Parse("http://localhost:4000")
	if _, err := ValidateGatewayURL(local, "http://localhost:50051"); err != nil {
		t.Errorf("loopback dev setup rejected: %v", err)
	}
}

func TestParseAPIURL(t *testing.T) {
	for _, s := range []string{"https://api.example", "http://127.0.0.1:4000"} {
		if _, err := ParseAPIURL(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}

	for _, s := range []string{"http://api.example", "ftp://x", "https://api.example?x=1"} {
		if _, err := ParseAPIURL(s); err == nil {
			t.Errorf("%s: expected error", s)
		}
	}
}

func TestPairingResponseIsValidated(t *testing.T) {
	api, _ := url.Parse("https://api.nabbed.example")

	ok := &AuthorizeResponse{UserCode: "ABCD-1234", VerificationURI: "https://app.nabbed.example/device"}
	if err := checkAuthorizeResponse(api, ok); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}

	for _, bad := range []*AuthorizeResponse{
		{UserCode: "ABCD-1234", VerificationURI: "https://nabbed-login.evil/device"},
		{UserCode: "ABCD-1234", VerificationURI: "http://app.nabbed.example/device"},
		{UserCode: "\x1b[2J\x1b[31mPWNED", VerificationURI: "https://app.nabbed.example/device"},
		{UserCode: "ABCD-1234", VerificationURI: "https://app.nabbed.example/device", VerificationURIComplete: "https://evil.example/?c=1"},
	} {
		if err := checkAuthorizeResponse(api, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestOnlyExplicitCodesArePermanent(t *testing.T) {
	if (&APIError{Status: 401, Code: "Unauthorized"}).Permanent() {
		t.Fatal("a bare 401 (WAF, proxy) must not force a re-pair")
	}

	if !(&APIError{Status: 400, Code: "invalid_grant"}).Permanent() {
		t.Fatal("invalid_grant must be permanent")
	}
}

func TestAPIHostIsLowercased(t *testing.T) {
	u, err := ParseAPIURL("https://API.Nabbed.Example")
	if err != nil || u.Host != "api.nabbed.example" {
		t.Fatalf("host = %q, %v", u.Host, err)
	}
}

// M6: if the rotated token cannot be saved, the new access token must not
// be used, and the persisted intent must still allow an idempotent retry.
func TestRotatedTokenIsNotUsedUntilSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root from writing")
	}

	f := newFakeAPI(t)
	ts, store, _ := pairForTest(t, f)
	first, _ := ts.Token(context.Background())
	ts.Invalidate(first.Raw)

	// Make the data dir read-only so Save fails after the server rotates.
	creds, _ := store.Load()
	pending := creds.Clone()
	pending.PendingRefresh = &credstore.PendingRefresh{Nonce: "n1", RefreshToken: creds.RefreshToken, StartedAt: time.Now()}
	if err := store.Save(pending); err != nil {
		t.Fatal(err)
	}

	ts = NewTokenSource(NewClient(f.url(), f.srv.Client(), "test"), store, mustIdentity(t, store), pending, nil, quiet)

	if err := os.Chmod(store.Dir(), 0o500); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(store.Dir(), 0o700) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := ts.Token(ctx); err == nil {
		t.Fatal("token returned although the rotated refresh token was not saved")
	}

	if err := os.Chmod(store.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}

	// The retry replays the same nonce, the server answers idempotently,
	// and the agent is not revoked.
	if _, err := ts.Token(ctx); err != nil {
		t.Fatalf("retry after save failure: %v", err)
	}

	if f.revoked {
		t.Fatal("agent revoked after a failed save")
	}
}

func mustIdentity(t *testing.T, store *credstore.Store) *identity.Identity {
	t.Helper()

	c, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	id, err := identity.FromSecrets(c.SignSeed, c.SealKey)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func TestVerificationURIMustBeAbsolute(t *testing.T) {
	api, _ := url.Parse("https://api.nabbed.media")

	err := ValidateVerificationURI(api, "/device")
	if err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("err = %v, want a clear not-absolute error", err)
	}

	// What the server should send: the web app on the same site.
	if err := ValidateVerificationURI(api, "https://app.nabbed.media/device"); err != nil {
		t.Fatalf("absolute app URL rejected: %v", err)
	}
}
