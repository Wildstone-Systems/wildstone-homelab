# Vision: bringing the data center back home

This document is the why behind Wildstone — the conviction the epics
serve. The historical scope doc records the original epics. Current ADRs and issues
guide implementation; this document explains what the product is for. When a future decision is ambiguous, it should resolve
in the direction of this document.

## The problem: the home stopped belonging to its household

The modern home is full of other people's computers. The assistant on
the counter listens for a corporation. The photo library, the movie
night, the shopping list, the thermostat — each one is a client of a
server someone else owns, monetized by attention and data. The
household's daily life produces a stream of information that leaves the
house, is stored on infrastructure the family will never see, and comes
back as recommendations, ads, and pressure. That is surveillance
capitalism at the household scale: convenient, polished, and
fundamentally not yours.

The self-hosting movement already built the replacement parts — Immich
for the photo library, Jellyfin for the media shelf, Paperless for the
filing cabinet, Home Assistant for the house itself, PantryOS for the
kitchen. What's missing is the thing that makes them livable for a
family rather than a hobby for an operator: something that installs
them, wires them together behind one identity and one front door, keeps
them healthy, keeps them backed up, and survives a dead disk.

## The answer: a butler, not a landlord

Wildstone is that missing piece: **the home lab as household
infrastructure, with a Butler as its coordinator** — the first stepping
stone in bringing the data center back home.

The Butler is the deliberate counter-image of Alexa, Siri, and the
Google home. Those are a corporation's agent placed inside your house,
acting on the corporation's incentives, asking for your trust. The
Wildstone Butler is **the household's agent**, and its loyalty is not a
promise — it is construction:

- **Data stays inward.** The desired state is a git repo on your host.
  Secrets travel a write-only path and are encrypted on your node —
  there is no individual secret read-back operation. Backups go to storage you
  name. Nothing phones home; there is no telemetry to a vendor, because
  there is no vendor in the loop.
- **Authority belongs to the household.** Infrastructure proposals require
  human approval. Agents may perform only granted tools under current policy,
  including explicit confirmation where required; they cannot approve their
  own infrastructure Plans. Owner/Member classes, resource authority and
  disclosure rules are separate boundaries (ADR-W033).
- **Model processing is explicit.** Local-only data must not follow a cloud
  fallback. Cloud providers and externally hosted runtimes are optional,
  permission-bound choices, not prerequisites for deterministic management.
  Redaction is defense in depth, not a guarantee that all household content is
  removed before an authorized model call.

## One platform, household and management surfaces

Homepage is the household experience; Console is Owner administration. Agents
(also called Butlers) have explicit classes, tools and Connections. The older
two-butler story is historical context, not the current authorization model.
Read [agent system design](agent-system-design.md) and ADR-W033/W039/W049 for
the governing identity and runtime boundaries; accepted design is not proof
that every slice or external-client combination has been verified live.

## Simple adoption and predictable recovery

Start with one computer and a guided install. Failure of a large application
should not also remove every way to diagnose it. Keep management small,
resource ownership explicit, and application data recoverable with keys and
instructions that survive the lost host.

[ADR-W050](decisions/ADR-W050-proxmox-first-deployment-and-recovery.md)
proposes independent management and application recovery groups on Proxmox.
The placement remains under review; neither a cluster nor Kubernetes is a
prerequisite for the product goal. The [management contract](design/application-management-contract-v0.1.md)
proposes consistent semantics across hosting environments, using existing
packages and traits rather than a second integration model.

CI recovery drills exercise defined fixtures. Household survivability also
requires tested application restores, external storage coverage and correct
restart dependencies on the actual installation.

## Principles (the short list)

1. **The household controls its data.** Local operation is a first-class path.
   Optional encrypted off-site backups and permitted cloud processing require
   explicit policy and choice.
2. **Power stays with the household.** Automation may propose anything
   and decide nothing irreversible. The approve verb belongs to humans.
3. **Loyalty is structural.** Trust properties are enforced by
   construction and pinned by tests; a promise that can't be verified
   in the code doesn't count.
4. **Survivability is part of sovereignty.** Data you can lose to one
   dead disk was never fully yours. Recovery drills provide bounded evidence;
   each deployment must also verify its data, keys and application restore procedure.
5. **Coordinate, don't colonize.** Wildstone manages what it created
   and never deletes what it didn't. It composes the self-hosted
   ecosystem; it does not replace it.
6. **No dark patterns, ever.** No ads, no attention engineering, no
   growth hooks, no pressure. The product is quiet infrastructure that
   serves the household and otherwise stays out of the way.
