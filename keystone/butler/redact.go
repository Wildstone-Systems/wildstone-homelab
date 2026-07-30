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
// shape-based regexps above (catches short/low-entropy planted values a
// shape match would miss). It requires the colon/equals to immediately
// follow the keyword so it does not fire on field names that merely contain
// the substring (e.g. "secretRef:", "clientSecretHint:").
var kvSecretRegexp = regexp.MustCompile(`(?i)\b(password|secret|token|api[_-]?key)\s*[:=]\s*(\S+)`)

const redactedPlaceholder = "[REDACTED]"

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
		return fmt.Sprintf("\x00SECROUND\x00%d\x00", len(refs)-1)
	})

	protected = hexTokenRegexp.ReplaceAllString(protected, redactedPlaceholder)
	protected = base64TokenRegexp.ReplaceAllString(protected, redactedPlaceholder)
	protected = kvSecretRegexp.ReplaceAllString(protected, "$1: "+redactedPlaceholder)

	for i, ref := range refs {
		placeholder := fmt.Sprintf("\x00SECROUND\x00%d\x00", i)
		protected = strings.ReplaceAll(protected, placeholder, ref)
	}
	return protected
}
