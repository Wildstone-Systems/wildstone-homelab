// Package redact is Keystone's centralized secret-value redaction
// (ADR-W007 A5), extracted from the S4 Butler so the public API and the
// Butler share one implementation. Secret VALUES (hex/base64 shapes,
// password/token-keyed values) are replaced with a placeholder; secret
// REFERENCES (secret/<name>) survive verbatim — they are the plan/UX
// vocabulary, not secrets.
package redact

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
//
// The quotes may be backslash-escaped: String also runs over JSON that has
// itself been serialized into a JSON string, where the text is
// \"basedOnHead\":\"…\". Without allowing that backslash the shelter
// missed, the hash fell through to the hex rule, and one document reported
// the same field two different ways — the real hash at the top level and
// [REDACTED] in the escaped copy (F32).
var commitRefScanRegexp = regexp.MustCompile(
	`(?i)\\?"?\b(?:basedOnHead|statefileHead|newHead|head|commit)\\?"?\s*[:=]\s*\\?"?[0-9a-fA-F]{7,64}\\?"?`)

// eventIDRefScanRegexp finds a household event id or correlation id
// carried under the key that names it, in JSON shape (ADR-W024 A2). A
// publisher-supplied id is, by construction, whatever shape the
// publisher's own id scheme produces — a ULID is 26 characters of
// [0-9A-Z], a UUID is hex with hyphens — and both fall squarely into the
// value rules below, so without this shelter every event read back
// through the API carried "[REDACTED]" where its identity should be. An
// id nobody can read is an id nobody can dedupe, replay, or correlate
// against, which is the whole reason the envelope has one.
//
// Keyed on the field name, like the commit shelter, because the value's
// shape is exactly what cannot be trusted to distinguish it. The value
// class is the id character set — which includes "@" and ":", the
// separators the automation engine's own decision ids use
// ("<rule>@<triggerSeq>:act") — and the length bound mirrors the intake's
// (keystone/household MaxEventIDBytes), so an oversize value is not
// sheltered. As with commit hashes, the quotes may be backslash-escaped.
//
// Not a leak: the value under these keys came from a publisher, not from
// anything Keystone holds. A publisher that writes a secret it already
// holds into an id has exfiltrated nothing it did not have, and the
// Butler's own tool-result redaction is a separate layer that still
// runs over anything an id carries into a model.
var eventIDRefScanRegexp = regexp.MustCompile(
	`\\?"(?:eventId|correlationId)\\?"\s*:\s*\\?"[A-Za-z0-9._:+@-]{1,128}\\?"`)

// kindRefScanRegexp finds a resource reference of the form "Kind/name"
// over the closed kind vocabulary. These are sheltered for exactly the
// reason digest pins are: an ordinary "Volume/immich-postgres-data" is a
// 24+-character run of [A-Za-z0-9/-], so the base64 shape below fired on
// it and a plan step read "Create [REDACTED]" (F27). Any Volume name of
// 17 characters was enough, a Container 14, a BackupPlan 13 — and the
// first-party ingress package tripped it on its own plan.
//
// That is not cosmetic. Plan review by a human is the product's whole
// safety claim, and a plan whose steps cannot be read is a plan that
// cannot be reviewed. It hit error messages too, where there is no
// adjacent field to recover the name from.
//
// The alternation is built from resource.Kinds() rather than written out,
// so adding a kind cannot silently leave it unsheltered. Names use the
// same character class as secret references.
var kindRefScanRegexp = regexp.MustCompile(
	`\b(?:` + strings.Join(kindNames(), "|") + `)/[A-Za-z0-9._-]+\b`)

