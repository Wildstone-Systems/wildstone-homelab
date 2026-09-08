# Vision: bringing the data center back home

This document is the why behind Wildstone — the conviction the epics
serve. The scope doc says what we build and in what order; this says
what it is for. When a future decision is ambiguous, it should resolve
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
  there is no operation that reads one back. Backups go to storage you
  name. Nothing phones home; there is no telemetry to a vendor, because
  there is no vendor in the loop.
- **The Butler proposes; the household disposes.** Its only write
  capability produces a plan a human must approve. It holds no approve
  verb, no exec, no side channel — a ceiling enforced by structure and
  pinned by tests, not by policy documents. Even a fully compromised or
  fully fooled model can leave nothing behind but a proposal waiting
  for your "no".
- **Even the AI's own vendor is held at arm's length.** Every tool
  result the model sees is redacted and bounded first. Local processing
  is preferred and local-only data never follows a cloud fallback. Per
  ADR-W030, an admitted adult profile may explicitly opt into a cloud
  model only for data whose own policy permits it; background jobs make
  the same decision independently. Fully local inference remains a
  first-class path.

## The arc: two butlers

**The operator's butler (this repo, the MVP).** What exists today
manages the infrastructure layer: it installs capabilities as signed
packages, converges nodes through a closed operation surface, diagnoses
a broken capability down to its root dependency, writes the morning
report, runs the backups, and — proven weekly by the exit drill — lets
the household lose the host and keep its data. This is the foundation:
sovereign and survivable.

**The family's butler (the horizon).** The same trust machinery,
pointed at the services themselves rather than at their containers.
The coordinator that talks to PantryOS about the kitchen, to Home
Assistant about the house, to Immich about the photos — for every
member of the household, on the household's terms, with the same
propose-and-approve power relationship. The Alexa replacement where the
answer to "who does this assistant work for?" is checkable in the
source: it works for the people who live here.

## Principles (the short list)

1. **Data stays home.** If a design forces household data off-site, the
   design is wrong.
2. **Power stays with the household.** Automation may propose anything
   and decide nothing irreversible. The approve verb belongs to humans.
3. **Loyalty is structural.** Trust properties are enforced by
   construction and pinned by tests; a promise that can't be verified
   in the code doesn't count.
4. **Survivability is part of sovereignty.** Data you can lose to one
   dead disk was never fully yours. The exit drill is the standing
   proof.
5. **Coordinate, don't colonize.** Wildstone manages what it created
   and never deletes what it didn't. It composes the self-hosted
   ecosystem; it does not replace it.
6. **No dark patterns, ever.** No ads, no attention engineering, no
   growth hooks, no pressure. The product is quiet infrastructure that
   serves the household and otherwise stays out of the way.
