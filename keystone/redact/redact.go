// Package redact is Keystone's centralized secret-value redaction
// (ADR-W007 A5), extracted from the S4 Butler so the public API and the
// Butler share one implementation. Secret VALUES (hex/base64 shapes,
// password/token-keyed values) are replaced with a placeholder; secret
// REFERENCES (secret/<name>) survive verbatim — they are the plan/UX
// vocabulary, not secrets.
package redact

import (
	"fmt"
	"regexp"
	"strings"
)

// secretRefScanRegexp finds secret *references* of the form "secret/<name>"
// anywhere in a larger string (unanchored sibling of resource's own
// ^secret/...$ validator regexp). These must survive redaction verbatim —
// ADR-W005 A2: a reference is not a secret value.
var secretRefScanRegexp = regexp.MustCompile(`secret/[A-Za-z0-9._-]+`)

// digestPinScanRegexp finds digest-pinned image references and bare
// sha256 content digests (ADR-W004 A1: every Container image is
// digest-pinned). A digest is an integrity pin over public content, not a
// secret, and redacting it would break round-tripping a resource through
// the public API (GET yaml → edit → propose), so pins are sheltered like
// secret references. The image path is included in the shelter because
// the base64-shape rule otherwise fires on registry paths
// ("ghcr.io/immich-app/immich-server" is a 24+-run of [A-Za-z0-9/-]).
// The bare 64-hex shape WITHOUT the "sha256:" prefix still redacts.
var digestPinScanRegexp = regexp.MustCompile(
	`\b(?:[a-zA-Z0-9][a-zA-Z0-9./_:-]*@)?sha256:[0-9a-fA-F]{64}\b`)

// commitRefScanRegexp finds a Statefile commit hash carried under a key
// that names one, in JSON or YAML shape. A Statefile commit is a git
// object id — the same hashes `git log` prints in the clear one directory
// away, and precisely what an auditor needs to know WHICH desired state a
// plan was computed against. The bare 40-hex shape is indistinguishable
// from a hex-encoded key, so the shelter is keyed on the field name
// rather than the value: only these keys are sheltered, and a hash
// appearing anywhere else still redacts.
var commitRefScanRegexp = regexp.MustCompile(
	`(?i)"?\b(?:basedOnHead|statefileHead|newHead|head|commit)"?\s*[:=]\s*"?[0-9a-fA-F]{7,64}"?`)

// hexTokenRegexp matches long hex runs — sha256 digests, hex-encoded keys,
// password hashes — the shape of value redaction must catch regardless of
// surrounding field name.
var hexTokenRegexp = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)

// base64TokenRegexp matches long base64/base64url-shaped runs (API keys,
// bearer tokens) that aren't already caught by hexTokenRegexp.
var base64TokenRegexp = regexp.MustCompile(`\b[A-Za-z0-9+/_-]{24,}={0,2}\b`)

// kvSecretRegexp matches "password/secret/token/api_key: VALUE"-shaped
// key/value pairs and redacts VALUE, as defense-in-depth alongside the
// shape-based regexps above (catches short/low-entropy values a shape match
// would miss — a short DB password or PIN reaches the model otherwise).
//
// The key may carry an identifier prefix or suffix, so DB_PASSWORD,
// IMMICH_API_KEY, and app-secret are all caught — the previous leading \b
// failed whenever a word char or underscore preceded the keyword, which is
// the overwhelmingly common case for env-var-style keys. The keyword must
// still be a whole word within the identifier (bounded by a non-alnum
// separator or an underscore/dash on either side) so it does not fire on
// field names that merely contain the substring (e.g. "secretRef:",
// "clientSecretHint:"). Group 1 captures the full key (echoed back
// verbatim); group 2 is the redacted value.
// The value class excludes quotes and backslashes (not just whitespace):
// String also runs over JSON-serialized API bodies (ADR-W007 A5), where a
// \S+ value would eat the string's closing quote or an escape sequence
// and corrupt the document. A value is therefore redacted up to the first
// quote/escape; the shape-based rules above still catch high-entropy
// secrets that contain them.
var kvSecretRegexp = regexp.MustCompile(
	`(?i)([A-Za-z0-9]*[_-]?(?:password|secret|token|api[_-]?key))\s*[:=]\s*([^\s"'\\]+)`)

