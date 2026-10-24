# W050 slice 15: `replicated` and `offload`

> Tracked in #171 (labels `W050`, `POST Alpha`, `agent:blocked`). This file is the source draft; the issue is the live copy.

## Goal

Stateless packages can run several copies behind one Service; Jellyfin (and possibly Immich ML) can send heavy work to other machines.

## User-visible outcome

- Package setting "Copies: 1 / 2 / 3" when supported.
- Jellyfin: "Use other machines for video conversion" when measured useful.

## Scope

- `replicated`: `replicas`, `RollingUpdate` (`maxUnavailable: 0`, `maxSurge: 1`), PDB, soft anti-affinity; packages from §5.5.
- `offload`: worker Deployment; evaluate rffmpeg for Jellyfin and Immich ML as a separate Deployment; decide with measurements.
- Caddy with more than one copy only if shared certificate storage is proven (O5).

## Non-goals

Autoscaling.

## Dependencies

Slice 14.

## Acceptance criteria

- [ ] Rolling update of a 2-copy app with zero failed requests (load probe).
- [ ] Node drain with PDB keeps one copy serving.
- [ ] Offload measurement report before enabling any offload default.

## Risks

Apps that claim statelessness but keep local state.

## Size

**L.** Two shapes plus measurement work.

## ADR amendment

No.
