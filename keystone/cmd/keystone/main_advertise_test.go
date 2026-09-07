package main

import "testing"

// The gateway and the public API listen separately, and -api-listen's
// documented default is loopback. Advertising a routable SAN while the
// API answers only on 127.0.0.1 would prefill a keystone_url nothing is
// listening on, and an adapter installed with it could never check in.
func TestAdvertisedAPIURLStaysSilentWhenLoopbackBound(t *testing.T) {
	hosts := "localhost,127.0.0.1,192.168.1.52,100.64.0.59"
	for _, listen := range []string{"127.0.0.1:9441", "localhost:9441", "[::1]:9441"} {
		if got := advertisedAPIURL(hosts, listen); got != "" {
			t.Errorf("advertisedAPIURL(_, %q) = %q, want empty", listen, got)
		}
	}
	// A wildcard bind answers on every SAN, so it advertises the first
	// routable one — the same host the pairing command already names.
	for _, listen := range []string{"0.0.0.0:9441", "[::]:9441", ":9441"} {
		if got := advertisedAPIURL(hosts, listen); got != "http://192.168.1.52:9441" {
			t.Errorf("advertisedAPIURL(_, %q) = %q", listen, got)
		}
	}
	if got := advertisedAPIURL(hosts, "192.168.1.52:9441"); got != "http://192.168.1.52:9441" {
		t.Errorf("specific bind = %q", got)
	}
	// No routable SAN means nothing to advertise.
	if got := advertisedAPIURL("localhost,127.0.0.1", "0.0.0.0:9441"); got != "" {
		t.Errorf("no routable SAN = %q, want empty", got)
	}
}

// The gateway keeps its existing behaviour: the two share the host
// selection, not the loopback rule.
func TestAdvertisedGatewayAddrUnchanged(t *testing.T) {
	if got := advertisedGatewayAddr("localhost,192.168.1.52", "0.0.0.0:9440"); got != "192.168.1.52:9440" {
		t.Fatalf("gateway addr = %q", got)
	}
}
