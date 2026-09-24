// Package conformance is the ADR-W039 §4 runtime contract turned into
// runnable checks, against ANY agentapi.Client — Wildstone's own two
// (keystone/agentgateway.InProcess, keystone/agentapi/httpclient.Client)
// and, unmodified, a third party's (ADR-W039 §8 Phase 5, #343).
//
// It exists so proving "this transport behaves like the in-process
// gateway" never again means hand-copying a script between two ad hoc
// tests (as keystone/api/handlers_agent_test.go did through Phase 4):
// one table of named checks, each traced to the runtime-contract MUST
// it evidences, run against a Target built from nothing but the
// keystone/agentapi types every implementation already speaks.
//
// # Running the suite against Wildstone's own implementations
//
// Inside this repository, build a Target from an agentgateway.Gateway or
// an httpclient.Client and call Run — see
// keystone/api/handlers_agent_test.go's own TestInProcessGatewayIsConformant
// and TestHTTPClientIsConformant for the two worked examples: a fresh
// database and Agent, a handle-issuing closure, and the tool names the
// fixture actually offers.
//
// # Running the suite against an external runtime's Wildstone-compatible server
//
// A runtime author who has built their own agentapi.Client — because
// they operate a Wildstone-compatible server for it to call, or because
// they are proving their own gateway forwards to a real Keystone
// correctly — runs this suite two ways, neither of which needs a fork of
// this package or of Keystone:
//
//  1. Import keystone/agentapi/conformance and keystone/agentapi/httpclient
//     from their own module's test file, build a Target with
//     &httpclient.Client{BaseURL: ..., Bearer: ...} as both Client and
//     Deliveries, and call conformance.Run(t, target) from their own
//     `go test`. This needs no build tag and no flag of Keystone's: it is
//     an ordinary dependency on two packages that import nothing under
//     keystone except each other and the standard library (see this
//     package's and httpclient's own structural tests).
//
//  2. From inside this repository, `go test -tags conformance_external
//     ./keystone/agentapi/conformance/... -target-url <base URL>` runs
//     example_external_test.go's TestExternalRuntimeIsConformant against
//     a live, already-deployed Keystone (or a Wildstone-compatible
//     stand-in) from CI. The tag keeps that test out of every ordinary
//     `go test ./...`, since it dials a network address the build itself
//     never should. -target-url is the only flag: NEVER a bearer, which
//     that test reads from the KEYSTONE_RUNTIME_BEARER environment
//     variable and never logs, prints, or echoes into a t.Log/t.Error —
//     a flag value can land in a shell history or a process listing, an
//     environment variable read once cannot, and ADR-W040 §7's own rule
//     ("no secret in URLs; redaction in logs") applies to how this suite
//     is invoked, not only to what Keystone itself does.
//
// Either way, the runtime author supplies a live task handle for their
// own Agent (KEYSTONE_CONFORMANCE_HANDLE for route 2) — this package
// cannot mint one itself without importing a Wildstone internal it must
// never depend on (structural_test.go) — and, optionally, the name of a
// read-only tool and a propose_change-shaped tool their deployment
// actually offers, so the invoke checks have something real to call.
// Every check that needs a fixture-specific probe has a corresponding
// Target field with a doc comment saying so; a check whose Target field
// is left empty skips itself with a reason, rather than failing on a
// property this package cannot assume every deployment has (see Target's
// own doc comment).
package conformance
