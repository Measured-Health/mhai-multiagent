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
