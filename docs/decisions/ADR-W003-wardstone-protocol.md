# ADR-W003: Wardstone protocol — typed no-exec surface, layout, versioning

- **Status:** accepted (locked decision #1 of proposal §10, produced by Spike S1)
- **Relates to:** architecture proposal §4, implementation scope S1/E1/E2

## Context

Wardstone is the only privileged component; everything that touches the
host crosses its wire boundary. Spike S1 built the protocol and both ends
of it (agent + Keystone gateway) to force the layout/versioning decisions.

## Decisions

### 1. Typed, closed, no-exec operation surface

The operation surface is a single protobuf `oneof` (`wardstone.v1.Operation`).
There is no `Exec(string)`, no "run this compose file", no free-form
escape hatch — "no arbitrary shell" is a property of the wire format.
Adding privilege requires adding a typed arm to the proto, reviewed like
any schema change. Agent-side invariants back this up in depth (managed
prefixes, `dev.wildstone.owner` label guard, digest-pinned images) so even
a compromised Keystone cannot exceed the typed surface. The S1 test suite
asserts each invariant; those tests are security documentation and must
never be deleted without an ADR superseding this one.

### 2. Wire layout

- One proto package per major version: `wardstone.v1` in
  `proto/wardstone/v1/wardstone.proto`; generated Go committed under
  `proto/gen/` (module `.../proto`), consumed by both `wardstone` and
  `keystone` via workspace + `replace`.
- `KeystoneGateway` service, implemented by Keystone. Two RPCs:
  - `Join(JoinRequest) → JoinResponse`: one-time bootstrap — join token +
    CSR in, CA-issued client cert + pinned CA + (future) op-signing key out.
  - `Session(stream AgentMessage) ↔ (stream KeystoneMessage)`: the agent's
    persistent dial-out stream. First agent message is `Hello`
    (node id, agent version, protocol minor); afterwards operations flow
    Keystone→agent correlated by `op_id`, results flow back. Results may
    complete out of order; correlation is the contract, not ordering.
- Errors are typed (`OpError` + `ErrorCode`), never stringly-typed status:
  `FORBIDDEN` (invariant refused), `VERIFICATION` (signature/digest),
  `INVALID`, `NOT_FOUND`, `RUNTIME`.

### 3. Versioning and compatibility

- Within `v1`: additive-only. New fields and new `oneof` arms are allowed;
  renumbering, retyping, or removing anything is not.
- An agent receiving an unknown operation arm returns
  `INVALID` ("protocol skew") rather than crashing — that is the graceful
  half of the skew story.
- `Hello.protocol_minor` announces the agent's revision; Keystone must
  tolerate agents **one minor revision behind** (the proposal's
  no-lockstep-upgrades guarantee). Keystone simply doesn't send arms the
  agent's minor doesn't have.
- Breaking changes mean a `wardstone.v2` package served side by side
  during a migration window.

### 4. Identity and trust (spike state, hardening path)

- mTLS everywhere after bootstrap: Keystone's embedded CA issues per-node
  client certs (CN = node id, forced by the gateway — never taken from
  the CSR). `Session` refuses connections without a verified client cert;
  join tokens are single-use.
- The `Hello.node_id` must match the certificate CN, so a stolen cert
  cannot impersonate another node.
- Spike simplifications, to harden in E1/E2 (tracked, not forgotten):
  - Join uses trust-on-first-use for the server; production installers
    deliver a CA fingerprint instead.
  - CA is in-memory per process; E2 persists it in Keystone's data dir.
  - `KeystoneMessage.op_signature` (anti-MITM op signing) is reserved in
    the wire format but not yet enforced.
  - Cert rotation is not implemented (certs are 3-month to force it early).

### 5. Self-update shape

`SelfUpdate` carries the signed binary inline (sha256 + ed25519 over the
digest, verified against a baked-in key). The agent stages A/B
(`wardstoned-next`), keeps one rollback generation (`wardstoned-prev`),
and promotes atomically. Re-exec + health-gate + auto-revert are the
systemd-watchdog half and belong to E1; chunked/fetched delivery replaces
inline bytes when binaries outgrow a message (E1).

## Consequences

- Every new host-side ability is a proto change + agent handler + test —
  deliberate friction, exactly where the security model wants it.
- Generated code is committed, so builds need no protoc; regeneration is
  documented in `proto/README.md` and only needed when the .proto changes.
- The gateway is importable (`keystone/gateway`) so S2's reconciler drives
  agents through the same code path the S1 tests exercise.
