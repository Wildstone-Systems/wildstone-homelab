package redact

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/fakesecret"
)

// The URL shelter (ADR-W025 A6). "/" is in the base64 character class, so
// a URL path of ordinary short segments read as one long token run — and
// the consequences ran in both directions at once: the policy engine
// REFUSED a container env carrying the household intake URL, and the
// redactor wrote [REDACTED] over it in the plan that would have explained
// why.
//
// These tests come in pairs on purpose. Every case that must now pass is
// matched by one that must still be caught, because the way to make the
// first set green is to weaken the rule until the second set fails too.
//
// Every credential-shaped fixture comes from keystone/internal/fakesecret
// and is assembled at runtime. A fabricated key written as a literal is
// still exactly what the repository's secret scanner exists to catch, and
// it cannot tell an invented one from a real one — see that package, and
// .gitleaksignore, for the two earlier times this was learned.

// TestOrdinaryURLsAreNotSecrets is the reported defect, with the exact
// value that produced it.
func TestOrdinaryURLsAreNotSecrets(t *testing.T) {
	for _, s := range []string{
		"https://wildstone.tailnet.ts.net/api/v1/household/events",
		"https://wildstone.example.com/api/v1/household/events",
		"http://192.168.1.10:8080/api/v2/state/current",
		"https://docs.example.com/accounts/authelia/login/callback/",
		"https://photos.example.com/auth/login?redirect=/albums/2024",
		"https://home.example.com:10443/api/v1/services/status",
	} {
		if SecretShaped(s) {
			t.Errorf("refused an ordinary URL as secret-shaped: %s", s)
		}
		if got := String(s); got != s {
			t.Errorf("redacted an ordinary URL\n got %q\nwant %q", got, s)
		}
	}
}

// TestURLsCarryingCredentialsAreStillCaught. Each of these has a part
// that is token-shaped on its own, so the shelter declines and the value
// rules take the whole URL. If the shelter is ever loosened to "it parses
// as a URL, therefore it is safe", every one of these goes silent.
func TestURLsCarryingCredentialsAreStillCaught(t *testing.T) {
	token, hex := fakesecret.Token(), fakesecret.Hex(56)
	for name, tc := range map[string]struct{ url, secret string }{
		"token in a path segment": {
			"https://hooks.example.com/services/T0000/B0000/" + token, token},
		"token in a query value": {
			"https://api.example.com/v1/push?" + "api" + "_key=" + token, token},
		"hex key in a path": {
			"https://api.example.com/v1/" + hex, hex},
		"userinfo credentials": {
			"https://admin:" + token + "@internal.example.com/admin", token},
		"a JWT": {
			"https://api.example.com/cb#" + fakesecret.JWTShaped(), fakesecret.Token()},
		"presigned signature": {
			"https://s3.example.com/bucket/obj?X-Amz-Signature=" + hex, hex},
	} {
		if !SecretShaped(tc.url) {
			t.Errorf("%s: no longer caught: %s", name, tc.url)
		}
		if got := String(tc.url); strings.Contains(got, tc.secret) {
			t.Errorf("%s: credential survived redaction: %s", name, got)
		}
	}
}

// TestUserinfoIsNeverSheltered asserts the shelter's DECISION rather than
// the final verdict, because those are two different things here and the
// difference is worth writing down.
//
// A short credential in a URL is not caught by shape today and was not
// before this shelter existed: no run in it reaches 24 characters. That
// gap is real and pre-existing, and it is covered where it actually
// matters, by rules that read the KEY rather than the value:
// kvSecretRegexp redacts anything under a password/secret/token/api_key
// key, and the policy engine refuses such a key to hold anything but a
// secret/<name> reference.
//
// What this shelter must never do is CREATE that gap for values the shape
// rules would otherwise have caught. So the assertion is that a userinfo
// URL is refused shelter unconditionally, before shape enters into it —
// which is what makes the long-credential case in the test above still
// fail closed.
func TestUserinfoIsNeverSheltered(t *testing.T) {
	for _, s := range []string{
		"https://admin:" + fakesecret.Password() + "@internal.example.com/api/v1/status",
		"https://admin:" + fakesecret.Token() + "@internal.example.com/admin",
		"https://" + fakesecret.Password() + "@internal.example.com/api/v1/status",
	} {
		if urlIsSafe(s) {
			t.Errorf("a URL carrying userinfo must never be sheltered: %s", s)
		}
	}
}

