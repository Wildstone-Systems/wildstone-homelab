package pkgspec

import (
	"strings"
	"testing"
)

// TestValidateParamValueEnumAccepts: a value named in the enum passes,
// exactly like any other shape.
func TestValidateParamValueEnumAccepts(t *testing.T) {
	for _, v := range []string{"platform-only", "sandboxed", "host-admin"} {
		if err := ValidateParamValue("enum:platform-only|sandboxed|host-admin", v); err != nil {
			t.Errorf("value %q: unexpected error: %v", v, err)
		}
	}
}

// TestValidateParamValueEnumRejectsAndListsValues: a value outside the
// closed choice fails, and the error names every allowed value — the
// requirement H2 exists for. A household that mistypes "sandbox" for
// "sandboxed" should be told what it could have written, not just that
// what it wrote was wrong.
func TestValidateParamValueEnumRejectsAndListsValues(t *testing.T) {
	err := ValidateParamValue("enum:platform-only|sandboxed|host-admin", "sandbox")
	if err == nil {
		t.Fatal("expected an error for a value outside the enum")
	}
	for _, want := range []string{"platform-only", "sandboxed", "host-admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name allowed value %q", err.Error(), want)
		}
	}
}

// TestValidateParamValueEnumEmptyValuePasses mirrors the rule every other
// shape follows: whether a param may be empty is `required`'s question,
// not the shape's.
func TestValidateParamValueEnumEmptyValuePasses(t *testing.T) {
	if err := ValidateParamValue("enum:a|b", ""); err != nil {
		t.Errorf("empty value should pass regardless of shape: %v", err)
	}
}

// TestValidateParamValueExistingShapesUnaffected pins the pre-H2 shapes:
// adding the enum form must not change how a fixed-vocabulary shape is
// looked up or reported.
func TestValidateParamValueExistingShapesUnaffected(t *testing.T) {
	if err := ValidateParamValue("iana_timezone", "Europe/London"); err != nil {
		t.Errorf("iana_timezone regressed: %v", err)
	}
	if err := ValidateParamValue("iana_timezone", "CST"); err == nil {
		t.Error("iana_timezone stopped rejecting an abbreviation")
	}
	if err := ValidateParamValue("port", "8080"); err != nil {
		t.Errorf("port regressed: %v", err)
	}
	err := ValidateParamValue("not-a-real-shape", "x")
	if err == nil {
		t.Fatal("expected an error for an unknown shape")
	}
	if !strings.Contains(err.Error(), "enum:a|b|c") {
		t.Errorf("unknown-shape error should mention the enum form as an option: %q", err.Error())
	}
}

// TestPrivateIPv4Shape: a LAN or tailnet listener gets one private
// address, and each thing an operator might type instead — every
// interface, loopback, a public address, a name, IPv6 — is refused with
// a reason.
func TestPrivateIPv4Shape(t *testing.T) {
	for _, v := range []string{"192.168.1.10", "10.0.0.2", "172.16.5.4", "172.31.255.1", "100.64.0.1"} {
		if err := ValidateParamValue("private_ipv4", v); err != nil {
			t.Errorf("%s: unexpected error: %v", v, err)
		}
	}
	for v, want := range map[string]string{
		"0.0.0.0":       "every interface",
		"127.0.0.1":     "loopback",
		"8.8.8.8":       "not a private address",
		"172.32.0.1":    "not a private address",
		"broker.local":  "not an IPv4 address",
		"fd00::1":       "not an IPv4 address",
		"192.168.1.10:": "not an IPv4 address",
	} {
		err := ValidateParamValue("private_ipv4", v)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error containing %q, got %v", v, want, err)
		}
	}
}

// TestEnumShapeValues pins the parsing helper directly: the prefix, the
// split, and that a bare "enum" (no colon) is not the enum form at all —
// it falls through to the ordinary closed-vocabulary lookup and fails
// there, the same as any other misspelling.
func TestEnumShapeValues(t *testing.T) {
	values, ok := enumShapeValues("enum:a|b|c")
	if !ok || len(values) != 3 || values[0] != "a" || values[1] != "b" || values[2] != "c" {
		t.Errorf("enumShapeValues(enum:a|b|c) = %v, %v", values, ok)
	}
	if _, ok := enumShapeValues("enum"); ok {
		t.Error(`"enum" with no colon must not parse as the enum form`)
	}
	if _, ok := enumShapeValues("port"); ok {
		t.Error(`"port" must not parse as the enum form`)
	}
}

// TestValidateEnumShapeDecl pins the package-lint-time structural checks:
// at least one value, none blank, none repeated.
func TestValidateEnumShapeDecl(t *testing.T) {
	if err := validateEnumShapeDecl([]string{"a", "b"}); err != nil {
		t.Errorf("well-formed enum rejected: %v", err)
	}
	for name, values := range map[string][]string{
		"empty (validate: enum:)": {""},
		"blank between pipes":     {"a", "", "b"},
		"duplicate value":         {"a", "b", "a"},
	} {
		if err := validateEnumShapeDecl(values); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestInterfaceNameShape: a node interface name lands in a sysfs path and
// an upstream script's sed expression, so anything with a separator,
// whitespace or more than IFNAMSIZ-1 bytes is refused.
func TestInterfaceNameShape(t *testing.T) {
	for _, v := range []string{"eth0", "enp1s0", "br-lan", "eth0.100", "wlp2s0", "enx001122334455"} {
		if err := ValidateParamValue("interface_name", v); err != nil {
			t.Errorf("%s: unexpected error: %v", v, err)
		}
	}
	for _, v := range []string{"../eth0", "eth0/", "eth 0", "a-name-that-is-too-long", ".hidden", "eth0;reboot", "s/x/y/"} {
		if err := ValidateParamValue("interface_name", v); err == nil {
			t.Errorf("%q: accepted, want refused", v)
		}
	}
}

// TestSerialByIDShape: a radio is named by its stable alias, never by a
// plug-order tty and never by anything outside /dev/serial/by-id.
func TestSerialByIDShape(t *testing.T) {
	for _, v := range []string{
		"/dev/serial/by-id/usb-Nabu_Casa_Home_Assistant_Connect_ZBT-1_REPLACED-SERIAL-if00-port0",
		"/dev/serial/by-id/usb-SEGGER_J-Link_000683000000-if00",
	} {
		if err := ValidateParamValue("serial_by_id", v); err != nil {
			t.Errorf("%s: unexpected error: %v", v, err)
		}
	}
	for v, want := range map[string]string{
		"/dev/ttyUSB0":                  "plug order",
		"/dev/ttyACM0":                  "plug order",
		"/dev/serial/by-id/../../sda":   "not a /dev/serial/by-id/ path",
		"/dev/serial/by-id/":            "not a /dev/serial/by-id/ path",
		"/dev/serial/by-path/pci-0:1.0": "not a /dev/serial/by-id/ path",
		"/dev/serial/by-id/a/b":         "not a /dev/serial/by-id/ path",
		"tcp://192.168.1.5:6638":        "not a /dev/serial/by-id/ path",
	} {
		err := ValidateParamValue("serial_by_id", v)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want an error containing %q, got %v", v, want, err)
		}
	}
}
