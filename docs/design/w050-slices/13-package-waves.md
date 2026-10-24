# W050 slice 13: package migration waves

> Tracked in #169 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

Every package with a workload translates to k3s, passes admission and the e2e install-verify-remove check, ordered easy → hard.

## User-visible outcome

More packages offer "Install on the new engine" / "Move to the new engine".

## Waves

- **Wave A (easy, 21):** the §5.5 list. Mostly config: availability, probes (#39), limits (#41).
- **Wave B (medium, 31):** database apps (§5.3), media and shared folders (§5.4, needs D4: NFS CSI / Longhorn RWX), LAN-port packages (`mqtt-mosquitto`, `dns-*`, `code-gitea`, `downloads-qbittorrent`, `sync-syncthing`), host-binary packages (§5.6, needs O11 images). Add `sqlite` hooks where missing (#40).
- **Wave C (rest):** anything left from §5.1/§5.2 not done in slices 9 and 11.

## Scope per package

`availability`, probes, `resources`, translation golden test, admission test, e2e install → ready → front door → remove (the #28 harness).

## Non-goals

`replicated`/`offload` behaviour (slice 15).

## Dependencies

Slices 11 and 12; wave B also needs D4 and O11.

## Acceptance criteria

- [ ] CI lint: every package that claims `k3s` translates and passes admission in k3d.
- [ ] Nightly QEMU e2e covers each package at least weekly (rotating).
- [ ] Owner checklist for packages that need real hardware or a NAS.

## Risks

Long tail of app quirks (UIDs, `fsGroup`, read-only rootfs).

## Size

**3 × L.** Wave A is small per package but many packages; B carries the storage work.

## ADR amendment

No.
