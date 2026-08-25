# ADR-W011: Infrastructure packages — ingress, monitoring, notifications, dashboard, backups

- **Status:** accepted
- **Relates to:** ADR-W005/W010 (format + toolchain), implementation
  scope E6
- **Audience:** decision record AND implementation spec for E6.

---

# Part A — Decisions (binding)

### A1. Transcription first, engine second

E6 splits cleanly: the ◆ transcription packages (ingress, monitoring,
notifications, dashboard — proven configs from the reference deployment,
digest-pinned via `wild pkg refresh-digests`'s resolver) land first and
must lint clean under the E5 toolchain; the ▲ backup engine (BackupPlan
kind + restic controller + restore plans + drill duty) is its own work
package with its own decisions (A4). Identity already shipped with S3.

### A2. The transcription packages

- **ingress** (`caddy-cloudflared` profile): caddy-public and
  caddy-private Containers (both mount the managed prefix read-only —
  the reconciler's Route fragments are their config; the agent's
  managed-prefix guard is what makes that bind safe), a cloudflared
  Container whose tunnel token is `secret/cloudflared-tunnel-token`
  (reference only, E8 materializes), and a proxied wildcard DnsRecord
  pointing the public domain at the tunnel — the first package to carry
  a DnsRecord. Tailscale join is host-level and belongs to the E9
  installer, not a container here.
- **monitoring**: Uptime Kuma, `admin_tailscale` route behind
  forward-auth (per the reference registry). Wardstone metrics feed it
  later via its own probes — no agent changes here.
- **notifications**: ntfy, `family_private` route with ntfy's own ACLs
  (`auth: none` is honest and policy-clean off the public class).
- **dashboard**: Homepage, `family_public` behind forward-auth (the
  reference's family-homepage posture).

### A3. ContainerSpec grows `command`

ntfy's image needs an argument (`serve`); the reference compose uses
`command:`. ContainerSpec gains `command: []string` (container args,
appended after the image entrypoint), threaded through the proto
(`EnsureContainer.command`, protocol minor 6), the agent's create call,
the plan differ, and the spec hash — so a command change is spec drift
like any other. A pre-minor-6 agent ignores the field (proto3 unknown
field): acceptable pre-alpha, visible via Hello's protocol_minor.

### A4. Backups (the MVP headline — own work package)

Decisions to bind when I2 starts, recorded now as direction: a
**BackupPlan** resource kind (source volumes/paths, repository ref,
schedule, retention); restic runs as an owner-labeled CONTAINER with
declared mounts (never a host binary — the lifecycle-hooks philosophy);
the repository password is a `secret/` reference; schedules live in
Keystone (the reconcile service gains a timer duty, not cron on the
host); every run and every restore drill is a plan/event with
provenance; the monthly restore-drill is a scheduled plan the owner
approves once as a standing duty. Restore-alongside first, in-place
restore only via explicit High-risk plans.

# Part B — Work packages

- **I1** — A3 schema extension; the four A2 packages, digest-pinned and
  lint-clean; the pkgtool contract test walks ALL of packages/ so a new
  package cannot ship unlinted.
- **I2** — the A4 backup engine (BackupPlan kind, controller, restore
  plans, drill duty).