// kindRefIsSafe reports whether a matched Kind/name reference may be
// sheltered. The shelter is purely textual, so without this any
// high-entropy string wearing a kind prefix passed through in the clear
// (F36):
//
//	Volume/deadbeefcafebabe0123456789abcdef0123456789abcdef  → unchanged
//
// Not an escalation — writing that requires already holding the secret —
// but the redactor should not treat "looks like a resource reference" as
// proof that it is one. Resource names are operator-chosen words; a name
// that is itself secret-shaped is not a name worth protecting, so the
// shelter simply declines and the value rules take it.
func kindRefIsSafe(match string) bool {
	_, name, ok := strings.Cut(match, "/")
	if !ok {
		return false
	}
	if hexTokenRegexp.MatchString(name) {
		return false
	}
	// Mirrors SecretShaped's digit requirement rather than the bare
	// base64 shape: real token material virtually always carries digits,
	// and ordinary reference names ("caddy-tailnet-data") do not.
	for _, m := range base64TokenRegexp.FindAllString(name, -1) {
		if isSeparatedIdentifier(m) {
			continue
		}
		if strings.ContainsAny(m, "0123456789") {
			return false
		}
	}
	return true
}

// urlScanRegexp finds an http(s) URL anywhere in a larger string.
//
// Sheltered for the same reason digest pins and Kind/name references are
// (F27, F36): a URL is structure, not a value, and the base64 shape rule
// fires on it because "/" is in that character class. A path of ordinary
// short segments reads as one long run:
//
//	https://wildstone.tailnet.ts.net/api/v1/household/events  → redacted
//	https://wildstone.tailnet.ts.net/household/events         → fine
//
// Two consequences, and neither is cosmetic. The policy engine's
// secret-values-forbidden rule REFUSES the first one, so a sidecar
// carrying the household intake URL in its env cannot be planned at all —
// and the message says "value is secret-shaped", which names nothing the
// author can act on. And the redactor writes [REDACTED] over it, so a
// plan step naming where a service posts is a plan step nobody can
// review. No shipped package hit either, because their env URLs carry no
// path; the first one to need a real path would have.
//
// The character class stops at whitespace, quotes and backslashes for
// kvSecretRegexp's reason: String also runs over serialized JSON, and a
// greedier match would eat a closing quote and corrupt the document.
var urlScanRegexp = regexp.MustCompile(
	`\bhttps?://[A-Za-z0-9._~:/?#\[\]@!$&()*+,;=%-]+`)

// urlDelimiters are the characters urlIsSafe splits a URL on before
// judging its parts.
//
// The set is chosen for one property: none of them appears inside base64,
// base64url, or hex material. "-" and "_" are base64url's own alphabet
// and are deliberately absent, so a token is never broken up by this
// split. "/" and "+" ARE in standard base64 — but standard base64 is not
// URL-safe and must be percent-encoded to appear in one, so inside a
// well-formed URL a "/" is a path delimiter rather than token content.
//
// The residual escape is worth stating plainly: a secret containing a raw
// "/" that also breaks into pieces all shorter than 24 characters would
// pass. That is a deliberately obfuscated secret rather than an
// accidental one, and it is not the case these rules exist for — an
// accidental literal is one someone pasted whole. Anything under a
// credential-shaped KEY is caught regardless of shape by kvSecretRegexp,
// and the policy engine additionally requires such a key to hold a
// secret/<name> reference and nothing else.
const urlDelimiters = "/?#&=:@."

// urlIsSafe reports whether a matched URL may be sheltered: it is safe
// only when NO part of it is itself token-shaped.
//
// Deliberately stricter than SecretShaped's own bar. SecretShaped ignores
// a base64-shaped run with no digits; this does not, so that sheltering a
// URL can never make String() less aggressive than it was before the
// shelter existed. The shelter's job is to stop the STRUCTURE of a URL
// reading as a token, not to excuse anything carried inside one.
//
// Userinfo is refused outright. "https://user:t0ken@host/path" is the
// textbook way a credential ends up in a config value, and it is exactly
// what this rule is for — the whole URL falls through to the value rules,
// which take the credential.
func urlIsSafe(match string) bool {
	u, err := url.Parse(match)
	if err != nil || u.User != nil {
		return false
	}
	for _, part := range strings.FieldsFunc(match, func(r rune) bool {
		return strings.ContainsRune(urlDelimiters, r)
	}) {
		if hexTokenRegexp.MatchString(part) {
			return false
		}
		for _, m := range base64TokenRegexp.FindAllString(part, -1) {
			if isSeparatedIdentifier(m) {
				continue // a name, not a value
			}
			return false
		}
	}
	return true
}

