# W050 slice 16: retire the Docker backend

> Draft issue body. Plan: §3 ("Gone" rows).

## Goal

Remove Docker code and CI once every package and every supported install runs on k3s.

## User-visible outcome

Release notes: Docker installs must move (slice 12 tooling) before upgrading past version X.

## Scope

- Delete `wardstone/internal/agent/docker.go`, Docker translator, `EnsureDockerNetwork`, `ingressaddr.go` Docker path, `host.docker.internal` rendering.
- Proto: mark Docker-only arms deprecated, then reserved (keep numbers).
- Installer: remove Docker install and `--skip-docker`.
- CI: drop Docker legs of e2e and exit drill.
- Docs: install, recovery, migration.

## Non-goals

Talos.

## Dependencies

Slices 10 and 13 complete; one release with a deprecation warning.

## Acceptance criteria

- [ ] No `docker` reference in Go code outside migration tooling.
- [ ] e2e and exit drill (k3s) green.
- [ ] Upgrade from the last Docker-capable release refused with a clear message if packages are still on Docker.

## Risks

Households that never moved; keep the migration tool in one more release.

## Size

**M.** Mostly deletion.

## ADR amendment

ADR-W050 status note.
