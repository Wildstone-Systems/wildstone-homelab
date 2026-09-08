package redact

import "testing"

// A path is structure, not a value. The base64 shape rule is
// [A-Za-z0-9+/_-]{24,} and "/" is in that class, so every absolute path
// of 24 characters or more was redacted — including the defaults four
// shipped media packages ship. Because redaction runs over every API
// response and the console's install form submits the values the API
// serves, "/[REDACTED]" was written into desired state as a real bind
// mount, and the install failed forever.
func TestFilesystemPathsSurviveRedaction(t *testing.T) {
	for _, p := range []string{
		"/srv/wildstone/media/audiobooks",
		"/srv/wildstone/media/books",
		"/srv/wildstone/media/music",
		"/home/someone/audiobooks",
		"/mnt/8TB/media/audiobooks",
		"/media/library/audiobooks/fiction",
		"/etc/wildstone/home-assistant/wildstone.yaml",
		"/var/lib/wildstone/keystone",
	} {
		if got := String(p); got != p {
			t.Errorf("path %q was redacted to %q", p, got)
		}
	}
}

// The shelter must not become a way to smuggle a secret past the value
// rules by putting a slash in front of it.
func TestSecretsWearingASlashAreStillRedacted(t *testing.T) {
	for _, s := range []string{
		"/aGVsbG8gd29ybGQgdGhpcyBpcyBhIHZlcnkgbG9uZyBzZWNyZXQ",
		"/deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"/srv/wildstone/aGVsbG9Xb3JsZFRoaXNJc0FTZWNyZXRWYWx1ZTEyMw",
	} {
		if got := String(s); got == s {
			t.Errorf("secret-shaped %q survived redaction", got)
		}
	}
}

// A path under a credential-shaped key is still a credential: the
// keyword rule runs after the shelters over the same text.
func TestPathShelterDoesNotDefeatTheKeywordRule(t *testing.T) {
	in := `{"DB_PASSWORD":"/srv/wildstone/media/audiobooks"}`
	if got := String(in); got == in {
		t.Errorf("a value under a credential-shaped key must still be redacted, got %q", got)
	}
}

// The exact shape that broke: a rendered ContainerSpec mount.
func TestAMountSourceSurvivesRoundTrippingThroughTheAPI(t *testing.T) {
	in := `{"mounts":[{"source":"/srv/wildstone/media/audiobooks","target":"/audiobooks"}]}`
	got := String(in)
	if got != in {
		t.Errorf("a mount the operator must be able to read was redacted:\n in: %s\nout: %s", in, got)
	}
}

// URLs and digest pins are matched by their own, more specific shelters
// first; the path rule must not have disturbed them.
func TestExistingSheltersStillWin(t *testing.T) {
	for _, s := range []string{
		"https://wildstone.example.com/household/events",
		"ghcr.io/home-assistant/home-assistant@sha256:612d76760b544cb40b7ba01387fdac964c59a6a550a50a4d30b4773c822d2918",
		"secret/restic-home-assistant",
	} {
		if got := String(s); got != s {
			t.Errorf("%q was redacted to %q", s, got)
		}
	}
}