// absPathScanRegexp finds an absolute POSIX filesystem path.
//
// Sheltered for exactly the reason URLs and digest pins are: a path is
// STRUCTURE, not a value. The base64 shape rule is
// `[A-Za-z0-9+/_-]{24,}`, and "/" is in that class — so every absolute
// path of 24 characters or more looked like token material and was
// redacted.
//
// That was not a cosmetic failure. Redaction is a serialization property
// applied to every API response (ADR-W007 A5), and the console's install
// form uses the values the API serves as the values it SUBMITS. So a
// package's own default path came back as "/[REDACTED]", the form
// offered it, the operator accepted what they were shown, and
// "/[REDACTED]" was written into desired state as a real bind mount. The
// node then refused it — correctly, it resolves outside every managed
// prefix — and the install failed forever, three ticks at a time.
//
// Four shipped packages could not be installed with their own defaults:
// audiobooks-audiobookshelf, books-calibre-web, books-kavita and
// music-navidrome. Every one of them is a media package, because media
// packages are the ones whose paths are long.
//
// The character class deliberately excludes "+" and "=": they are base64
// alphabet and padding, they do not appear in ordinary paths, and
// leaving them out means a base64 blob that happens to start with "/"
// is not matched here at all.
var absPathScanRegexp = regexp.MustCompile(`/[A-Za-z0-9._/-]{2,}`)

// pathIsSafe judges a candidate path the way urlIsSafe judges a URL:
// split it into segments and decline the shelter if any segment is
// itself secret-shaped. A path is a sequence of names, and a name is not
// a value — but "/" + one long base64 run is a value wearing a slash.
func pathIsSafe(match string) bool {
	segments := strings.Split(strings.Trim(match, "/"), "/")
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if hexTokenRegexp.MatchString(seg) {
			return false
		}
		for _, m := range base64TokenRegexp.FindAllString(seg, -1) {
			if isSeparatedIdentifier(m) {
				continue // a name, not a value
			}
			return false
		}
	}
	return true
}

func kindNames() []string {
	kinds := resource.Kinds()
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, regexp.QuoteMeta(string(k)))
	}
	return names
}

// hexTokenRegexp matches long hex runs — sha256 digests, hex-encoded keys,
// password hashes — the shape of value redaction must catch regardless of
// surrounding field name.
var hexTokenRegexp = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)

// base64TokenRegexp matches long base64/base64url-shaped runs (API keys,
// bearer tokens) that aren't already caught by hexTokenRegexp.
var base64TokenRegexp = regexp.MustCompile(`\b[A-Za-z0-9+/_-]{24,}={0,2}\b`)

// separatedIdentifierRegexp matches a separated identifier: kebab-case or
// SCREAMING_SNAKE_CASE, one case family throughout, starting with a
// letter, at least one separator, no segment longer than 20 characters.
//
// This is the F27 shelter applied to the shapes F27 missed. A secret NAME
// ("authelia-storage-encryption-key", 31 chars) and an env-var KEY
// ("IMMICH_MACHINE_LEARNING_URL", 27) are both long runs of the base64
// character class, so `wild secret list` printed a column of [REDACTED]
// and an identity plan's env block was unreadable — the same failure as
// F27 (a plan a human cannot read is a plan a human cannot review),
// reached through a different door.
//
// Sheltering by shape rather than by call site is deliberate: the same
// names appear in plans, in error messages, in the Butler's tool results
// and in API bodies, and a per-site exception would have to be added to
// each and would be missing from the next one.
var separatedIdentifierRegexp = regexp.MustCompile(
	`^(?:[a-z][a-z0-9]{0,19}(?:[_-][a-z0-9]{1,20})+|[A-Z][A-Z0-9]{0,19}(?:[_-][A-Z0-9]{1,20})+)$`)

