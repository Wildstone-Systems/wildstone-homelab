// Package butler is the Wildstone Butler's tool gateway, redaction layer,
// untrusted-content envelope, and agent loop (ADR-W006, Spike S4, work
// package S4b).
//
// # Structural safety claim
//
// Authority does not come from the model (ADR-W006 A5.1). This package
// gives the Butler exactly two capabilities over live Keystone state:
//
//   - Read: get_system_overview, get_capability_status, get_health,
//     get_plan, explain_resource — all always allowed, all run over real
//     S2 state (Statefile, plan.Diff, a minimal read-only Wardstone-op
//     reader).
//   - Propose: propose_change(spec_diff) — builds a resource.Resource from
//     the model's requested change and runs plan.Diff against the current
//     Statefile contents. It returns a plan.Plan. It never writes to the
//     Statefile and never dispatches a mutating Wardstone operation.
//
// This is enforced structurally, not by convention: this package (outside
// _test.go files) does not import keystone/reconcile at all — not
// reconcile.Applier, not reconcile.Reconciler, not reconcile.Reconcile. The
// only network-facing capability it holds is agentReader (reader.go), which
// wraps a *gateway.Gateway but exposes exactly two methods — Health and
// ContainerStats — each constructing only its own read-only wsv1.Operation
// variant. There is no method, field, or code path in this package that can
// construct an EnsureContainer/StopContainer/RemoveContainer/EnsureVolume/
// WriteManagedFile op or call gateway.Gateway.Dispatch with anything other
// than Operation_Health/Operation_ContainerStats. Approval and apply stay
// exactly where ADR-W004 put them: a human, `wild`, or the Console, working
// from the Plan this package returns.
//
// Every tool result — from any tool, read or propose — passes through a
// single centralized redact() call in ToolGateway.Dispatch before it is
// handed back to the caller (redact.go): a new tool cannot forget this,
// because tools never return directly to the model, only through Dispatch.
// Attacker-influenceable content (currently: get_health's agent-reported
// detail string) is additionally wrapped in a delimited
// <untrusted_data>...</untrusted_data> envelope (envelope.go) the system
// prompt tells the model to treat as inert data, never as instructions.
//
// Real-model operation (a live Anthropic or Ollama backend from
// keystone/butler/provider) is an owner-run step, same posture as S4a — see
// keystone/butler/README.md. This package's own tests use a scripted fake
// Provider exclusively.
package butler
