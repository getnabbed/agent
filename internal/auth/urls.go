package auth

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/publicsuffix"
)

// ParseAPIURL validates the control-plane URL the user configured. It must
// be https, except plain http to a loopback host for local development.
func ParseAPIURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid api url: %w", err)
	}

	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid api url %q: want https://host[:port][/path]", raw)
	}

	if err := checkScheme(u); err != nil {
		return nil, err
	}

	// Canonical lower case: the host is signed into every refresh proof and
	// must match how the server canonicalises it.
	u.Host = strings.ToLower(u.Host)

	return u, nil
}

// ValidateGatewayURL checks the gateway_url returned at pairing against the
// configured api_url, so a compromised or spoofed token endpoint cannot
// steer the agent to an attacker's gateway. The two must share a registrable
// domain (eTLD+1), or be the same host for IPs and loopback.
func ValidateGatewayURL(api *url.URL, raw string) (*url.URL, error) {
	gw, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway url: %w", err)
	}

	if gw.Host == "" || gw.User != nil {
		return nil, fmt.Errorf("invalid gateway url %q", raw)
	}

	if err := checkScheme(gw); err != nil {
		return nil, err
	}

	if gw.Scheme != api.Scheme {
		return nil, fmt.Errorf("gateway url scheme %q does not match api url scheme %q", gw.Scheme, api.Scheme)
	}

	if err := sameSite(api, gw); err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}

	return gw, nil
}

// ValidateVerificationURI checks the device-flow link shown to the user:
// it must be https (http only for loopback dev) on the API's own site, so
// a compromised token endpoint cannot send the user to a phishing page.
func ValidateVerificationURI(api *url.URL, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return fmt.Errorf("invalid verification uri %q", printable(raw))
	}

	if !u.IsAbs() || u.Host == "" {
		// The agent cannot guess which host serves the page: it is often the
		// web app, not the API. RFC 8628 section 3.2 requires an absolute URI.
		return fmt.Errorf("verification uri %q is not absolute; the server must send a full https:// URL (RFC 8628 section 3.2)", printable(raw))
	}

	if err := checkScheme(u); err != nil {
		return err
	}

	return sameSite(api, u)
}

// sameSite requires u to share api's registrable domain (eTLD+1), or to be
// the same host for IPs and localhost.
func sameSite(api, u *url.URL) error {
	a, h := strings.ToLower(api.Hostname()), strings.ToLower(u.Hostname())
	if net.ParseIP(a) != nil || net.ParseIP(h) != nil || a == "localhost" || h == "localhost" {
		if a != h {
			return fmt.Errorf("host %q must equal api host %q", h, a)
		}

		return nil
	}

	ad, err := publicsuffix.EffectiveTLDPlusOne(a)
	if err != nil {
		return fmt.Errorf("api host %q: %w", a, err)
	}

	hd, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return fmt.Errorf("host %q: %w", h, err)
	}

	if ad != hd {
		return fmt.Errorf("domain %q is not under api domain %q", hd, ad)
	}

	return nil
}

func checkScheme(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopback(u.Hostname()) {
			return nil
		}

		return errors.New("plain http is only allowed for loopback hosts; use https")
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
}

// IsLoopback reports whether host is localhost or a loopback IP.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// printable makes a server-supplied value safe to show in an error: no
// control or format characters, bounded length.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}

		return r
	}, s)

	if len(s) > 200 {
		s = s[:200] + "..."
	}

	return s
}
