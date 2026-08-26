# ADR-W014: Installer bootstrap — releases, install.sh, pairing

- **Status:** accepted
- **Relates to:** ADR-W003 (join tokens, CA pinning), ADR-W007 (API),
  implementation scope E9
- **Audience:** decision record AND implementation spec for E9's
  bootstrap half. The Console onboarding flow (scope §7.2 steps 1–7,
  probes, the Restore branch) is deliberately NOT here: it is
  Console-shaped work and lands with E10, sharing the pairing endpoint
  this ADR adds.

---

# Part A — Decisions (binding)

### A1. Binaries come from GitHub releases, checksum-verified

A tag `v*` builds `keystone`, `wardstoned`, and `wild` for
linux/amd64 and linux/arm64 — CGO_ENABLED=0 throughout (the pure-Go
SQLite choice in ADR-W007 exists exactly so this cross-compile is
boring) — and publishes one tar.gz per binary/arch plus a single
`checksums.txt`. The installer downloads from
`github.com/chickenrat17/wildstone-homelab/releases` and verifies
sha256 before installing anything; a `get.wildstone.dev` alias stays
the post-launch nicety the scope doc says it is.

### A2. One installer, two roles

`scripts/install.sh` (bash, no dependencies beyond curl/tar/sha256sum,
root, systemd) serves both machines:

- `--role keystone`: installs the `keystone` binary and `wild`, a
  systemd unit, and DataDir `/var/lib/wildstone/keystone`; first start
  prints the one-time admin API token (stdout of the service — the
  installer tells the operator where to find it) and the CA
  fingerprint.
- `--role node --keystone <host:port> --join-token <tok>`: installs
  `wardstoned` and its unit; `--node-id` defaults to the hostname;
  `--ca-fingerprint` is accepted and STRONGLY recommended (the pairing
  endpoint hands it out precisely so joins are pinned, ADR-W003 W3).
  Docker is installed via get.docker.com only if absent, skippable
  with `--skip-docker`.

Re-running the script upgrades the binary in place and restarts the
unit — idempotence is the upgrade path at MVP. CI keeps the script at
least parse-clean (`bash -n`).

### A3. Pairing is an API concern

`POST /api/v1/join-tokens` (admin `manage` verb) mints a single-use
join token via the gateway (default TTL 15 minutes, capped at 24h) and
returns it TOGETHER with the CA fingerprint and a ready-to-run install
command — the "pairing code" of the scope doc is this one response.
The join token is 64 hex chars, exactly the shape the redaction layer
exists to eat, so the response joins token minting as the second (and
last) deliberate redaction carve-out; like the first, it is the only
moment the value ever appears in a response. `wild node mint-join`
wraps the endpoint.

# Part B — Work packages

- **N1** — A1 release workflow; A2 install.sh + systemd units; A3
  endpoint + `wild node mint-join`; CI syntax gate.
- **N2** (deferred to E10) — Console onboarding flow + probes + the
  Restore branch, consuming A3.
