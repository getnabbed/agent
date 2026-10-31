package torrent

import (
	"net"
	"net/netip"
	"testing"

	"github.com/getnabbed/agent/internal/netguard"
)

func TestLANBlocklistHonoursAllowedRanges(t *testing.T) {
	b := lanBlocklist{policy: netguard.Policy{Allow: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}}}

	for ip, blocked := range map[string]bool{
		"192.168.1.20": false, // the owner's range
		"192.168.2.20": true,
		"10.0.0.1":     true,
		"1.1.1.1":      false,
	} {
		// anacrolix passes 16-byte forms for IPv4 peers too.
		for _, form := range []net.IP{net.ParseIP(ip), net.ParseIP(ip).To4()} {
			if _, got := b.Lookup(form); got != blocked {
				t.Errorf("%s (%d bytes): got blocked = %v, want %v", ip, len(form), got, blocked)
			}
		}
	}
}
