# testing/

e2e harness (disposable Ubuntu 24.04 VMs via Proxmox API, cloud-image
fallback for CI), the scrubbed fixture library, and the restore-drill runner.

## e2e/

The harness lives in `e2e/` (its own Go module). Network boundary per
ADR-W002: hosted CI uses the local QEMU provider; the Proxmox provider is
only usable from inside the tailnet. See `e2e/README.md` for how to run.

## fixtures/

Golden configs transcribed from the reference deployment
(the maintainer's private, hand-managed homelab repo): Authelia, Caddy, LLDAP, Immich OIDC wiring,
Jellyfin LDAP plugin, Gluetun VPN egress.

**Scrub rule (non-negotiable):** every fixture uses placeholder values —
`example.internal` hostnames, `REPLACED-CLIENT-ID` identifiers, no real
domains, IPs, or credentials of any kind. CI runs gitleaks on every push;
a fixture that trips it is a bug even if the value is "only" semi-secret.

Failure fixtures (for Butler diagnostics, E12): broken OIDC secret, dead
database, full disk, detached NFS, and friends — built alongside the happy
paths.
