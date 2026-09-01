package agent

import (
	"strings"
	"testing"
)

// The pbkdf2 digest is consumed by Authelia, which decodes it with
// go-crypt's Base64RawAdaptedEncoding — alphabet
// "A-Za-z0-9./", i.e. standard base64 with '.' at index 62 instead of
// '+'. Emitting standard base64 produced a digest Authelia rejected:
//
//	could not decode '$pbkdf2-sha512$310000$...$+uvh6fCsWtlck...'
//	to a *schema.PasswordDigest: pbkdf2 decode error:
//	illegal base64 data at input byte 0
//
// The bug was DATA-DEPENDENT, which is what made it worth a dedicated
// test rather than a passing glance: a digest only breaks when its salt
// or key happens to encode a '+'. Over ~108 base64 characters that is
// most secrets but not all, so a single hand-test can pass and the
// failure reads as flake.

// TestCryptAlphabetMapsIndex62 is the deterministic half. 0xFB's first
// six bits are 111110 = 62, the one index where the two alphabets
// differ. Standard base64 emits '+' there; the crypt alphabet emits '.'.
func TestCryptAlphabetMapsIndex62(t *testing.T) {
	if got := crypt64.EncodeToString([]byte{0xFB}); got[0] != '.' {
		t.Fatalf("crypt64 encoded index 62 as %q, want '.' — this is the single character that "+
			"separates the crypt alphabet from Go's standard one, and the one Authelia rejects", got[:1])
	}
	// Index 63 is '/' in BOTH alphabets; asserting it guards against
	// "fixing" the encoder by swapping the wrong character.
	if got := crypt64.EncodeToString([]byte{0xFF}); got[0] != '/' {
		t.Fatalf("crypt64 encoded index 63 as %q, want '/'", got[:1])
	}
}

// TestDigestNeverEmitsAStandardBase64Character is the end-to-end half,
// over enough real digests that a '+' would be overwhelmingly likely if
// the encoder regressed: each digest is ~108 base64 characters, so the
// chance one contains no '+' under the standard alphabet is about 0.18.
func TestDigestNeverEmitsAStandardBase64Character(t *testing.T) {
	const runs = 10
	for i := 0; i < runs; i++ {
		got, err := pbkdf2Sha512Digest("correct horse battery staple")
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		body, ok := strings.CutPrefix(got, "$pbkdf2-sha512$310000$")
		if !ok {
			t.Fatalf("digest has an unexpected prefix: %q", got)
		}
		if strings.ContainsAny(body, "+=") {
			t.Fatalf("digest carries '+' or padding, which the crypt alphabet has no room for:\n%s\n"+
				"Authelia will reject it with \"illegal base64 data\"", got)
		}
	}
}

// TestDigestShape pins the format Authelia parses, so a future change to
// iterations or salt size is a deliberate one.
func TestDigestShape(t *testing.T) {
	got, err := pbkdf2Sha512Digest("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(got, "$")
	// "", "pbkdf2-sha512", "310000", salt, key
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha512" || parts[2] != "310000" {
		t.Fatalf("digest shape changed: %q", got)
	}
	// 16 salt bytes and 64 key bytes, unpadded base64.
	if len(parts[3]) != 22 {
		t.Errorf("salt is %d chars, want 22 (16 bytes, unpadded)", len(parts[3]))
	}
	if len(parts[4]) != 86 {
		t.Errorf("key is %d chars, want 86 (64 bytes, unpadded)", len(parts[4]))
	}
}
