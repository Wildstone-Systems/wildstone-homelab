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
// stable identity the Console's radio picker keys on. Exactly that value
// — as a whole input or a whole JSON string — survives.
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
	} {
		if got := String(in); got != in {
			t.Errorf("by-id value was redacted:\n in: %s\nout: %s", in, got)
		}
	}
}

// The shelter is for the exact value only: anything else wearing a by-id
// name still meets the value rules.
func TestSerialByIDShelterIsNarrow(t *testing.T) {
	name := sonoffByIDName()
	alias := "/dev/serial/by-id/" + name
	for _, in := range []string{
		// Extra path segments.
		alias + "/extra",
		`{"source":"` + alias + `/extra"}`,
		"/dev/serial/by-id/sub/" + name,
		// Embedded in a larger string.
		"map " + alias + " into the container",
		`{"summary":"map ` + alias + ` into the container"}`,
		`{"summary":"` + alias + ` missing"}`,
		`{"summary":"x serial-by-id:` + name + `"}`,
		// Escaped inside a larger JSON string, not a value of its own.
		`{"msg":"device \"` + alias + `\" not found"}`,
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
	// A by-id value under a credential-shaped key is still a credential.
	in = `{"DB_PASSWORD":"` + alias + `"}`
	if got := String(in); got != `{"DB_PASSWORD":"`+Placeholder+`"}` {
		t.Errorf("keyword rule must still take the value, got %s", got)
	}
}
