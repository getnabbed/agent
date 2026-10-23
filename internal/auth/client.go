// Package auth implements the agent side of pairing (RFC 8628 device flow)
// and access-token management with Ed25519 proof of possession.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

// ErrBadResponse means cloud-api answered with success but the body was not
// usable: undecodable, missing fields or invalid values. Retrying the same
// request will not fix it.
var ErrBadResponse = errors.New("cloud-api: unusable response")

// OAuth grant types sent to cloud-api.
const (
	// GrantTypeDeviceCode is required when polling /api/device/token
	// (RFC 8628 section 3.4).
	GrantTypeDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"
	// GrantTypeRefreshToken is sent to /api/device/refresh (RFC 6749 section 6).
	GrantTypeRefreshToken = "refresh_token"
)

// Client talks to the cloud-api device endpoints.
type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
}

// NewClient returns a Client for api. hc may be nil.
func NewClient(api *url.URL, hc *http.Client, userAgent string) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	return &Client{base: api, http: hc, userAgent: userAgent}
}

// Host returns the API host, which refresh signatures bind to.
func (c *Client) Host() string { return c.base.Host }

// APIError is a non-2xx response from cloud-api. Code follows RFC 6749 §5.2
// and RFC 8628 §3.5 (authorization_pending, slow_down, expired_token,
// access_denied, invalid_grant).
type APIError struct {
	Status      int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *APIError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("cloud-api %d %s: %s", e.Status, e.Code, e.Description)
	}

	return fmt.Sprintf("cloud-api %d %s", e.Status, e.Code)
}

// Permanent reports whether retrying the same request cannot succeed.
func (e *APIError) Permanent() bool {
	// Only explicit OAuth or agent error codes count. A bare 401 or 403 can
	// come from a WAF, a captive portal or a TLS-intercepting proxy, and
	// treating it as permanent would force a needless re-pair.
	switch e.Code {
	case "invalid_grant", "access_denied", "expired_token", "invalid_client", "unauthorized_client", "agent_revoked":
		return true
	}

	return false
}

// PublicKeys are sent at pairing, base64url without padding.
type PublicKeys struct {
	Ed25519 string `json:"ed25519"`
	X25519  string `json:"x25519"`
}

// AuthorizeRequest is the body of POST /api/device/authorize.
type AuthorizeRequest struct {
	ClientName string     `json:"client_name"`
	PublicKeys PublicKeys `json:"public_keys"`
	OS         string     `json:"os"`
	Version    string     `json:"version"`
}

// AuthorizeResponse follows RFC 8628 §3.2.
type AuthorizeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in,omitempty"`
	Interval                int    `json:"interval"`
}

// TokenResponse is the success body of POST /api/device/token.
type TokenResponse struct {
	AgentID      WireID `json:"agent_id"`
	TenantID     WireID `json:"tenant_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	GatewayURL   string `json:"gateway_url"`
}

// WireID is a cloud-issued id. cloud-api may send it as a JSON string or as a
// whole number; both decode to the same decimal string.
type WireID string

// UnmarshalJSON accepts a string or an integer.
func (w *WireID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*w = WireID(s)
		return nil
	}

	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("id must be a string or integer: %w", err)
	}

	if _, err := n.Int64(); err != nil {
		return fmt.Errorf("id must be a whole number, got %s", n)
	}

	*w = WireID(n.String())

	return nil
}

// RefreshRequest is the body of POST /api/device/refresh. Signature is
// base64url of Ed25519 over identity.RefreshSigningInput.
type RefreshRequest struct {
	// GrantType is always "refresh_token" (RFC 6749 section 6); Refresh sets it.
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	TimestampMs  int64  `json:"timestamp_ms"`
	Nonce        string `json:"nonce"`
	Signature    string `json:"signature"`
}

// RefreshResponse is the success body of POST /api/device/refresh.
type RefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// Authorize starts the device flow.
func (c *Client) Authorize(ctx context.Context, req AuthorizeRequest) (*AuthorizeResponse, error) {
	var out AuthorizeResponse
	if err := c.post(ctx, "/api/device/authorize", req, &out); err != nil {
		return nil, err
	}

	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return nil, errors.New("cloud-api: incomplete device authorization response")
	}

	return &out, nil
}

// Token polls once for the device-flow result.
func (c *Client) Token(ctx context.Context, deviceCode string) (*TokenResponse, error) {
	var out TokenResponse
	body := map[string]string{"grant_type": GrantTypeDeviceCode, "device_code": deviceCode}
	if err := c.post(ctx, "/api/device/token", body, &out); err != nil {
		return nil, err
	}

	if missing := missingFields(map[string]string{
		"agent_id": string(out.AgentID), "access_token": out.AccessToken,
		"refresh_token": out.RefreshToken, "gateway_url": out.GatewayURL,
	}); missing != "" {
		return nil, fmt.Errorf("%w: token response is missing %s", ErrBadResponse, missing)
	}

	if !validID(string(out.AgentID)) || !validID(string(out.TenantID)) {
		return nil, fmt.Errorf("%w: agent_id and tenant_id must be 1-64 letters, digits, - or _", ErrBadResponse)
	}

	return &out, nil
}

// Refresh exchanges a refresh token for a new pair.
func (c *Client) Refresh(ctx context.Context, req RefreshRequest) (*RefreshResponse, error) {
	req.GrantType = GrantTypeRefreshToken

	var out RefreshResponse
	if err := c.post(ctx, "/api/device/refresh", req, &out); err != nil {
		return nil, err
	}

	if out.AccessToken == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("%w: refresh response is missing access_token or refresh_token", ErrBadResponse)
	}

	return &out, nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}

	u := c.base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}

	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, apiErr)
		if apiErr.Code == "" {
			apiErr.Code = http.StatusText(resp.StatusCode)
		}

		return apiErr
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s: decode: %w", ErrBadResponse, path, err)
	}

	return nil
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validID reports whether a cloud-issued id is safe to store and to use in
// file names.
func validID(s string) bool { return idPattern.MatchString(s) }

// missingFields returns the sorted, comma-separated names of empty fields.
// Only names are returned, never values, so it is safe to log.
func missingFields(fields map[string]string) string {
	var names []string
	for name, v := range fields {
		if v == "" {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return strings.Join(names, ", ")
}
