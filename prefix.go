package main

import "net/netip"

// raAddr is the part of an interface address that prefix selection needs.
type raAddr struct {
	ip        netip.Addr
	bits      int
	preferred int
	dynamic   bool
}

// selectPrefix returns the /64 of the SLAAC address with the longest preferred
// lifetime. After renumbering the old prefix stays on the link, deprecated,
// until its valid lifetime ends, so deprecated addresses are skipped.
func selectPrefix(addrs []raAddr) (netip.Prefix, bool) {
	global := netip.MustParsePrefix("2000::/3")
	var best raAddr
	found := false
	for _, a := range addrs {
		if !a.dynamic || a.bits != 64 || a.preferred <= 0 || !global.Contains(a.ip) {
			continue
		}
		if !found || a.preferred > best.preferred {
			best, found = a, true
		}
	}
	if !found {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(best.ip, 64).Masked(), true
}

// poolCIDR places hostPart in the interface identifier of a /64 prefix.
func poolCIDR(prefix netip.Prefix, hostPart netip.Addr, bits int) string {
	p, h := prefix.Addr().As16(), hostPart.As16()
	copy(p[8:], h[8:])
	return netip.PrefixFrom(netip.AddrFrom16(p), bits).Masked().String()
}
