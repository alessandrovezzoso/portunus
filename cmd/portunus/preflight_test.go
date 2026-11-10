package main

import (
	"net/netip"
	"slices"
	"testing"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestNonPrivate(t *testing.T) {
	tests := []struct {
		name string
		in   []netip.Addr
		want []netip.Addr
	}{
		{"rfc1918 10/8", addrs("10.1.2.3"), nil},
		{"rfc1918 172.16/12", addrs("172.20.0.1"), nil},
		{"rfc1918 192.168/16", addrs("192.168.1.10"), nil},
		{"ipv6 ula", addrs("fd00::10"), nil},
		{"public", addrs("8.8.8.8"), addrs("8.8.8.8")},
		{"documentation range is not private", addrs("192.0.2.10"), addrs("192.0.2.10")},
		{"loopback is not private", addrs("127.0.0.1"), addrs("127.0.0.1")},
		{"cgnat is not private", addrs("100.64.0.1"), addrs("100.64.0.1")},
		{"mixed", addrs("192.168.1.10", "203.0.113.5"), addrs("203.0.113.5")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nonPrivate(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("nonPrivate(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCheckLocal(t *testing.T) {
	local := addrs("127.0.0.1", "192.168.1.20", "fe80::1")

	if err := checkLocal(netip.MustParseAddr("192.168.1.20"), local); err != nil {
		t.Errorf("an assigned address: unexpected error %v", err)
	}
	if err := checkLocal(netip.MustParseAddr("192.168.1.99"), local); err == nil {
		t.Error("an address of another host: expected an error")
	}
}
