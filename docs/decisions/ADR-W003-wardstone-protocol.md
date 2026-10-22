# ADR-W003: Wardstone protocol — typed no-exec surface, layout, versioning

- **Status:** accepted (locked decision #1 of proposal §10, produced by Spike S1); ADR-W030 constraints amended by ADR-W033
- **Relates to:** architecture proposal §4, implementation scope S1/E1/E2
- **Governed by:** ADR-W033 where they overlap.
- **Audience:** this document is both the decision record AND the
  implementation specification for finishing Wardstone to production grade
  (E1). It is written so that implementation subagents can execute work
  packages from Part C without access to the planning conversation. If a
  work package contradicts Part A or B, Part A/B wins — stop and report
  the conflict instead of improvising.

## Constraints from ADR-W030

- The Butler log reader adds no protocol operation: it reuses the existing
  typed `ContainerLogs` arm, ownership checks, and 1 MiB tail-biased bound.
  The Butler gateway and reader narrow and redact that result. The reader is
  an Owner-class tool, grantable only to Owner Agents.
  _Changed by ADR-W033 §B3: tool authority follows tool class, not surface._
- Automatic adapter fallback requires verified removal of an integration
  connection secret (an app adapter's credential, not an ADR-W033
  Connection) from a node leaving the approved placement set. Before that
  feature ships, implementation must add one typed, idempotent, additive
  `RemoveSecret` arm carrying secret name and expected version and returning
  only whether that version existed/was removed. It never returns a value,
  refuses a version mismatch, remains Plan-mediated with human Owner
  approval, and refuses while any managed container on the node still
  references that secret. Adapter placement cleanup fences the lease and
  stops/removes the container before erasure so it cannot retain or
  rematerialize the value. The operation follows A3's protocol-minor
  compatibility process. An equivalent typed erasure operation is acceptable
  only if it preserves those properties.

---

# Part A — Decisions (binding, do not re-litigate)

### A1. Typed, closed, no-exec operation surface

The operation surface is a single protobuf `oneof` (`wardstone.v1.Operation`).
There is no `Exec(string)`, no "run this compose file", no free-form
escape hatch — "no arbitrary shell" is a property of the wire format.
Adding privilege requires adding a typed arm to the proto, reviewed like
any schema change. Agent-side invariants back this up in depth (§A4) so
even a compromised Keystone cannot exceed the typed surface.

### A2. Wire layout

- One proto package per major version: `wardstone.v1` in
  `proto/wardstone/v1/wardstone.proto`; generated Go committed under
  `proto/gen/` (module `.../proto`), consumed by both `wardstone` and
  `keystone` via workspace + `replace`.
- `KeystoneGateway` service, implemented by Keystone. Two RPCs today:
  - `Join(JoinRequest) → JoinResponse`: one-time bootstrap — join token +
    CSR in, CA-issued client cert + pinned CA + op-signing key out.
  - `Session(stream AgentMessage) ↔ (stream KeystoneMessage)`: the agent's
    persistent dial-out stream. First agent message is `Hello`; afterwards
    operations flow Keystone→agent correlated by `op_id`, results flow
    back. Results may complete out of order; correlation is the contract,
    not ordering.
- Errors are typed (`OpError` + `ErrorCode`), never stringly-typed status:
  `FORBIDDEN` (invariant refused), `VERIFICATION` (signature/digest),
  `INVALID`, `NOT_FOUND`, `RUNTIME`.

### A3. Versioning and compatibility

- Within `v1`: **additive-only**. New fields and new `oneof` arms are
  allowed; renumbering, retyping, renaming, or removing anything is not.
- An agent receiving an unknown operation arm returns `INVALID`
  ("protocol skew"), never crashes.
- `Hello.protocol_minor` announces the agent's revision; Keystone must
  tolerate agents **one minor revision behind**. Bump `agent.ProtocolMinor`
  whenever a new op arm or semantic is added.
- Breaking changes mean a `wardstone.v2` package served side by side.

### A4. Agent-side hard invariants (security floor)

Enforced inside the agent regardless of what Keystone asks. The S1 tests
asserting these are **security documentation**: they may be extended,
never weakened or deleted, except by an ADR superseding this one.

1. **Managed prefixes** — file reads/writes and host-path mounts only
   under configured prefixes (prod default `/etc/wildstone`,
   `/srv/wildstone`). Paths must be absolute; `..` and symlink escapes are
   resolved and refused (`resolveManaged` in `ops.go`).
2. **Digest pinning** — container images must match
   `^[a-zA-Z0-9][a-zA-Z0-9./_:-]*@sha256:[a-f0-9]{64}$`. Tags are refused
   with `FORBIDDEN`.
3. **Ownership label** — every container Wardstone creates gets
   `dev.wildstone.owner=wildstone`; every mutating/reading container op
   first inspects and refuses (`FORBIDDEN`) anything without that label.
4. **Bounded responses** — logs capped at 1 MiB (tail-biased); any new op
   returning bulk data must define and enforce a cap.
5. **No shell-outs, ~zero dependencies** — the wardstone module may
   depend on stdlib + grpc/protobuf + the proto module ONLY. Docker is
   spoken via the tiny stdlib Engine-API client in `docker.go`. Adding any
   other dependency to `wardstone/go.mod` requires explicit sign-off from
   the platform owner — report back instead of adding.

### A5. Identity and trust

- mTLS everywhere after bootstrap; TLS 1.3 minimum on both ends.
- Keystone's embedded CA issues per-node client certs. The cert CN **is**
  the node identity and is forced to the join request's node id by the
  gateway — never taken from the CSR subject.
- `Session` requires a verified client cert (checked per-RPC because the
  TLS layer runs `VerifyClientCertIfGiven` to let `Join` through);
  `Hello.node_id` must equal the cert CN.
- Join tokens are single-use.
- Client certs are issued with ~3-month lifetime deliberately, to force
  rotation work (W4) before it becomes an outage.

### A6. Self-update shape

`SelfUpdate` carries sha256 + ed25519-over-digest, verified against a key
baked into the agent. A/B staging: write `<binary>-next`, keep one
rollback generation `<binary>-prev`, promote by atomic rename. The
re-exec + health-gate + auto-revert half is W5; chunked delivery replaces
inline bytes when binaries outgrow a message (also W5).

---

# Part B — Implementation state and codebase contract

### B1. What exists (S1, all tested and passing)

| Path | Content |
|---|---|
| `proto/wardstone/v1/wardstone.proto` | The protocol. Regeneration: see `proto/README.md`. Generated Go is committed under `proto/gen/`. |
| `keystone/gateway/ca.go` | Embedded CA: `NewCA`, `ServerTLSCert(hosts)`, `IssueFromCSR(csr, nodeID)` (CN forced), `PEM()`, `Pool()`. In-memory per process (W2 persists it). |
| `keystone/gateway/gateway.go` | gRPC server: `New(Config)`, `Serve(lis)`, `Stop()`, `Join`, `Session`, `Dispatch(ctx, nodeID, op)`, `Connected`, `WaitConnected`. Session registry keyed by cert CN; pending ops correlated by op_id via channels. |
| `keystone/cmd/keystone/main.go` | Flags: `--listen`, `--join-tokens`, `--hosts`. Runs the gateway. |
| `wardstone/internal/agent/agent.go` | `Config` + `New` + `Run(ctx)`: join-if-no-identity, then session loop with full-jitter backoff (`BackoffBase`/`BackoffCap`). Identity files: `node.crt`, `node.key`, `keystone-ca.crt` in `DataDir` (0600). Join is TOFU (W3 replaces). |
| `wardstone/internal/agent/ops.go` | `dispatch` (op → handler switch), `resolveManaged`, all invariant enforcement, container/file handlers. |
| `wardstone/internal/agent/docker.go` | Stdlib Docker Engine API client. `unix://` socket or `http(s)://` (tests). Treats 304 as success for start/stop; wraps errors as `*dockerError` with `notFound()`. |
| `wardstone/internal/agent/facts_linux.go` / `facts_other.go` | HostFacts (linux real, other stub — keep both building). |
| `wardstone/internal/agent/selfupdate.go` | Verify + A/B stage/promote (§A6). |
| `wardstone/internal/agent/integration_test.go` | Real gateway + real agents over localhost mTLS: lifecycle, token misuse, cert-less refusal, all §A4 invariants, self-update happy/tampered, 20-agent reconnect storm through gateway restart. |
| `wardstone/internal/agent/fakedocker_test.go` | In-memory Docker API fake (create/inspect/start/stop/logs/delete/images-create) with real status-code semantics (409 conflict, 304 not-modified, 404). Rejects un-pinned pulls with 400 — keep that tripwire. |
| `wardstone/cmd/wardstoned/main.go` | Flags: `--node-id`, `--keystone`, `--data-dir`, `--join-token`. `version` var set via `-ldflags`. |

Module graph: `wardstone` → {`proto`, `keystone`} and `keystone` →
`proto`, via `replace ../...` directives + `go.work`. The
keystone-in-wardstone dependency exists **for the integration tests
only**; do not import `keystone/gateway` from non-test agent code.

### B2. Build/verify contract (what "done" means for every work package)

Run from the repo root; all must pass:

```sh
export GOTOOLCHAIN=go1.25.0   # local toolchain may be older
gofmt -l ./proto ./wardstone ./keystone ./cli ./testing/e2e   # must print nothing
for m in proto wardstone keystone cli testing/e2e; do (cd "$m" && go vet ./... && go build ./... && go test ./...); done
cd wardstone && go test -race -count=1 ./...                  # race-clean is mandatory
```

Also: CI runs gitleaks — never commit private keys or certs, including in
testdata (tests must generate keys/certs at runtime, as the S1 tests do).

### B3. Known traps (each cost debugging time in S1 — do not rediscover)

1. **Generated oneof wrapper interfaces are unexported.** You cannot
   declare your own Go interface matching `isOpResult_Result` in another
   package. Handlers return concrete wrapper types
   (`*wsv1.OpResult_XxxYyy`) and `dispatch` assigns them per-case. Follow
   the existing pattern when adding ops.
2. **`go build ./...` inside a module directory writes stub binaries**
   (`wardstone/wardstoned` etc.) into the tree. They are gitignored by
   exact path — if you add a new main, add its path to `.gitignore`.
3. **Run `gofmt -w` before finishing.** Struct-field alignment after edits
   is the usual offender; CI fails on unformatted code.
4. **Workspace vs module mode:** `go mod tidy` works because of the
   `replace` directives; keep them in sync if modules move. New modules
   must be added to `go.work` AND to the four loops in
   `.github/workflows/ci.yml` (fmt/vet/build/test).
5. **protoc plugins:** install with `GOTOOLCHAIN=go1.25.0 go install ...`
   (two separate `go install` commands — a combined one fails), commands
   in `proto/README.md`. Commit regenerated `gen/` output in the same
   change as the `.proto` edit.
6. **grpc start/stop semantics:** Docker returns 304 when a container is
   already started/stopped — that is success, not an error.
7. **Reconnect tests:** re-listen on `h.addr` and reuse the same `Gateway`
   object (same CA) — a fresh CA would be correctly rejected by agents
   that pinned the old one, and the test would hang.

---

# Part C — Work packages to production grade (E1)

Rules of engagement for implementation subagents:

- One work package per change; do them in the listed order unless told
  otherwise (W1–W3 are ordered by dependency; W4–W7 are parallelizable
  after W2).
- Additive proto changes only (§A3); bump `ProtocolMinor` when you add an
  op arm and say so in your report.
- Every package lands with tests meeting §B2, including new invariant
  tests where the package touches security behavior.
- Report back: what changed (files), proto field/arm numbers consumed,
  test names added, anything you had to decide that this spec didn't pin
  down (flag it — the planner reviews those), and any deviation from spec
  with the reason.
- If a package turns out to require a non-additive proto change, a new
  dependency, or weakening an §A4 invariant: STOP, report, do not commit.

### W1 — Per-operation signing (anti-MITM)

Goal: a MITM on the transport (or a compromised load balancer in a future
multi-node world) cannot inject operations even inside a valid TLS tunnel.

- Keystone: generate an ed25519 signing keypair in `gateway.New` (persist
  in W2's data dir alongside the CA; until W2 lands, in-memory is fine —
  coordinate field layout with W2's owner). Serve the public key in
  `JoinResponse.op_signing_public_key` (field exists).
- Signing: for each dispatched op, sign
  `sha256("wardstone-op-v1" || op_id || deterministic-marshal(Operation))`
  and place it in `KeystoneMessage.op_signature` (field 3, reserved for
  this). Deterministic marshal = `proto.MarshalOptions{Deterministic: true}`.
  The domain-separation prefix `"wardstone-op-v1"` is part of the wire
  contract — document it in the .proto comment.
- Agent: persist the key from join as `op-signing.pub` in `DataDir`
  (0600). If the file exists, EVERY received operation must verify or be
  answered with `ERROR_CODE_VERIFICATION` (and logged at Warn). If the
  file does not exist (pre-W1 join), accept unsigned but log at Warn once
  per session — and the agent must re-pin on next join. No config flag to
  disable verification.
- Tests: (a) signed op verifies and executes; (b) tampered op_id, tampered
  op bytes, and missing signature each → `VERIFICATION`, op not executed
  (prove no side effect, e.g. file not written); (c) agent without pinned
  key still works and logs the warning.

### W2 — Keystone persistence (CA, signing key, join tokens)

Goal: Keystone restart must not orphan every agent (today a restart mints
a new CA and all agents' pinned CAs go stale).

- Add `DataDir string` to `gateway.Config`. Layout:
  `ca.crt`, `ca.key`, `op-signing.key`, `op-signing.pub`, `tokens.json`.
  Files 0600, dir 0700. Load-if-exists, create-if-missing, atomic writes
  (tmp + rename, same pattern as `writeManagedFile`).
- `tokens.json`: `{token: {"expires": RFC3339, "used": bool}}`. Add
  `MintJoinToken(ttl) (string, error)` — 32 bytes crypto/rand, hex —
  and mark-used must persist before `Join` returns success (crash-safe:
  a token must never survive a crash as reusable after it was used).
- Keystone main: `--data-dir` flag (default `/var/lib/wildstone/keystone`);
  `--join-tokens` remains for dev/tests but minted tokens are the
  documented path.
- Tests: gateway restart with same DataDir keeps agents connectable (extend
  the storm test: full `Gateway` re-`New()` from the same DataDir, agents
  reconnect without rejoining); expired token refused; used-token
  persistence survives restart (kill between use and next start).

### W3 — Replace TOFU with CA pinning at join

Goal: close the join-time MITM window.

- Agent `Config.CAFingerprint string` (hex sha256 of Keystone's CA cert
  DER) + `--ca-fingerprint` flag. When set, the join dial uses
  `InsecureSkipVerify: true` **with** `VerifyPeerCertificate` comparing
  `sha256(rawCert)` of the presented leaf's root/self-signed cert against
  the fingerprint (constant-time compare). When unset, current TOFU
  behavior + a loud Warn log ("TOFU join — use --ca-fingerprint in
  production").
- Keystone: print the CA fingerprint at startup (`log.Info`) and add a
  `Fingerprint()` method on `CA` so the installer (E10) can surface it.
- Tests: correct fingerprint joins; wrong fingerprint fails BEFORE the
  join token is spent (token still valid afterwards — this matters);
  no-fingerprint TOFU still works.

### W4 — Certificate rotation

Goal: agents renew before the 3-month expiry without human involvement.

- Proto (additive): unary `rpc Renew(RenewRequest) returns (RenewResponse)`;
  `RenewRequest{bytes csr_pem}` (node id comes from the mTLS cert CN, not
  the request), `RenewResponse{bytes cert_pem}`. Requires verified client
  cert — same per-RPC check as `Session` (`peerNodeID`).
- Agent: on session establish and then daily, check cert NotAfter; renew
  when less than 1/3 of lifetime remains. New key per renewal. Write new
  key+cert atomically (tmp+rename both, then reload on next reconnect —
  forcing a reconnect after renewal is acceptable and simpler; do that).
- Gateway: `Renew` issues from CSR with CN := peer cert CN.
- Tests: agent with a short-lived cert (issue a 90-second cert via a test
  hook on CA, e.g. `IssueFromCSRWithLifetime`) renews and reconnects with
  the new cert; a `Renew` call without client cert is refused; CN cannot
  be changed via renewal (CSR with different CN → issued cert still has
  the session's CN).

### W5 — Self-update: re-exec, health gate, auto-revert, chunking

Goal: complete §A6 to the proposal's full loop.

- Proto (additive): extend `SelfUpdate` with
  `bool staged_only` (Keystone asks for stage-without-promote) and add ops
  `PromoteUpdate{}` / `RevertUpdate{}` if needed — OR implement the whole
  gate agent-side. **Decision pinned here: agent-side.** Flow:
  1. Verify + stage `-next` (existing code).
  2. Write a state file `update-state.json` in DataDir
     (`{"pending": version, "deadline": now+2m}`), promote, then re-exec
     via `syscall.Exec` (same args + env).
  3. New process: if `update-state.json` exists, it must establish a
     Keystone session before `deadline`; on success delete the state file
     (promotion confirmed) and log Info.
  4. Outer watchdog: ship `packaging/wardstoned.service` (systemd unit:
     `Restart=always`, `WatchdogSec=60`, sd_notify not required for spike
     — document that `Restart=always` + step 5 is the revert path).
  5. On startup, if `update-state.json` exists AND its deadline has
     passed (i.e. the promoted binary kept crashing until systemd
     restarted us enough times), swap `-prev` back over the binary,
     delete the state file, log Error. This self-heals because the
     reverted binary is the one that was working.
- Chunked delivery: add `SelfUpdateChunk{bytes data; uint32 seq; bool last}`
  streaming is NOT available on the existing unary-op stream — instead
  keep inline bytes but enforce/raise gRPC max message size to 64 MiB on
  both ends (`grpc.MaxRecvMsgSize`/`MaxSendMsgSize` server option +
  `grpc.MaxCallRecvMsgSize` client call option). Wardstone binaries are
  ~10–20 MiB static; 64 MiB has headroom. True chunking is deferred until
  a binary approaches the limit — note this in the .proto comment.
- Tests: full happy loop with a real re-exec (build a tiny helper binary
  in `TestMain` that phones a local listener, then exits — the test
  asserts the state file lifecycle); revert path (promote a binary that
  exits(1) immediately, simulate the deadline passing, assert `-prev`
  restored); oversized message rejected cleanly (not a stream kill).
  The re-exec test can live behind `//go:build linux`.

### W6 — Remaining operation surface (proposal §4.2, ~25 ops)

Add in four batches, each batch = one work package + tests, following the
existing handler pattern (proto arm → handler → dispatch case → fake
extension → integration test; concrete wrapper return types, §B3.1):

- **W6a Containers/Images:** RemoveContainer, ContainerStats,
  PruneImages. RemoveContainer respects ownership; Prune only prunes
  wildstone-owned (label filter in the Engine API call:
  `filters={"label":["dev.wildstone.owner=wildstone"]}`).
- **W6b Volumes/FS:** EnsureVolume (named docker volume, owner-labeled),
  SnapshotDir (tar.gz of a managed-prefix dir to a managed-prefix dest;
  no shell — use archive/tar + compress/gzip), MeasureUsage (recursive
  du of a managed prefix; cap walk at 1M entries).
- **W6c Storage/Network:** ListBlockDevices (parse /sys/block, no lsblk),
  EnsureMount (fstab-managed, mounts only under `/srv/wildstone`,
  `syscall.Mount` — flag: this one is genuinely dangerous, its FORBIDDEN
  tests are the most important in the batch), TestNfs/TestSmb (TCP dial +
  protocol hello only, no mounting), ListPorts (/proc/net/tcp{,6} parse),
  ProbeTcp/ProbeHttp (allowlist targets passed per-op by Keystone, agent
  enforces scheme+port sanity, 10s timeout), EnsureDockerNetwork
  (owner-labeled).
- **W6d System:** SystemdUnitStatus via `github.com/coreos/go-systemd/v22`
  D-Bus — **exception to §A4.5 pre-approved for this one dependency**
  (pure Go, no shell-out; unit name must match an allowlist config, prod
  default `wildstoned.service`, `docker.service`), TimeSyncStatus
  (`/run/systemd/timesync` + `adjtimex` via x/sys — x/sys is also
  pre-approved).
- Every op that returns bulk data gets a size cap; every op that touches
  the host gets a FORBIDDEN test proving its boundary.

### W7 — Audit trail

Goal: proposal §4.2's "two independent audit trails".

- Agent: log every dispatched op at Info as structured slog
  (`op_id`, arm name, key params — never file contents or env values,
  which may hold secrets) — journald picks this up under systemd.
- Keystone: `Dispatch` appends JSONL to `<DataDir>/audit.log`
  (`ts, node, op_id, arm, outcome, error_code`), fsync'd per entry is NOT
  required (document why: local homelab durability tradeoff), rotate at
  64 MiB keeping one generation.
- Tests: audit line written per op incl. failed ops; secrets absent from
  audit output when a `WriteManagedFile` carries secret-looking content.

### Sizing note for the planner

W1–W4 are small (each well under a day of subagent work); W5 and W6c are
the two with real risk — W5 because of re-exec testing, W6c because
EnsureMount is the most dangerous op in the product. Review those two
reports with extra care, and run W5's revert test locally before merging.

---

## Consequences

- Every new host-side ability is a proto change + agent handler + test —
  deliberate friction, exactly where the security model wants it.
- Generated code is committed, so builds need no protoc; regeneration is
  documented in `proto/README.md` and only needed when the .proto changes.
- The gateway is importable (`keystone/gateway`) so S2's reconciler drives
  agents through the same code path the S1 tests exercise.
- This ADR doubles as the E1 execution plan; when E1 completes, trim
  Part C to a short "implemented by" note and keep Parts A–B as the
  living contract.
