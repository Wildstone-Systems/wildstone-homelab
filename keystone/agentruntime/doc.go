// Package agentruntime is the line between Wildstone and the loop that
// reasons for an agent. It holds two things, and no internals of its
// own: no database, no statefile, no Toolset.
//
// The AgentRuntime contract (ADR-W041, #357). Wildstone is an agent
// platform, not an agent framework. Wildstone owns everything on this
// side of the line — job lifecycle, identity, tool authorization, audit,
// timeouts, cancellation and runtime selection — and a runtime owns only
// the model's reasoning loop on the other side. The native adapter
// (keystone/agentruntime/native) is the only implementation of
// AgentRuntime; it delegates the loop to the OpenAI Agents SDK. External
// runtimes (Hermes, OpenClaw) do not implement this interface: they are
// Agent API clients (ADR-W039, ADR-W040), bounded at the gateway by task
// handle, budget, attempt fencing and heartbeat TTL. Nothing in this package names a model provider or an SDK type, and
// nothing may: an adapter that needs one keeps it behind its own
// boundary.
//
// The shared ledger vocabulary (ADR-W039 Phase 2b, #337). A handful of
// types are needed both by keystone/butler (which keeps the two tool
// gateways, ToolGateway and HouseholdGateway, and the closure-driven
// Session) and by keystone/agentruntime/legacy (Wildstone's own durable
// reasoning loop, Runner, wired to keystone/agentapi rather than a
// Toolset/Ledger pair): the audit record a turn returns (ToolCallRecord,
// Audit, Transcript) and the ledger's step/trace vocabulary (LedgerStep
// and friends, the tool-step result envelope, the objective-
// classification seam). Two independent copies of one shared shape is
// how two sides silently disagree, so they live here once. Package
// keystone/butler re-exports every name as a type alias or a redeclared
// constant, so moving the definitions changed no call site.
//
// The names here are careful never to assert anything about authority:
// they are recording shapes, not decisions.
package agentruntime
