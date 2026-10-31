// Package netguard decides which IP addresses the agent may talk to on
// behalf of untrusted input, such as a torrent's peers, trackers and web
// seeds, or a usenet source configured from the web app. By default it
// refuses anything on the user's local network, so a hostile torrent or
// control plane cannot make the agent probe or attack LAN devices, the
// router, or cloud metadata endpoints. The host owner can allow chosen
// local ranges in the agent's own configuration (Policy.Allow).
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
	"strings"
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
	"64:ff9b:1::/48", // local-use NAT64 (RFC 8215); where it maps IPv4 varies, so all of it
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"fec0::/10",      // site-local, deprecated but still routed on some networks
	"ff00::/8",       // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}

	return out
}

// nat64 is the well-known NAT64 prefix (RFC 6052), which maps IPv4 into
// the low 32 bits; the embedded address is checked. Its local-use
// counterpart is blocked outright, in local.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// IsLocal reports whether ip is on a local, private or special-purpose
// network. An address with a zone, such as fe80::1%eth0 or ::1%lo, is
// scoped to one interface, so it is always local whatever its bits say.
func IsLocal(ip netip.Addr) bool {
	if ip.Zone() != "" {
		return true
	}

	ip = reached(ip)

	for _, p := range local {
		if p.Contains(ip) {
			return true
		}
	}

	return false
}

// reached returns the IPv4 address that ip stands for when it maps one
// (::ffff:a.b.c.d) or translates to one through well-known NAT64, and ip
// otherwise.
func reached(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()

	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}

	return ip
}

// Ranges returns the local ranges, for libraries that take a blocklist.
func Ranges() []netip.Prefix { return append([]netip.Prefix(nil), local...) }

// ParseRanges parses ranges for Policy.Allow: CIDR ranges such as
// 192.168.1.0/24 or fd00:1::/64, or single addresses such as 192.168.1.10.
// Blank entries are skipped, and IPv4-mapped IPv6 forms become IPv4. A
// range with bits set past its length, such as 192.168.1.10/24, is refused,
// since it is unclear whether one address or the whole network was meant.
func ParseRanges(ss []string) ([]netip.Prefix, error) {
	var out []netip.Prefix

	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR range: %w", s, err)
			}

			if a.Zone() != "" {
				return nil, fmt.Errorf("%q has a zone; write the address without it", s)
			}

			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))

			continue
		}

		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR range: %w", s, err)
		}

		if m := p.Masked(); m != p {
			return nil, fmt.Errorf("%q has bits set past /%d: write %s for the network, or %s for one address",
				s, p.Bits(), m, netip.PrefixFrom(p.Addr(), p.Addr().BitLen()))
		}

		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}

		out = append(out, p)
	}

	return out, nil
}

// Policy is a connection policy.
type Policy struct {
	// AllowLAN permits local and private addresses.
	AllowLAN bool
	// Allow lists ranges the host owner lets the agent contact even though
	// they are local, such as a seedbox or a usenet server on the LAN. It
	// comes from the agent's own configuration, never from the cloud. An
	// IPv4 range also covers its addresses reached through IPv4-mapped or
	// well-known NAT64 IPv6 addresses.
	Allow []netip.Prefix
}

// Allowed reports whether ip may be contacted.
func (p Policy) Allowed(ip netip.Addr) bool {
	return p.AllowLAN || !IsLocal(ip) || p.allows(ip)
}

// allows reports whether ip is in one of the owner's ranges.
func (p Policy) allows(ip netip.Addr) bool {
	ip = ip.WithZone("")
	via := reached(ip)

	for _, r := range p.Allow {
		if r.Contains(ip) || r.Contains(via) {
			return true
		}
	}

	return false
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
