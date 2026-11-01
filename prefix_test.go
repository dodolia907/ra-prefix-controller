package main

import (
	"net/netip"
	"testing"
)

func addr(s string, bits, preferred int, dynamic bool) raAddr {
	return raAddr{ip: netip.MustParseAddr(s), bits: bits, preferred: preferred, dynamic: dynamic}
}

func TestSelectPrefix(t *testing.T) {
	tests := []struct {
		name  string
		addrs []raAddr
		want  string
	}{
		{"slaac", []raAddr{addr("2001:db8:1:2:aaaa::1", 64, 3600, true)}, "2001:db8:1:2::/64"},
		{"ignores ula and link-local", []raAddr{addr("fd00:20::1", 64, 3600, true), addr("fe80::1", 64, 3600, true)}, ""},
		{"ignores static gua", []raAddr{addr("2001:db8::1", 64, 3600, false)}, ""},
		{"ignores non /64", []raAddr{addr("2001:db8::1", 128, 3600, true)}, ""},
		{"renumbering skips deprecated", []raAddr{addr("2001:db8:1::1", 64, 0, true), addr("2001:db8:2::1", 64, 1800, true)}, "2001:db8:2::/64"},
		{"longest preferred wins", []raAddr{addr("2001:db8:1::1", 64, 100, true), addr("2001:db8:2::1", 64, 1800, true)}, "2001:db8:2::/64"},
		{"nothing", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := selectPrefix(tt.addrs)
			if tt.want == "" {
				if ok {
					t.Fatalf("got %s, want none", got)
				}
				return
			}
			if !ok || got.String() != tt.want {
				t.Fatalf("got %s (%v), want %s", got, ok, tt.want)
			}
		})
	}
}

func TestPoolCIDR(t *testing.T) {
	got := poolCIDR(netip.MustParsePrefix("2001:db8:1:2::/64"), netip.MustParseAddr("::beef:0"), 112)
	if want := "2001:db8:1:2::beef:0/112"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
