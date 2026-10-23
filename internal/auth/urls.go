package auth

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

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

	a, g := api.Hostname(), gw.Hostname()
	if net.ParseIP(a) != nil || net.ParseIP(g) != nil || a == "localhost" || g == "localhost" {
		if !strings.EqualFold(a, g) {
			return nil, fmt.Errorf("gateway host %q must equal api host %q", g, a)
		}

		return gw, nil
	}

	ad, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(a))
	if err != nil {
		return nil, fmt.Errorf("api host %q: %w", a, err)
	}

	gd, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(g))
	if err != nil {
		return nil, fmt.Errorf("gateway host %q: %w", g, err)
	}

	if ad != gd {
		return nil, fmt.Errorf("gateway domain %q is not under api domain %q", gd, ad)
	}

	return gw, nil
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
