package fetcher

import (
	"fmt"
	"net/netip"
)

// blockedPrefixes are CIDRs rejected beyond Go's netip helpers: CGNAT,
// documentation ranges, benchmarking, NAT64 translation, discard-only and
// IETF-reserved space (SPEC §6.2.2).
var blockedPrefixes = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT (RFC 6598), cloud metadata adjacents
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (incl. 255.255.255.255)
	// IPv6
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 translation (maps to v4 space)
	netip.MustParsePrefix("100::/64"),      // discard-only (RFC 6666)
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix("::ffff:0:0/96"), // IPv4-mapped (also handled via Unmap)
}

// CheckIP rejects any address that must never be dialed by the fetcher:
// loopback, link-local (including cloud metadata 169.254.169.254), private
// (RFC1918), CGNAT, ULA, multicast, and the explicit reserved table above.
func CheckIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return fmt.Errorf("invalid address")
	}
	if ip.Zone() != "" {
		return fmt.Errorf("zoned address not allowed")
	}
	switch {
	case ip.IsUnspecified(),
		ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsMulticast():
		return fmt.Errorf("%s is in a restricted range", ip)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return fmt.Errorf("%s is in blocked prefix %s", ip, p)
		}
	}
	return nil
}
