// Package netguard decides which IP addresses the agent may talk to on
// behalf of untrusted input, such as a torrent's peers, trackers and web
// seeds. By default it refuses anything on the user's local network, so a
// hostile torrent cannot make the agent probe or attack LAN devices, the
// router, or cloud metadata endpoints.
//
// Checks run against the address actually being connected to, after DNS
// resolution, so a hostname that resolves to a private address (DNS
// rebinding) is caught too.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ErrBlocked is returned when a connection is refused by policy.
var ErrBlocked = errors.New("netguard: destination is on a local network and LAN access is disabled")

// local lists every range treated as "the user's network" rather than the
// public internet.
var local = mustPrefixes(
	"0.0.0.0/8",      // "this network"
	"10.0.0.0/8",     // RFC 1918
	"100.64.0.0/10",  // carrier-grade NAT, also Tailscale
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, includes cloud metadata 169.254.169.254
	"172.16.0.0/12",  // RFC 1918
	"192.0.0.0/24",   // IETF protocol assignments
	"192.168.0.0/16", // RFC 1918
	"198.18.0.0/15",  // benchmarking, sometimes used internally
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, includes 255.255.255.255
	"::/128",         // unspecified
	"::1/128",        // loopback
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"ff00::/8",       // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}

	return out
}

// nat64 maps IPv4 into IPv6 (RFC 6052); the embedded address is checked.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// IsLocal reports whether ip is on a local, private or special-purpose
// network.
func IsLocal(ip netip.Addr) bool {
	ip = ip.Unmap()

	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}

	for _, p := range local {
		if p.Contains(ip) {
			return true
		}
	}

	return false
}

// Ranges returns the local ranges, for libraries that take a blocklist.
func Ranges() []netip.Prefix { return append([]netip.Prefix(nil), local...) }

// Policy is a connection policy.
type Policy struct {
	// AllowLAN permits local and private addresses.
	AllowLAN bool
}

// Allowed reports whether ip may be contacted.
func (p Policy) Allowed(ip netip.Addr) bool {
	return p.AllowLAN || !IsLocal(ip)
}

func (p Policy) check(address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("netguard: unexpected address %q: %w", address, err)
	}

	if !p.Allowed(ap.Addr()) {
		return fmt.Errorf("%w (%s)", ErrBlocked, ap.Addr())
	}

	return nil
}

// Dialer returns a DialContext that refuses blocked destinations at
// connect time. base may be nil.
func (p Policy) Dialer(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{}
	if base != nil {
		d = *base
	}

	prev := d.ControlContext
	d.ControlContext = func(ctx context.Context, network, address string, c syscall.RawConn) error {
		if err := p.check(address); err != nil {
			return err
		}

		if prev != nil {
			return prev(ctx, network, address, c)
		}

		return nil
	}

	return d.DialContext
}

// ListenPacket returns a ListenPacket whose connections refuse to send to
// blocked destinations.
func (p Policy) ListenPacket() func(network, addr string) (net.PacketConn, error) {
	return func(network, addr string) (net.PacketConn, error) {
		pc, err := net.ListenPacket(network, addr)
		if err != nil {
			return nil, err
		}

		return &guardedPacketConn{PacketConn: pc, p: p}, nil
	}
}

type guardedPacketConn struct {
	net.PacketConn

	p Policy
}

func (g *guardedPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return 0, fmt.Errorf("netguard: unexpected address %q: %w", addr, err)
	}

	if !g.p.Allowed(ap.Addr()) {
		return 0, fmt.Errorf("%w (%s)", ErrBlocked, ap.Addr())
	}

	return g.PacketConn.WriteTo(b, addr)
}
