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
	"time"
)

const maxResponseBytes = 1 << 20

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
	switch e.Code {
	case "invalid_grant", "access_denied", "expired_token", "invalid_client", "unauthorized_client", "agent_revoked":
		return true
	}

	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
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
	AgentID      string `json:"agent_id"`
	TenantID     string `json:"tenant_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	GatewayURL   string `json:"gateway_url"`
}

// RefreshRequest is the body of POST /api/device/refresh. Signature is
// base64url of Ed25519 over identity.RefreshSigningInput.
type RefreshRequest struct {
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
	if err := c.post(ctx, "/api/device/token", map[string]string{"device_code": deviceCode}, &out); err != nil {
		return nil, err
	}

	if out.AgentID == "" || out.AccessToken == "" || out.RefreshToken == "" || out.GatewayURL == "" {
		return nil, errors.New("cloud-api: incomplete token response")
	}

	return &out, nil
}

// Refresh exchanges a refresh token for a new pair.
func (c *Client) Refresh(ctx context.Context, req RefreshRequest) (*RefreshResponse, error) {
	var out RefreshResponse
	if err := c.post(ctx, "/api/device/refresh", req, &out); err != nil {
		return nil, err
	}

	if out.AccessToken == "" || out.RefreshToken == "" {
		return nil, errors.New("cloud-api: incomplete refresh response")
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
		return fmt.Errorf("cloud-api %s: decode response: %w", path, err)
	}

	return nil
}
