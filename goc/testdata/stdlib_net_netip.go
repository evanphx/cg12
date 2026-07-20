package main

import "net/netip"

func main() {
	addr, err := netip.ParseAddr("2001:db8::1")
	if err != nil {
		panic("netip ParseAddr failed")
	}
	if !addr.Is6() {
		panic("netip address family mismatch")
	}
	if addr.String() != "2001:db8::1" {
		panic("netip String mismatch")
	}

	port, err := netip.ParseAddrPort("[2001:db8::1]:443")
	if err != nil {
		panic("netip ParseAddrPort failed")
	}
	if port.Addr() != addr {
		panic("netip AddrPort address mismatch")
	}
	if port.Port() != 443 {
		panic("netip AddrPort port mismatch")
	}

	prefix, err := netip.ParsePrefix("2001:db8::/32")
	if err != nil {
		panic("netip ParsePrefix failed")
	}
	if !prefix.Contains(addr) {
		panic("netip prefix containment mismatch")
	}
}
