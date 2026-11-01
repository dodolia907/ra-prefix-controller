package main

import "net/netip"

// switchMargin is in seconds. RFC 4861 caps the interval between unsolicited
// RAs at 1800 seconds, so prefixes that are still being advertised with equal
// lifetimes never drift further apart than this.
const switchMargin = 1800

// raAddr is the part of an interface address that prefix selection needs.
type raAddr struct {
	ip        netip.Addr
	bits      int
	preferred int
	dynamic   bool
}

// selectPrefix returns the /64 to use. Deprecated addresses are skipped because
// after renumbering the old prefix stays on the link until its valid lifetime
// ends. The /64 with the longest preferred lifetime wins, except that current
// is kept until another prefix leads it by more than switchMargin. Lifetimes
// count down between RAs, so without the margin two advertised prefixes would
// keep swapping places. Pass the zero Prefix when there is no current prefix.
func selectPrefix(addrs []raAddr, current netip.Prefix) (netip.Prefix, bool) {
	global := netip.MustParsePrefix("2000::/3")
	var best netip.Prefix
	bestLft, currentLft := 0, 0
	for _, a := range addrs {
		if !a.dynamic || a.bits != 64 || a.preferred <= 0 || !global.Contains(a.ip) {
			continue
		}
		prefix := netip.PrefixFrom(a.ip, 64).Masked()
		if a.preferred > bestLft {
			best, bestLft = prefix, a.preferred
		}
		if prefix == current && a.preferred > currentLft {
			currentLft = a.preferred
		}
	}
	if bestLft == 0 {
		return netip.Prefix{}, false
	}
	if currentLft > 0 && bestLft-currentLft <= switchMargin {
		return current, true
	}
	return best, true
}

// poolCIDR places hostPart in the interface identifier of a /64 prefix.
func poolCIDR(prefix netip.Prefix, hostPart netip.Addr, bits int) string {
	p, h := prefix.Addr().As16(), hostPart.As16()
	copy(p[8:], h[8:])
	return netip.PrefixFrom(netip.AddrFrom16(p), bits).Masked().String()
}