// TestTheShelterDoesNotRescueATokenBesideAURL. The shelter blanks the URL
// it matched and nothing else, so a secret sitting next to one is
// unaffected. Worth pinning: a shelter that over-matched would be a way
// to smuggle a value past the rule by writing a URL in front of it.
func TestTheShelterDoesNotRescueATokenBesideAURL(t *testing.T) {
	token := fakesecret.Token()
	s := "endpoint=https://api.example.com/v1/events " + "k" + "ey=" + token
	if !SecretShaped(s) {
		t.Error("a token beside a URL must still be caught")
	}
	if got := String(s); strings.Contains(got, token) {
		t.Errorf("token beside a URL survived redaction: %s", got)
	}
}

// TestTheShelterStopsAtQuotes. String runs over serialized JSON bodies
// (ADR-W007 A5), and a shelter that swallowed the closing quote would
// corrupt every document carrying a URL — the failure kvSecretRegexp's
// value class already exists to avoid.
func TestTheShelterStopsAtQuotes(t *testing.T) {
	s := `{"intake":"https://wildstone.example.com/api/v1/household/events","node":"n1"}`
	if got := String(s); got != s {
		t.Errorf("a URL in a JSON body must survive verbatim\n got %q\nwant %q", got, s)
	}
	if SecretShaped(s) {
		t.Error("a JSON body carrying an ordinary URL is not secret-shaped")
	}
}

// TestACredentialKeyedURLIsStillRefused closes the obvious hole in the
// whole idea. A value under a credential-shaped KEY is refused by the key
// rule regardless of shape, so sheltering the URL's structure cannot let
// a webhook secret through under a name like this one.
func TestACredentialKeyedURLIsStillRefused(t *testing.T) {
	s := "WEBHOOK_" + "TOKEN: https://hooks.example.com/t/abc"
	if got := String(s); strings.Contains(got, "hooks.example.com") {
		t.Errorf("a URL under a credential-shaped key must still be redacted: %s", got)
	}
}

// TestTheShelterIsAnchoredOnHTTP. The scheme is the anchor, so the
// shelter reaches exactly the values it was written for and nothing
// resembling them. ftp:// is the control: same structure, same long
// digit-carrying run, still judged the way it was before this existed.
//
// Widening the scheme set would be a decision about a value class nobody
// has needed yet, made by accident.
func TestTheShelterIsAnchoredOnHTTP(t *testing.T) {
	if urlScanRegexp.MatchString("ftp://example.com/api/v1/some/long/path/here") {
		t.Error("the shelter must not match a non-http scheme")
	}
	if !SecretShaped("ftp://example.com/api/v1/some/long/path/here") {
		t.Error("an ftp URL's judgement must be unchanged by an http(s) shelter")
	}
}

// TestReferenceFormsAreUnaffected. The earlier shelters (F27, F36) run
// after this one and must still do their jobs.
func TestReferenceFormsAreUnaffected(t *testing.T) {
	// Not secret VALUES, so the policy engine accepts all of them.
	for _, s := range []string{
		"container/immich-postgres",
		"secret/home-automation-publisher",
		"Volume/immich-postgres-data",
	} {
		if SecretShaped(s) {
			t.Errorf("%s: a reference is not a secret value", s)
		}
	}
	// String is deliberately more aggressive than SecretShaped, and only
	// the forms with their own shelter survive it verbatim.
	// "container/immich-postgres" is not among them — it reads as a 25-char
	// base64 run and redacts, which SecretShaped's own comment records as
	// the accepted asymmetry (over-redaction is the safe direction).
	for _, s := range []string{
		"secret/home-automation-publisher",
		"Volume/immich-postgres-data",
	} {
		if got := String(s); got != s {
			t.Errorf("%s: redacted a sheltered reference: %q", s, got)
		}
	}
	// The bare hex shape still redacts whether or not a URL is nearby.
	hex := fakesecret.Hex(48)
	if !SecretShaped(hex) {
		t.Error("a bare hex token must still be secret-shaped")
	}
	if !SecretShaped("see https://example.com/docs then " + hex) {
		t.Error("a hex token beside a sheltered URL must still be secret-shaped")
	}
}
