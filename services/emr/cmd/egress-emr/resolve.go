package main

// The address check (EMR design §6.4, E3; plan Task 13): after the one
// lookup, every address in the answer must be one the named connection may
// reach. Public unicast addresses may be reached; an RFC 1918 address only
// inside the connection's own declared ranges; everything else never:
// loopback, unspecified, link-local (with the cloud metadata addresses),
// CGNAT, multicast, broadcast, documentation and benchmarking ranges, the
// IPv6 forms that carry an IPv4 address (NAT64, 6to4, Teredo,
// IPv4-compatible), ULA and anything else outside global unicast, and the
// internal networks (backend, egress-emr, emr-proxy) even when declared.

import (
	"context"
	"net"
	"net/netip"
)

// Resolver is the one lookup per CONNECT.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer dials the address that was checked, never a name.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// refusedV4 are never reached, declared or not.
var refusedV4 = mustPrefixes(
	"0.0.0.0/8",       // this network, unspecified
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, with the metadata address 169.254.169.254
	"100.64.0.0/10",   // CGNAT (shared address space)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, with broadcast 255.255.255.255
)

// hostSide are never reached, declared or not: Docker's default bridge,
// whose gateway is the host itself (docker0). The host's other addresses
// are found at start (routing-table gateways, the proxy's own networks)
// or named in EGRESS_EMR_INTERNAL_CIDRS, which is required outside
// development.
var hostSide = mustPrefixes("172.17.0.0/16")

// privateV4 are reached only inside the connection's declared ranges.
var privateV4 = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")

// globalV6 is the only IPv6 space reached at all; refusedV6 are the parts
// of it that embed IPv4 or are reserved.
var (
	globalV6  = netip.MustParsePrefix("2000::/3")
	refusedV6 = mustPrefixes(
		"2001::/23",     // IETF protocol assignments, with Teredo 2001::/32
		"2001:db8::/32", // documentation
		"2002::/16",     // 6to4
		"3fff::/20",     // documentation
	)
)

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// allowed reports whether a may be dialled for a connection with declared
// ranges, given the internal networks.
func allowed(a netip.Addr, declared, internal []netip.Prefix) bool {
	a = a.WithZone("").Unmap()
	if !a.IsValid() || inAny(a, internal) || inAny(a, hostSide) {
		return false
	}
	if a.Is4() {
		if inAny(a, refusedV4) {
			return false
		}
		if inAny(a, privateV4) {
			return inAny(a, declared)
		}
		return true
	}
	return globalV6.Contains(a) && !inAny(a, refusedV6)
}
