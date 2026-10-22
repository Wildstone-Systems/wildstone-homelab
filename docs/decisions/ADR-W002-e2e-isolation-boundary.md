# ADR-W002: E2E harness network isolation boundary

- **Status:** accepted (platform owner decision, 2026-07-30)
- **Relates to:** E0.2 (e2e harness), implementation scope §8

## Context

The e2e harness boots disposable Ubuntu 24.04 VMs, installs Wildstone
components onto them, and asserts behavior. The cheapest source of VMs is
the platform owner's Proxmox host — but it is reachable only over
Tailscale (`proxmox.tailnet`). GitHub-hosted CI runners and cloud agent
sessions are not tailnet members, so they cannot reach it.

Options considered:

1. **Hard isolation boundary** — CI and cloud agents never touch Proxmox.
   They run e2e against locally-booted QEMU cloud-image VMs on the runner
   itself. Proxmox-backed runs happen only from machines already inside
   the tailnet.
2. **Ephemeral tailnet join** — CI runs join the tailnet with short-lived,
   ACL-restricted auth keys. Most capable, but makes third-party CI
   machines transient members of the home network and adds ACL surface to
   maintain.
3. **Relay/broker** — an always-on tailnet service proxies "make me a VM"
   requests. Extra moving parts for a solo operator.

## Decision

**Option 1.** The tailnet boundary is absolute for hosted CI and cloud
agents: nothing outside the tailnet gets credentials for, or network reach
to, Proxmox.

- The harness exposes a provider interface with two implementations:
  - `local` — QEMU + cloud-init NoCloud seed, KVM when `/dev/kvm` exists
    (GitHub-hosted Linux runners have it), TCG fallback otherwise. This is
    the CI path and works anywhere.
  - `proxmox` — Proxmox REST API (clone template → cloud-init → start).
    Only usable from inside the tailnet.
- If Proxmox-backed CI is ever wanted, the sanctioned route is a
  **self-hosted GitHub runner inside the tailnet** (owner-operated), not
  tailnet keys for hosted runners. The provider interface makes that a
  runner-label change, not a harness change.

## Consequences

- CI e2e fidelity is bounded by what a nested/local VM can exercise;
  multi-node and real-hardware scenarios stay owner-triggered.
- No Tailscale ACL or key-rotation machinery is needed for CI.
- Proxmox provider code cannot be exercised by hosted CI; its first real
  run must happen from inside the tailnet and it is flagged as untested
  until then.
