package resource

import "testing"

// TestSplitPortMapping pins the accepted syntax. The rejection cases matter
// as much as the accepted ones: this parser decides what a "private"
// listener is allowed to say, and a mapping that parses loosely here binds
// wider than the operator wrote (#41).
func TestSplitPortMapping(t *testing.T) {
	for _, tc := range []struct {
		in        string
		ip        string
		host, ctr uint32
	}{
		{"8443:80", "", 8443, 80},
		{"127.0.0.1:8443:80", "127.0.0.1", 8443, 80},
		{"100.64.0.1:8443:80", "100.64.0.1", 8443, 80},
		{"0.0.0.0:80:80", "0.0.0.0", 80, 80},
		{"[fd7a:115c:a1e0::1]:8443:80", "fd7a:115c:a1e0::1", 8443, 80},
		{"[::1]:8443:80", "::1", 8443, 80},
		{"1:65535", "", 1, 65535},
	} {
		ip, host, ctr, ok := SplitPortMapping(tc.in)
		if !ok || ip != tc.ip || host != tc.host || ctr != tc.ctr {
			t.Errorf("SplitPortMapping(%q) = (%q, %d, %d, %v), want (%q, %d, %d, true)",
				tc.in, ip, host, ctr, ok, tc.ip, tc.host, tc.ctr)
		}
	}

	for _, in := range []string{
		"",
		"80",
		"80:",
		":80",
		"8443:80:80:80",
		"0:80",              // port 0 means "pick one", which a spec must not do
		"8443:65536",        // out of range
		"-1:80",             // ParseUint rejects the sign
		"80:8o",             // not a number
		"localhost:8443:80", // a name, not an address: resolution is not the spec's to defer
		"caddy:8443:80",
		"::1:8443:80",  // bare IPv6 — ambiguous, brackets required
		"[::1]8443:80", // missing the "]:" separator
		"[::1]:80",     // bracketed address with only one port
		"[:8443:80",
	} {
		if ip, host, ctr, ok := SplitPortMapping(in); ok {
			t.Errorf("SplitPortMapping(%q) = (%q, %d, %d, true), want ok=false", in, ip, host, ctr)
		}
	}
}

// TestValidatePortMappingUsesSplit checks the door and the renderer agree:
// a mapping that validates must be one the reconciler can parse. Drift
// between the two is what let host_ip sit unusable in the proto.
func TestValidatePortMappingUsesSplit(t *testing.T) {
	for _, in := range []string{"8443:80", "127.0.0.1:8443:80", "[::1]:8443:80", "localhost:80:80", "80"} {
		r := &Resource{Kind: KindContainer, Metadata: Metadata{Name: "c"}}
		_, _, _, ok := SplitPortMapping(in)
		if err := validatePortMapping(r, in); (err == nil) != ok {
			t.Errorf("%q: validate err=%v but SplitPortMapping ok=%v", in, err, ok)
		}
	}
}