// isSeparatedIdentifier reports whether a base64-shaped run is really an
// identifier and so should not be redacted as a value.
//
// Two things it deliberately does NOT shelter. A run whose letters are all
// hex digits ("550e8400-e29b-41d4-a716-446655440000", and any hyphenated
// key material) is encoded bytes wearing separators, not a word — the same
// judgment kindRefIsSafe makes for F36. And a diceware-style passphrase
// ("correct-horse-battery-staple") is genuinely indistinguishable from a
// kebab identifier by shape, so it loses this shelter; it stays covered
// where it actually appears, as the value of a password/secret/token key,
// because kvSecretRegexp runs over the unsheltered text and does not care
// about shape at all.
func isSeparatedIdentifier(s string) bool {
	if !separatedIdentifierRegexp.MatchString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'g' && r <= 'z', r >= 'G' && r <= 'Z':
			return true
		}
	}
	return false
}

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
// The separator group tolerates the quotes a JSON body puts around the
// key and the value ("apiKey": "..."), and is echoed back verbatim so the
// document keeps its shape. Without it the rule never fired on a
// serialized API body at all — the shape rules were carrying that case,
// and the identifier shelter above would otherwise have handed a
// low-entropy quoted value straight through.
var kvSecretRegexp = regexp.MustCompile(
	`(?i)([A-Za-z0-9]*[_-]?(?:password|secret|token|api[_-]?key))(["']?\s*[:=]\s*["']?)([^\s"'\\]+)`)

// Placeholder replaces every redacted secret value.
const Placeholder = "[REDACTED]"

// secretRefSentinelMarker is the stable prefix of the per-reference sentinel
// used to shield secret/<name> references from the value-shaped redaction
// rules. Kept as a named constant so the key/value rule can recognize a
// sheltered reference and avoid redacting it.
const secretRefSentinelMarker = "\x00SECROUND\x00"

// urlSentinelMarker shields a sheltered URL from the value-shaped rules,
// and is DISTINCT from secretRefSentinelMarker on purpose.
//
// kvSecretRegexp declines to redact a value carrying the secret-ref
// marker, because a reference under a credential-shaped key is still a
// reference. A URL is not: "WEBHOOK_TOKEN: https://hooks.example.com/t/abc"
// is a secret webhook whatever its structure looks like, and sharing one
// marker with references would have quietly exempted it from the one rule
// that catches a credential by its KEY rather than by its shape.
const urlSentinelMarker = "\x00URLROUND\x00"

