package butler

import ksredact "github.com/chickenrat17/wildstone-homelab/keystone/redact"

// redact delegates to the shared keystone/redact package — ADR-W007 A5
// moved the S4 implementation there so the public API's response encoder
// and the Butler's tool gateway share one redaction layer. The
// package-local name keeps ToolGateway.Dispatch and the S4 redaction
// tests reading unchanged.
func redact(s string) string { return ksredact.String(s) }

// redactedPlaceholder mirrors the shared package's placeholder for the S4
// hardening tests that assert on it.
const redactedPlaceholder = ksredact.Placeholder
