package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestIsLocal(t *testing.T) {
	local := []string{
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "127.0.0.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "255.255.255.255", "224.0.0.251",
		"::1", "fe80::1", "fd12:3456::1", "::ffff:192.168.0.1", "64:ff9b::a00:1", "::",
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
