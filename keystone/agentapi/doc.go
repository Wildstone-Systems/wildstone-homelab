// Package agentapi is the canonical Agent API contract (ADR-W039): the
// resource-family types a runtime exchanges with Wildstone, and the
// client interface a runtime calls them through.
//
// One contract, all runtimes (ADR-W039 §3.1). The native runner and an
// external runtime — a Hermes profile, an OpenClaw gateway, whatever
// comes next — consume exactly these types. Nothing the native runner
// can do is unavailable to an external runtime, and nothing an external
// runtime is denied reaches the native runner by a side door. That is
// what makes ground rule 3 a transport property rather than a promise.
//
// The chain stays in Wildstone. Every call carries a Wildstone-issued
// task Handle, and Wildstone derives the Person, Connection, audience
// and budget from it at every call (§3.4). A runtime never asserts who
// is asking, never holds an integration credential, and never sends a
// message directly. So the types here carry what a runtime may SAY —
// which tool, which arguments, what it remembered, where it is in a
// task — and never what Wildstone DECIDES. There is no audience field a
// runtime can widen, no permission label it can invent, no principal it
// can name.
//
// This package must never import Wildstone internals: not the runtime
// database, the Statefile, the domestic executors, the Butler or its
// provider, the store or the API. It is the boundary those packages
// sit behind, and a boundary that reaches inward is a door. The
// structural test pins that, and Phase 2 of the ADR's migration plan
// relies on it: the in-process native client and an HTTP client are
// interchangeable only while nothing here knows which one it is.
//
// Phase 0 (ADR-W039 §8) names the contract and wires nothing. Every
// type mirrors something that already exists — the Toolset's offered
// specs, the ledger's steps and attempts, the memory store's scopes —
// so the later phases adapt rather than redesign.
package agentapi
