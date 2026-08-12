package butler

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
var kvSecretRegexp = regexp.MustCompile(
	`(?i)([A-Za-z0-9]*[_-]?(?:password|secret|token|api[_-]?key))\s*[:=]\s*(\S+)`)

const redactedPlaceholder = "[REDACTED]"

// secretRefSentinelMarker is the stable prefix of the per-reference sentinel
// used to shield secret/<name> references from the value-shaped redaction
// rules. Kept as a named constant so the key/value rule can recognize a
// sheltered reference and avoid redacting it.
const secretRefSentinelMarker = "\x00SECROUND\x00"

// redact strips secret-looking values from s before it is ever handed back
// to the model, while preserving secret/<name> references verbatim
// (ADR-W006 A3: "The model never sees secrets... leaves secret/<name>
// references"). Called from exactly one place — ToolGateway.Dispatch — so
// every tool result is covered without each handler having to remember to
// call it (ADR-W006 B3).
func redact(s string) string {
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

	protected = hexTokenRegexp.ReplaceAllString(protected, redactedPlaceholder)
	protected = base64TokenRegexp.ReplaceAllString(protected, redactedPlaceholder)
	protected = kvSecretRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		sub := kvSecretRegexp.FindStringSubmatch(m)
		// A value that is (or contains) a protected secret/<name> sentinel is
		// a reference, not a secret value — leave the whole match intact so
		// the reference is restored verbatim below, rather than redacting the
		// sentinel and silently dropping the reference.
		if strings.Contains(sub[2], secretRefSentinelMarker) {
			return m
		}
		return sub[1] + ": " + redactedPlaceholder
	})

	for i, ref := range refs {
		placeholder := fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, i)
		protected = strings.ReplaceAll(protected, placeholder, ref)
	}
	return protected
}
