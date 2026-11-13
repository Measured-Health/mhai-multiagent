package main

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// Review item 1: outside development the proxy refuses to start without
// the internal networks (the backend subnet and the host's addresses):
// bridge isolation alone must not be what keeps it off them.
func TestSettingsRequireInternalOutsideDevelopment(t *testing.T) {
	base := map[string]string{"EGRESS_EMR_ALLOW_LIST": "/emr-egress/allow.json", "EGRESS_EMR_KEY_PATH": "/k"}
	get := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		}
	}
	for _, tc := range []struct {
		env  map[string]string
		fail string
	}{
		{map[string]string{}, "EGRESS_EMR_INTERNAL_CIDRS"},
		{map[string]string{"EGRESS_EMR_ENV": "production"}, "EGRESS_EMR_INTERNAL_CIDRS"},
		{map[string]string{"EGRESS_EMR_ENV": "staging", "EGRESS_EMR_INTERNAL_CIDRS": " , "}, "EGRESS_EMR_INTERNAL_CIDRS"},
		{map[string]string{"EGRESS_EMR_ENV": "dev"}, "EGRESS_EMR_ENV"},
		{map[string]string{"EGRESS_EMR_ENV": "development"}, ""},
		{map[string]string{"EGRESS_EMR_INTERNAL_CIDRS": "172.19.0.0/16, 10.0.3.15/32"}, ""},
	} {
		s, err := readSettings(get(tc.env))
		switch {
		case tc.fail == "" && err != nil:
			t.Errorf("%v: %v", tc.env, err)
		case tc.fail != "" && (err == nil || !strings.Contains(err.Error(), tc.fail)):
			t.Errorf("%v: %v, want an error naming %s", tc.env, err, tc.fail)
		case tc.fail == "" && tc.env["EGRESS_EMR_INTERNAL_CIDRS"] != "" &&
			!slices.Equal(s.internal, []netip.Prefix{netip.MustParsePrefix("172.19.0.0/16"), netip.MustParsePrefix("10.0.3.15/32")}):
			t.Errorf("internal %v", s.internal)
		}
	}
}

// Review item 1: the gateways in the routing table (the host's side of
// each bridge the proxy is on) are found at start and refused as single
// addresses.
func TestGatewaysFromRouteTable(t *testing.T) {
	const route = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0100190A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t0000190A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"eth1\t000014AC\t010014AC\t0003\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	got := routeGateways(strings.NewReader(route))
	want := []netip.Prefix{netip.MustParsePrefix("10.25.0.1/32"), netip.MustParsePrefix("172.20.0.1/32")}
	if !slices.Equal(got, want) {
		t.Errorf("gateways %v, want %v", got, want)
	}
	if g := routeGateways(strings.NewReader("garbage\nx y z\n")); len(g) != 0 {
		t.Errorf("garbage gave %v", g)
	}
}

// The end-to-end run's allowance (plan Task 25): EGRESS_EMR_DEV_EXEMPT
// names the synthetic server's network, which the proxy shares; in
// development only, it is taken out of the proxy's own networks (so a
// connection that declares it may reach it). It is an RFC 1918 network of
// at most a /24, never one of the configured internal networks, and never
// exempts a gateway.
func TestDevExemptOnlyInDevelopment(t *testing.T) {
	base := map[string]string{"EGRESS_EMR_ALLOW_LIST": "/a", "EGRESS_EMR_KEY_PATH": "/k", "EGRESS_EMR_ENV": "development",
		"EGRESS_EMR_INTERNAL_CIDRS": "10.233.0.0/24"}
	get := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		}
	}
	s, err := readSettings(get(map[string]string{"EGRESS_EMR_DEV_EXEMPT": "10.233.5.0/24"}))
	if err != nil || s.devExempt != netip.MustParsePrefix("10.233.5.0/24") {
		t.Fatalf("development: %v %v", s.devExempt, err)
	}
	for name, extra := range map[string]map[string]string{
		"staging":    {"EGRESS_EMR_ENV": "staging", "EGRESS_EMR_DEV_EXEMPT": "10.233.5.0/24"},
		"production": {"EGRESS_EMR_ENV": "production", "EGRESS_EMR_DEV_EXEMPT": "10.233.5.0/24"},
		"public":     {"EGRESS_EMR_DEV_EXEMPT": "8.8.8.0/24"},
		"too wide":   {"EGRESS_EMR_DEV_EXEMPT": "10.233.0.0/16"},
		"internal":   {"EGRESS_EMR_DEV_EXEMPT": "10.233.0.0/24"},
		"junk":       {"EGRESS_EMR_DEV_EXEMPT": "nope"},
		"loopback":   {"EGRESS_EMR_DEV_EXEMPT": "127.0.0.0/24"},
	} {
		if _, err := readSettings(get(extra)); err == nil || !strings.Contains(err.Error(), "EGRESS_EMR_DEV_EXEMPT") {
			t.Errorf("%s: %v", name, err)
		}
	}
	own := []netip.Prefix{netip.MustParsePrefix("10.233.1.0/24"), netip.MustParsePrefix("10.233.5.0/24"), netip.MustParsePrefix("10.0.0.0/8")}
	got := exemptOwn(own, netip.MustParsePrefix("10.233.5.0/24"))
	if want := []netip.Prefix{own[0], own[2]}; !slices.Equal(got, want) {
		t.Fatalf("exempted %v, want %v (a wider own network stays)", got, want)
	}
	if got := exemptOwn(own, netip.Prefix{}); !slices.Equal(got, own) {
		t.Fatalf("no exemption changed %v", got)
	}
}
