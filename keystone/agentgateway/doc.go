// Package agentgateway is the Wildstone side of the Agent API
// (ADR-W039 §3.4, §7.1): the thing a runtime calls when it wants a
// capability, a memory, a checkpoint or a proposal, and the thing that
// decides — from a Wildstone-issued task handle and nothing the
// runtime says — whether it may have it.
//
// It implements agentapi.Client in-process. Every method resolves the
// handle it was given to a task, a Person, a Connection, an audience
// and a Butler (butlerstore.Ledger.ResolveHandle), builds the same
// Toolset butlerlive.Serve builds for that Butler, binds the same
// per-Butler authority under the same Connection ceiling, and then
// does what the native runner did through its closures: asks the
// Toolset what it offers (Manifest), or runs one tool with the ledger
// checkpoints around it (Invoke). The runner's operation.Identity is
// derived here from the handle; the runner never constructs one
// (removes C4 once the runner is switched).
//
// Phase 2a (this package's first commit) switches no caller. Serve and
// the Runner keep their own code paths; this package is exercised by
// its tests, and the parts of serve.go and runner.go it mirrors are
// marked duplicated-until-2b so the switch can delete them. What is
// owned here from now on: the conversation limits (rounds, tool calls,
// observation TTL), the verified-read cache semantics (a resume serves
// a verified reading from the ledger, re-checking its authority, until
// the reading ages out), and the ledger envelope a tool step is stored
// in.
//
// Methods whose Wildstone-side behaviour a later phase supplies —
// Recall's structured hits, Propose's in-gateway filing, Notify —
// return agentapi.NotImplementedError naming that phase. Nothing here
// applies anything: the package imports neither keystone/reconcile nor
// keystone/api, and the structural test pins both.
package agentgateway
