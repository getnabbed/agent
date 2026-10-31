package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIsLocal(t *testing.T) {
	local := []string{
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "127.0.0.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "255.255.255.255", "224.0.0.251",
		"::1", "fe80::1", "fd12:3456::1", "::ffff:192.168.0.1", "64:ff9b::a00:1", "::",
		// Local-use NAT64 maps IPv4 at a prefix length the network picks.
		"64:ff9b:1::a00:1", "64:ff9b:1:a00:1::", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff", "fec0::1",
		// A zone scopes an address to one interface, whatever its bits.
		"::1%lo", "fe80::1%eth0", "2606:4700::1111%eth0",
	}

	public := []string{"1.1.1.1", "8.8.8.8", "172.32.0.1", "100.128.0.1", "2606:4700::1111", "64:ff9b::808:808"}

	for _, s := range local {
		if !IsLocal(netip.MustParseAddr(s)) {
			t.Errorf("%s should be local", s)
		}
	}

	for _, s := range public {
		if IsLocal(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
}

func TestDialerBlocksLoopbackByDefault(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = ln.Close() }()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// "localhost" resolves to a blocked address: the check runs after DNS.
	addr := net.JoinHostPort("localhost", strconv.Itoa(int(netip.MustParseAddrPort(ln.Addr().String()).Port())))

	if _, err := (Policy{}).Dialer(nil)(ctx, "tcp", ln.Addr().String()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial loopback err = %v, want ErrBlocked", err)
	}

	if _, err := (Policy{}).Dialer(nil)(ctx, "tcp", addr); !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial localhost err = %v, want ErrBlocked", err)
	}

	c, err := (Policy{AllowLAN: true}).Dialer(nil)(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("AllowLAN dial: %v", err)
	}

	_ = c.Close()
}

func TestPacketConnBlocksLocalDestinations(t *testing.T) {
	pc, err := (Policy{}).ListenPacket()("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = pc.Close() }()

	dst := &net.UDPAddr{IP: net.ParseIP("192.168.1.1"), Port: 6969}
	if _, err := pc.WriteTo([]byte("x"), dst); !errors.Is(err, ErrBlocked) {
		t.Fatalf("WriteTo LAN err = %v, want ErrBlocked", err)
	}
}

// loopback returns the host's loopback interface.
func loopback(t *testing.T) net.Interface {
	t.Helper()

	ifs, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot list interfaces: %v", err)
	}

	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagUp != 0 {
			return i
		}
	}

	t.Skip("no loopback interface")

	return net.Interface{}
}

// A zone in a dial address once made loopback look public.
func TestDialerBlocksZonedLoopback(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}

	// Nothing needs to accept: the kernel completes handshakes into the
	// listen backlog, so a dial the guard lets through succeeds.
	defer func() { _ = ln.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	port := strconv.Itoa(int(netip.MustParseAddrPort(ln.Addr().String()).Port()))

	// The loopback interface is lo on Linux, lo0 on macOS and
	// "Loopback Pseudo-Interface 1" on Windows; zone it by name and index.
	lo := loopback(t)

	for _, host := range []string{"::1%" + lo.Name, "::1%" + strconv.Itoa(lo.Index)} {
		c, err := (Policy{}).Dialer(nil)(ctx, "tcp", net.JoinHostPort(host, port))
		if !errors.Is(err, ErrBlocked) {
			if c != nil {
				_ = c.Close()
			}

			t.Fatalf("dial %s: got %v, want ErrBlocked", host, err)
		}
	}
}

func TestParseRanges(t *testing.T) {
	got, err := ParseRanges([]string{"192.168.1.0/24", " 10.0.0.5 ", "", "fd00:1::/64", "::1", "::ffff:192.168.2.7", "::ffff:192.168.3.0/120"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"192.168.1.0/24", "10.0.0.5/32", "fd00:1::/64", "::1/128", "192.168.2.7/32", "192.168.3.0/24"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	for bad, hint := range map[string]string{
		"192.168.1.10/24": "write 192.168.1.0/24 for the network, or 192.168.1.10/32 for one address",
		"nas.lan":         "not an IP address",
		"10.0.0.0/33":     "not a CIDR range",
		"fe80::1%eth0":    "zone",
		"fe80::%eth0/64":  "not a CIDR range",
	} {
		if _, err := ParseRanges([]string{bad}); err == nil || !strings.Contains(err.Error(), hint) {
			t.Errorf("%q: got %v, want an error mentioning %q", bad, err, hint)
		}
	}
}

func TestPolicyAllowsOwnerRanges(t *testing.T) {
	allow, err := ParseRanges([]string{"192.168.1.0/24", "fd00:1::/64", "64:ff9b:1::/48", "fe80::/10"})
	if err != nil {
		t.Fatal(err)
	}

	p := Policy{Allow: allow}

	allowed := []string{
		"192.168.1.20", "::ffff:192.168.1.20", "64:ff9b::c0a8:114", // the same host, mapped and through NAT64
		"fd00:1::5", "64:ff9b:1:abcd::1", "fe80::1%eth0", "1.1.1.1",
	}

	blocked := []string{"192.168.2.1", "10.0.0.1", "fd00:2::5", "::1", "::1%lo", "169.254.169.254"}

	for _, s := range allowed {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}

	for _, s := range blocked {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
}