// Placeholder replaces every redacted secret value.
const Placeholder = "[REDACTED]"

// secretRefSentinelMarker is the stable prefix of the per-reference sentinel
// used to shield secret/<name> references from the value-shaped redaction
// rules. Kept as a named constant so the key/value rule can recognize a
// sheltered reference and avoid redacting it.
const secretRefSentinelMarker = "\x00SECROUND\x00"

// SecretShaped reports whether s contains a secret-VALUE shape (long hex
// or base64 runs), ignoring sheltered forms — secret/<name> references
// and sha256 digest pins are not secrets. The policy engine's
// secret-values-forbidden rule (ADR-W009 A2) uses it to keep literal
// secrets out of the Statefile with the same shape heuristics this
// package redacts by, so the two can never disagree about what counts.
func SecretShaped(s string) bool {
	if s == "" {
		return false
	}
	sheltered := secretRefScanRegexp.ReplaceAllString(s, "")
	sheltered = digestPinScanRegexp.ReplaceAllString(sheltered, "")
	sheltered = commitRefScanRegexp.ReplaceAllString(sheltered, "")
	if hexTokenRegexp.MatchString(sheltered) {
		return true
	}
	// The base64 shape alone over-matches reference-like values with zero
	// entropy ("container/immich-postgres" is 25 chars of [a-z/-]) —
	// tolerable for redaction (String stays aggressive; over-redaction is
	// the safe direction) but not for a policy REJECTION. Require at
	// least one digit in the candidate: real base64/token material
	// virtually always carries digits, hostname-and-reference prose
	// virtually never does.
	for _, m := range base64TokenRegexp.FindAllString(sheltered, -1) {
		if strings.ContainsAny(m, "0123456789") {
			return true
		}
	}
	return false
}

// String strips secret-looking values from s, while preserving
// secret/<name> references verbatim (ADR-W005 A2: a reference is not a
// secret value). Two centralized call sites, each covering a whole plane
// so no individual handler has to remember redaction: the Butler's
// ToolGateway.Dispatch redacts every tool result before it reaches the
// model (ADR-W006 B3), and the public API's response encoder redacts
// every serialized body (ADR-W007 A5).
func String(s string) string {
	if s == "" {
		return s
	}

	// Protect secret/<name> references from the value-shaped regexps below
	// by swapping them for placeholders that don't look like secrets, then
	// restoring the originals at the end.
	var refs []string
	protected := secretRefScanRegexp.ReplaceAllStringFunc(s, func(m string) string {
		refs = append(refs, m)
		return fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, len(refs)-1)
	})
	// Digest pins get the same shelter (see digestPinScanRegexp).
	protected = digestPinScanRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		refs = append(refs, m)
		return fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, len(refs)-1)
	})
	// So do Statefile commit hashes named by their key (see
	// commitRefScanRegexp).
	protected = commitRefScanRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		refs = append(refs, m)
		return fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, len(refs)-1)
	})

	protected = hexTokenRegexp.ReplaceAllString(protected, Placeholder)
	protected = base64TokenRegexp.ReplaceAllString(protected, Placeholder)
	protected = kvSecretRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		sub := kvSecretRegexp.FindStringSubmatch(m)
		// A value that is (or contains) a protected secret/<name> sentinel is
		// a reference, not a secret value — leave the whole match intact so
		// the reference is restored verbatim below, rather than redacting the
		// sentinel and silently dropping the reference.
		if strings.Contains(sub[2], secretRefSentinelMarker) {
			return m
		}
		return sub[1] + ": " + Placeholder
	})

	for i, ref := range refs {
		placeholder := fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, i)
		protected = strings.ReplaceAll(protected, placeholder, ref)
	}
	return protected
}