// pathSentinelMarker shelters absolute filesystem paths.
//
// Their own marker rather than the secret-reference one, for the reason
// URLs have theirs: a value restored from the reference marker survives
// kvSecretRegexp, and a path sitting under a credential-shaped key is
// still a credential. Under this marker the keyword rule replaces the
// whole value and the sentinel is simply not here to restore.
const pathSentinelMarker = "\x00PATHROUND\x00"

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
	// URLs first, so the value rules never see a path as one long run —
	// and before the other shelters, so the URL is judged exactly as
	// written. A shelter that blanked a fragment mid-URL would leave
	// urlIsSafe deciding about a string that no longer represents the
	// value anyone supplied.
	sheltered := urlScanRegexp.ReplaceAllStringFunc(s, func(m string) string {
		if urlIsSafe(m) {
			return ""
		}
		return m // something inside it is token-shaped; let the value rules take it
	})
	sheltered = secretRefScanRegexp.ReplaceAllString(sheltered, "")
	sheltered = digestPinScanRegexp.ReplaceAllString(sheltered, "")
	sheltered = commitRefScanRegexp.ReplaceAllString(sheltered, "")
	sheltered = eventIDRefScanRegexp.ReplaceAllString(sheltered, "")
	sheltered = kindRefScanRegexp.ReplaceAllStringFunc(sheltered, func(m string) string {
		if kindRefIsSafe(m) {
			return ""
		}
		return m // a secret-shaped name is not sheltered here either
	})
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
		if isSeparatedIdentifier(m) {
			continue // a name, not a value (see isSeparatedIdentifier)
		}
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
	// URLs first (see urlScanRegexp): a plan step naming where a service
	// posts is one a human has to be able to read. Under their own marker,
	// so that kvSecretRegexp still redacts one sitting under a
	// credential-shaped key.
	var urls []string
	protected := urlScanRegexp.ReplaceAllStringFunc(s, func(m string) string {
		if !urlIsSafe(m) {
			return m
		}
		urls = append(urls, m)
		return fmt.Sprintf("%s%d\x00", urlSentinelMarker, len(urls)-1)
	})
	protected = secretRefScanRegexp.ReplaceAllStringFunc(protected, func(m string) string {
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
	// And household event/correlation ids named by their key (see
	// eventIDRefScanRegexp).
	protected = eventIDRefScanRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		refs = append(refs, m)
		return fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, len(refs)-1)
	})
	// And Kind/name resource references (see kindRefScanRegexp), unless
	// the name half is itself secret-shaped (F36).
	protected = kindRefScanRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		if !kindRefIsSafe(m) {
			return m
		}
		refs = append(refs, m)
		return fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, len(refs)-1)
	})

	var paths []string
	// Absolute filesystem paths (see absPathScanRegexp). Last of the
	// shelters, so a path that is part of a URL or a digest pin has
	// already been taken by the more specific rule that understands it.
	protected = absPathScanRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		if !pathIsSafe(m) {
			return m
		}
		paths = append(paths, m)
		return fmt.Sprintf("%s%d\x00", pathSentinelMarker, len(paths)-1)
	})

	protected = hexTokenRegexp.ReplaceAllString(protected, Placeholder)
	// An identifier is a name, not a value, and must survive to be read
	// (see isSeparatedIdentifier). This declines the replacement in place
	// rather than sheltering behind a sentinel on purpose: kvSecretRegexp
	// runs after, over this same text, and it has to keep seeing both the
	// real key ("DB_PASSWORD", itself identifier-shaped) and any value
	// that only its keyword rule can catch.
	protected = base64TokenRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		if isSeparatedIdentifier(m) {
			return m
		}
		return Placeholder
	})
	protected = kvSecretRegexp.ReplaceAllStringFunc(protected, func(m string) string {
		sub := kvSecretRegexp.FindStringSubmatch(m)
		// A value that is (or contains) a protected secret/<name> sentinel is
		// a reference, not a secret value — leave the whole match intact so
		// the reference is restored verbatim below, rather than redacting the
		// sentinel and silently dropping the reference.
		if strings.Contains(sub[3], secretRefSentinelMarker) {
			return m
		}
		return sub[1] + sub[2] + Placeholder
	})

	for i, ref := range refs {
		placeholder := fmt.Sprintf("%s%d\x00", secretRefSentinelMarker, i)
		protected = strings.ReplaceAll(protected, placeholder, ref)
	}
	// A URL sentinel that kvSecretRegexp replaced with the placeholder is
	// simply not here to restore, which is the intended outcome: the key
	// said the whole value was a credential.
	for i, u := range urls {
		placeholder := fmt.Sprintf("%s%d\x00", urlSentinelMarker, i)
		protected = strings.ReplaceAll(protected, placeholder, u)
	}
	// Same for paths, and for the same reason: one the keyword rule
	// replaced is not here to restore.
	for i, path := range paths {
		placeholder := fmt.Sprintf("%s%d\x00", pathSentinelMarker, i)
		protected = strings.ReplaceAll(protected, placeholder, path)
	}
	return protected
}
