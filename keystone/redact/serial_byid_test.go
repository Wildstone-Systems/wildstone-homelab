package redact

import (
	"strings"
	"testing"
)

// sonoffByIDName is a realistic udev by-id name, built at runtime so no
// serial number is ever written into the repo.
func sonoffByIDName() string {
	return "usb-ITead_Sonoff_Zigbee_3.0_USB_Dongle_Plus_" + "REPLACED" + strings.Repeat("0a", 8) + "-if00-port0"
}

// A udev by-id name is hardware identity, not a credential, and it is the
// stable identity the Console's radio picker keys on. It survives both as
// a structured value and inside the multiline YAML document a plan renders.
func TestSerialByIDValuesSurviveRedaction(t *testing.T) {
	name := sonoffByIDName()
	alias := "/dev/serial/by-id/" + name
	id := "serial-by-id:" + name
	if got := String(name); got == name {
		t.Fatalf("precondition: the bare name %q must be secret-shaped to the value rules", name)
	}
	for _, in := range []string{
		alias,
		id,
		`{"id":"` + id + `","byIdPaths":["` + alias + `","` + alias + `"]}`,
		`{"devices":[{"source":"` + alias + `","target":"/dev/ttyACM0"}]}`,
		`{"radioCandidates": [ "` + id + `" ]}`,
		"put:\n  document: |\n    devices:\n      - source: " + alias + "\n        target: /dev/zigbee\n",
		"map " + alias + " into the container",
		`{"summary":"x ` + id + `"}`,
		`{"msg":"device \"` + alias + `\" not found"}`,
	} {
		if got := String(in); got != in {
			t.Errorf("by-id value was redacted:\n in: %s\nout: %s", in, got)
		}
	}
}

// The shelter is for the exact by-id name only: extended paths and values
// merely wearing the prefix still meet the value rules.
func TestSerialByIDShelterIsNarrow(t *testing.T) {
	name := sonoffByIDName()
	alias := "/dev/serial/by-id/" + name
	for _, in := range []string{
		// Extra path segments.
		alias + "/extra",
		`{"source":"` + alias + `/extra"}`,
		"/dev/serial/by-id/sub/" + name,
		// A genuine secret wearing the prefix.
		"/dev/serial/by-id/aGVsbG8gd29ybGQgdGhpcyBpcyBhIHZlcnkgbG9uZyBzZWNyZXQ/x",
	} {
		if got := String(in); !strings.Contains(got, Placeholder) {
			t.Errorf("not an exact by-id value, must still redact:\n in: %s\nout: %s", in, got)
		}
	}
}

// Genuine secret-shaped values are untouched by the new shelter.
func TestSecretsStillRedactNextToSerialByID(t *testing.T) {
	alias := "/dev/serial/by-id/" + sonoffByIDName()
	secret := "aGVsbG9Xb3JsZFRoaXNJc0FTZWNyZXRWYWx1ZTEyMw"
	in := `{"source":"` + alias + `","note":"` + secret + `"}`
	got := String(in)
	if strings.Contains(got, secret) || !strings.Contains(got, alias) {
		t.Errorf("want the alias kept and the secret redacted, got %s", got)
	}
	in = "source: " + alias + "\napi_token: " + secret + "\n"
	got = String(in)
	if strings.Contains(got, secret) || !strings.Contains(got, alias) {
		t.Errorf("want the YAML alias kept and adjacent secret redacted, got %s", got)
	}
	// A by-id value under a credential-shaped key is still a credential.
	in = `{"DB_PASSWORD":"` + alias + `"}`
	if got := String(in); got != `{"DB_PASSWORD":"`+Placeholder+`"}` {
		t.Errorf("keyword rule must still take the value, got %s", got)
	}
	// API responses can carry YAML or JSON inside an already serialized JSON
	// string. Escaped quotes must not hide the credential-shaped key from the
	// same rule before the by-id shelter is restored.
	in = `{"summary":"DB_PASSWORD: \"` + alias + `\""}`
	if got := String(in); got != `{"summary":"DB_PASSWORD: \"`+Placeholder+`\""}` {
		t.Errorf("escaped keyword rule must still take the value, got %s", got)
	}
	// A bare backslash starts a JSON escape; it is not part of a quoted
	// key/value separator. Consuming it would turn \n into the invalid \[ when
	// the following prose is redacted and corrupt the entire API response.
	in = `{"summary":"password: \nuse secret/home-automation-publisher"}`
	if got := String(in); got != in {
		t.Errorf("JSON escape beside a credential word was corrupted:\n in: %s\nout: %s", in, got)
	}
}
