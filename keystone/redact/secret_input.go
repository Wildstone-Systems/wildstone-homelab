package redact

import "regexp"

// SecretSubmission recognizes explicit credential statements, not arbitrary
// sensitive facts. Unlabelled free text is not a reliable secret-input format;
// protected entry and /secret are the supported paths for those values.
var secretStatement = regexp.MustCompile(`(?i)\b(password|passphrase|pin|alarm code|api key|access token|credit card|card number)\b["']?\s*(?:is|:|=)\s*\S+`)

func SecretSubmission(s string) bool { return secretStatement.MatchString(s) }
func SecretText(s string) string {
	if SecretSubmission(s) {
		return "[Credential statement omitted. Use the protected Secrets page to store or reveal a value.]"
	}
	return s
}
