package mailmonitor

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// linkPattern matches anything a calendar client would render as a link.
// Mail is written by strangers; a link in an event's notes is a link the
// household did not choose, so notes carry none (and nothing fetches
// one — link fetching is out of scope for V1).
var linkPattern = regexp.MustCompile(`(?i)\b(?:https?|ftp|mailto|javascript|data|file|tel|sms):\S+|\bwww\.\S+`)

// safeText is calendar-safe text: printable, no format or control
// characters (zero-width, bidi overrides), no links, collapsed
// whitespace, bounded to max bytes. keepLines keeps single line breaks
// (notes); otherwise everything is one line (title, location).
func safeText(s string, max int, keepLines bool) string {
	s = linkPattern.ReplaceAllString(s, "[link removed]")
	var b strings.Builder
	space := false
	newline := false
	for _, r := range s {
		switch {
		case r == '\n' && keepLines:
			newline = true
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == unicode.ReplacementChar:
			continue
		}
		if b.Len() > 0 {
			switch {
			case newline:
				b.WriteByte('\n')
			case space:
				b.WriteByte(' ')
			}
		}
		space, newline = false, false
		b.WriteRune(r)
	}
	return strings.TrimSpace(runtimedb.BoundMailText(b.String(), max))
}

// key is a connection-scoped digest of a header value. The database
// stores these, never the values: they are enough to match a correction
// or a cancellation to what it corrects, and meaningless across
// connections.
func key(label, connection, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("wildstone-mail-key/v1\x00" + label + "\x00" + connection + "\x00"))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

// subjectPrefix matches reply, forward and correction markers.
var subjectPrefix = regexp.MustCompile(`(?i)^\s*(?:re|fw|fwd|aw|wg|sv|correction|corrected|update|updated|change|changed|` +
	`revised|rescheduled|reminder|new time|time change|date change|invitation|updated invitation)\s*[:\-–]\s*`)

// normalizeSubject strips every leading reply/forward/correction marker
// and brackets, and folds case and space, so "Correction: Re: Family
// night" and "Family night" key alike.
func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		t := subjectPrefix.ReplaceAllString(s, "")
		t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "[EXTERNAL]"))
		if t == s {
			break
		}
		s = t
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// correctionMarker matches a subject or opening that says it corrects an
// earlier message. Deterministic, so a correction is recognised even
// when no model reads the mail.
var correctionMarker = regexp.MustCompile(`(?i)\b(?:correction|corrected|updated?\s+(?:time|date|details|invitation)|` +
	`time\s+change|date\s+change|change\s+of\s+(?:time|date|venue|location)|rescheduled|new\s+time|new\s+date|revised)\b`)

// dateEvidence and timeEvidence are the deterministic floor under model
// extraction: a date or time the model reports must have SOME textual
// basis in the message, or it is marked ambiguous rather than trusted.
var (
	dateEvidence = regexp.MustCompile(`(?i)\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+\d{1,2}\b|` +
		`\b\d{1,2}(?:st|nd|rd|th)?\s+(?:of\s+)?(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\b|` +
		`\b\d{4}-\d{2}-\d{2}\b|\b\d{1,2}/\d{1,2}(?:/\d{2,4})?\b|` +
		`\b(?:mon|tues?|wed(?:nes)?|thu(?:rs?)?|fri|sat(?:ur)?|sun)(?:day)?\b|\b(?:today|tomorrow|tonight)\b`)
	timeEvidence = regexp.MustCompile(`(?i)\b\d{1,2}(?::\d{2})?\s*(?:a\.?m\.?|p\.?m\.?)|\b\d{1,2}:\d{2}\b|\b(?:noon|midday|midnight)\b|` +
		`\b\d{1,2}h\d{2}\b`)
)
