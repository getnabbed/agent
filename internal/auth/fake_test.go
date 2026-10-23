package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/getnabbed/agent/internal/identity"
)

// fakeAPI is a minimal cloud-api implementing the server-side contract
// proposed in design 0003: rotation, reuse detection, and an idempotent
// retry window keyed by (refresh token, nonce).
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	pub          ed25519.PublicKey
	pendingPolls int
	current      string            // live refresh token
	spent        map[string]string // spent token -> nonce that spent it
	replay       map[string]map[string]string
	revoked      bool
	seq          int
	dropNext     bool // simulate the response being lost after rotation
	refreshes    int
	omitGateway  bool // answer the token poll without gateway_url
	numericIDs   bool // send tenant_id as a JSON number, as cloud-api does
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, spent: map[string]string{}, replay: map[string]map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/device/authorize", f.authorize)
	mux.HandleFunc("POST /api/device/token", f.token)
	mux.HandleFunc("POST /api/device/refresh", f.refresh)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeAPI) url() *url.URL { u, _ := url.Parse(f.srv.URL); return u }

func (f *fakeAPI) jwt() string {
	f.seq++
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{"jti": fmt.Sprintf("jti-%d", f.seq), "iat": now, "exp": now + 1800})

	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) authorize(w http.ResponseWriter, r *http.Request) {
	var req AuthorizeRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	pub, err := base64.RawURLEncoding.DecodeString(req.PublicKeys.Ed25519)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}

	f.mu.Lock()
	f.pub = pub
	f.mu.Unlock()
	writeJSON(w, 200, AuthorizeResponse{DeviceCode: "dc", UserCode: "ABCD-EFGH", VerificationURI: "http://" + r.Host + "/device", Interval: 1})
}

func (f *fakeAPI) token(w http.ResponseWriter, r *http.Request) {
	// Like cloud-api: RFC 8628 requires the grant type.
	var req map[string]string
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req["grant_type"] != GrantTypeDeviceCode || req["device_code"] == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingPolls > 0 {
		f.pendingPolls--
		writeJSON(w, 400, map[string]string{"error": "authorization_pending"})
		return
	}

	f.current = fmt.Sprintf("rt-%d", f.seq)
	gw := "https://gw.example"
	if f.omitGateway {
		gw = ""
	}

	if f.numericIDs {
		writeJSON(w, 200, map[string]any{"agent_id": "agent-1", "tenant_id": 42, "access_token": f.jwt(), "refresh_token": f.current, "gateway_url": gw})
		return
	}

	writeJSON(w, 200, TokenResponse{AgentID: "agent-1", TenantID: "tenant-1", AccessToken: f.jwt(), RefreshToken: f.current, GatewayURL: gw})
}

func (f *fakeAPI) refresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.GrantType != GrantTypeRefreshToken {
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++

	sig, _ := base64.RawURLEncoding.DecodeString(req.Signature)
	msg := identity.RefreshSigningInput(f.url().Host, req.RefreshToken, req.TimestampMs, req.Nonce)
	if !identity.Verify(f.pub, msg, sig) {
		writeJSON(w, 401, map[string]string{"error": "invalid_grant", "error_description": "bad proof"})
		return
	}

	if f.revoked {
		writeJSON(w, 401, map[string]string{"error": "agent_revoked"})
		return
	}

	// Idempotent retry of an already-rotated token with the same nonce.
	if byNonce, ok := f.replay[req.RefreshToken]; ok {
		if body, ok := byNonce[req.Nonce]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
	}

	if _, ok := f.spent[req.RefreshToken]; ok {
		f.revoked = true // reuse detected
		writeJSON(w, 401, map[string]string{"error": "invalid_grant", "error_description": "refresh token reuse"})
		return
	}

	if req.RefreshToken != f.current {
		writeJSON(w, 401, map[string]string{"error": "invalid_grant"})
		return
	}

	f.spent[req.RefreshToken] = req.Nonce
	f.current = fmt.Sprintf("rt-%d", f.seq+1)
	body, _ := json.Marshal(RefreshResponse{AccessToken: f.jwt(), RefreshToken: f.current})
	f.replay[req.RefreshToken] = map[string]string{req.Nonce: string(body)}
	if f.dropNext {
		f.dropNext = false
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
