package main

import (
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func linkAddrs(link netlink.Link) ([]raAddr, error) {
	list, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	out := make([]raAddr, 0, len(list))
	for _, a := range list {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		bits, _ := a.Mask.Size()
		out = append(out, raAddr{
			ip:        ip.Unmap(),
			bits:      bits,
			preferred: a.PreferedLft,
			dynamic:   a.Flags&unix.IFA_F_PERMANENT == 0,
		})
	}
	return out, nil
}
