// Package agentruntime is the small, leaf vocabulary shared between
// keystone/butler (which keeps the two tool gateways, ToolGateway and
// HouseholdGateway, and the legacy closure-driven Session) and
// keystone/agentruntime/native (which holds the durable reasoning loop,
// Runner, now wired to keystone/agentapi instead of a Toolset/Ledger
// pair — ADR-W039 Phase 2b, #337).
//
// It holds no internals of its own: no database, no statefile, no
// Toolset. It exists because a handful of types are genuinely needed on
// both sides of the Phase 2 split — the audit record a turn returns
// (ToolCallRecord, Audit, Transcript) and the ledger's own step/trace
// vocabulary (LedgerStep and friends, the tool-step result envelope,
// the objective-classification seam) — and two independent copies of
// one shared shape is how the two sides silently disagree. Package
// keystone/butler re-exports every name here as a type alias or a
// redeclared constant, so moving the definitions changed no call site.
//
// The names here are careful never to assert anything about authority:
// they are recording shapes, not decisions.
package agentruntime
