package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
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

	creds := &credstore.Credentials{AgentID: string(tr.AgentID), TenantID: string(tr.TenantID), APIURL: f.srv.URL, GatewayURL: tr.GatewayURL,
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

// failingProtector stores plaintext like credstore.PlaintextProtector, but
// fails every Protect while fail is set. It makes Save fail on every
// platform: a read-only directory stops neither Windows nor root.
type failingProtector struct {
	credstore.PlaintextProtector

	fail atomic.Bool
}

var errInjectedSave = errors.New("injected save failure")

// Protect implements credstore.Protector.
func (p *failingProtector) Protect(b []byte) ([]byte, error) {
	if p.fail.Load() {
		return nil, errInjectedSave
	}

	return p.PlaintextProtector.Protect(b)
}

// M6: if the rotated token cannot be saved, the new access token must not
// be used, and the persisted intent must still allow an idempotent retry.
func TestRotatedTokenIsNotUsedUntilSaved(t *testing.T) {
	f := newFakeAPI(t)
	ts, store, _ := pairForTest(t, f)
	first, _ := ts.Token(context.Background())
	ts.Invalidate(first.Raw)

	creds, _ := store.Load()
	pending := creds.Clone()
	pending.PendingRefresh = &credstore.PendingRefresh{Nonce: "n1", RefreshToken: creds.RefreshToken, StartedAt: time.Now()}
	if err := store.Save(pending); err != nil {
		t.Fatal(err)
	}

	// Reopen the store with a protector that makes Save fail after the
	// server rotates.
	prot := &failingProtector{}
	failing, err := credstore.OpenWith(store.Dir(), prot, quiet)
	if err != nil {
		t.Fatal(err)
	}

	ts = NewTokenSource(NewClient(f.url(), f.srv.Client(), "test"), failing, mustIdentity(t, store), pending, nil, quiet)
	prot.fail.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := ts.Token(ctx); !errors.Is(err, errInjectedSave) {
		t.Fatalf("got Token error = %v, want %v: the rotated refresh token was not saved", err, errInjectedSave)
	}

	prot.fail.Store(false)

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

func TestPairFailsFastOnUnusableTokenResponse(t *testing.T) {
	f := newFakeAPI(t)
	f.omitGateway = true

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	polls := 0
	sleep := func(context.Context, time.Duration) error {
		polls++
		if polls > 3 {
			t.Fatal("still polling after the cloud approved with an unusable response")
		}

		return nil
	}

	c := NewClient(f.url(), f.srv.Client(), "test")

	_, err = Pair(context.Background(), c, id, PairOptions{ClientName: "t", Sleep: sleep, Log: quiet})
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("got err = %v, want ErrBadResponse", err)
	}

	if !strings.Contains(err.Error(), "gateway_url") {
		t.Fatalf("got err = %v, want it to name gateway_url", err)
	}
}

func TestTokenAcceptsNumericTenantID(t *testing.T) {
	f := newFakeAPI(t)
	f.numericIDs = true

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	c := NewClient(f.url(), f.srv.Client(), "test")

	tr, err := Pair(context.Background(), c, id, PairOptions{ClientName: "t", Sleep: noSleep, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}

	if tr.TenantID != "42" {
		t.Fatalf("got TenantID = %q, want %q", tr.TenantID, "42")
	}
}

func TestWireIDRejectsFractions(t *testing.T) {
	var w WireID
	if err := w.UnmarshalJSON([]byte("4.5")); err == nil {
		t.Fatalf("got WireID = %q, want an error", w)
	}
}

func TestPairStartsOverWhenApprovalResponseIsLost(t *testing.T) {
	f := newFakeAPI(t)
	f.dropNextToken = true

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	var codes []string
	c := NewClient(f.url(), f.srv.Client(), "test")
	tr, err := Pair(context.Background(), c, id, PairOptions{
		ClientName: "t", Sleep: noSleep, Log: quiet,
		Prompt: func(a *AuthorizeResponse) { codes = append(codes, a.DeviceCode) },
	})
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}

	if tr.AccessToken == "" {
		t.Fatal("got no access token")
	}

	// dc-1 was redeemed by the lost poll, retried once (invalid_grant), then
	// dropped for dc-2.
	if got, want := strings.Join(codes, ","), "dc-1,dc-2"; got != want {
		t.Fatalf("got device codes %s, want %s", got, want)
	}

	if f.tokenPolls != 3 {
		t.Fatalf("got %d token polls, want 3", f.tokenPolls)
	}
}

func TestPairGivesUpAfterRepeatedInvalidGrant(t *testing.T) {
	f := newFakeAPI(t)
	f.rejectAll = true

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	c := NewClient(f.url(), f.srv.Client(), "test")
	_, err = Pair(context.Background(), c, id, PairOptions{ClientName: "t", Sleep: noSleep, Log: quiet})
	if !errors.Is(err, ErrPairingRejected) {
		t.Fatalf("got err = %v, want ErrPairingRejected", err)
	}

	// Each code is polled exactly once: invalid_grant is never retried.
	if f.authorizes != maxPairRounds || f.tokenPolls != maxPairRounds {
		t.Fatalf("got %d authorizes and %d polls, want %d of each", f.authorizes, f.tokenPolls, maxPairRounds)
	}
}

func TestPairRetriesServerErrorsWithBackoff(t *testing.T) {
	f := newFakeAPI(t)
	f.serverErrors = 3

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	var delays []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	c := NewClient(f.url(), f.srv.Client(), "test")
	if _, err := Pair(context.Background(), c, id, PairOptions{ClientName: "t", Sleep: sleep, Log: quiet}); err != nil {
		t.Fatalf("Pair: %v", err)
	}

	if f.authorizes != 1 {
		t.Fatalf("got %d authorizes, want 1: server_error must not restart pairing", f.authorizes)
	}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("got delays %v, want %v", delays, want)
	}
}

func TestPairStopsPollingOnceCodeLifetimeIsOver(t *testing.T) {
	f := newFakeAPI(t)
	f.serverErrors = 1 << 30
	f.expiresIn = 1

	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	polls := 0
	sleep := func(context.Context, time.Duration) error {
		polls++
		if polls > 200 {
			t.Fatal("still polling a code past its lifetime")
		}

		time.Sleep(20 * time.Millisecond)

		return nil
	}

	c := NewClient(f.url(), f.srv.Client(), "test")
	_, err = Pair(context.Background(), c, id, PairOptions{ClientName: "t", Sleep: sleep, Log: quiet})
	if !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("got err = %v, want ErrPairingExpired", err)
	}
}
