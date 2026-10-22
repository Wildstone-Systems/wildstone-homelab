// Package fakesecret builds secret-SHAPED values at runtime, for the
// tests that have to prove something about the shape.
//
// It exists because of a practice `.gitleaksignore` records, and it is
// the thing that file says the third occurrence calls for.
//
// The rule is CONTRIBUTING's first: no secrets in the repository, ever,
// including test fixtures. The subtlety is that an INVENTED key is still
// exactly what the scanner is there to catch, and the scanner cannot tell
// an invented one from a real one — so it fires, correctly, and the fix
// is the test rather than the rule. That has now happened three times:
// f663f34 wrote a fabricated API key, 9540e44 a fabricated PEM key, and
// the URL-shelter tests for ADR-W025 A6 wrote fabricated bearer tokens
// and a JWT. `.gitleaksignore` says what to do the third time, in as many
// words: "if a third entry is ever needed, that practice is not holding
// and the answer is a shared helper, not another line in this file."
//
// So every value here is ASSEMBLED AT CALL TIME from low-entropy pieces.
// The shape exists in memory, where a test needs it, and in no source
// line anywhere — which is what lets `keystone/redact` and
// `keystone/policy` both test the shape rules without either of them
// carrying a literal the scanner has to be told to ignore.
//
// Nothing here is, or has ever been, a real credential. The values are
// deterministic so a failure names the same string every run, and they
// decode to nothing.
//
// This is a non-test package so that tests in different packages can
// share it; nothing outside a test imports it.
package fakesecret

import "strings"

// lowercase is the only alphabet written down here, and it carries no
// information: every value below is derived from it by position.
const lowercase = "abcdefghijklmnopqrstuvwxyz"

// Token returns a 32-character run of the base64url alphabet.
//
// Two properties the redaction rules actually read, so they are chosen
// rather than incidental: it CARRIES DIGITS, because
// redact.SecretShaped ignores a digit-free run (hostname prose has no
// digits, real token material does), and it carries NO separator,
// because a separated identifier is sheltered as a name rather than
// judged as a value.
func Token() string {
	var b strings.Builder
	for i := 0; i < 32; i++ {
		switch i % 3 {
		case 0:
			b.WriteByte(lowercase[i%26])
		case 1:
			b.WriteByte(lowercase[(i*7)%26] - ('a' - 'A')) // the uppercase half
		default:
			b.WriteByte(byte('0' + i%10))
		}
	}
	return b.String()
}

// Hex returns n hex characters — the shape of a digest, a hashed
// password, or a hex-encoded key.
func Hex(n int) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(digits[i%16])
	}
	return b.String()
}

// JWTShaped returns three dot-separated token runs: the SHAPE of a JSON
// Web Token and nothing else.
//
// Deliberately not a realistic one. A real JWT's header decodes from a
// fixed "eyJ..." prefix, and writing that prefix is writing the literal
// the scanner's jwt rule exists to find — while none of the rules under
// test care about the prefix at all. What they read is three long runs
// separated by dots, which is exactly what this is.
func JWTShaped() string {
	return strings.Join([]string{Token(), Token(), Token()}, ".")
}

// Password returns a short, low-entropy credential — the kind no shape
// rule can catch, and that only a rule reading the KEY will ever find.
// Tests use it to say so out loud.
func Password() string {
	return strings.Join([]string{"pass", "word", "42"}, "")
}
