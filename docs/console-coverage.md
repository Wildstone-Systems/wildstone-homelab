# What the Console can actually do

A record of a gap that turned out to be systemic, the shape of it, and
what is left.

Run `python3 scripts/console-coverage.py` from the repo root for the
current numbers. This document explains what they mean and why the
gap happened; the script is the authority on where it stands today.

## The finding

Sixteen ADR-W030 work packages and seven ADR-W031 ones landed as stores,
policy, API routes and Console views, each with its own evidence run.
Every one of them was tested. The gap they share was invisible to all of
that testing, because it is not a defect in any of them.

**The views were built to show state and to create things. Editing and
removal were left for later on almost every one.**

It surfaced on the Butlers screen: a Butler could be created and then
not given to anybody, because the control that grants access was never
written. `console/src/api.ts` had `setButlerAccess`, `retireButler` and
`butlerScope` already implemented and no view called any of them. Then
the same shape appeared on Routines — where no routine could be authored
at all, so the screen read "No routines yet" and offered nothing to do
about it.

That is not two mistakes. It is one habit, and it is worth naming
because the next feature will have it too unless somebody looks.

## What the count means

The script compares every mutating route registered in
`keystone/api/api.go` against every path string `console/src/api.ts`
mentions. It answers one narrow question: **is there a client method at
all?** An endpoint it lists cannot be reached by any screen, however
that screen is written.

It deliberately does not judge whether a control is discoverable or
whether a screen is good. Those need a person.

A listed endpoint is not automatically a bug. Some routes are
machine-facing by design — a chat adapter posts inbound messages and
acks its outbound queue, an integration worker claims work and reports
results, a node completes a placement handshake. Those live in the
script's `MACHINE_FACING` set, are excluded from the count, and are
maintained BY HAND. That is on purpose: adding to that list should be a
decision somebody makes, not a regex quietly widening until the number
looks good.

## Closed

- **Butlers** — who may talk to it, where it answers, its directive, and
  retiring. Editing a directive needed an endpoint that did not exist
  (`PUT /api/v1/butlers/{name}/directive`); it applies at once and
  records an activity event, per ADR-W031 A8.
- **Butler scope** — `PUT /butlers/{name}/connection` and
  `PUT /butlers/{name}/tool`, which was the most load-bearing gap on
  this list: the difference between a nutrition Butler that *was not
  given* a door lock and one that merely has not been asked to use it.
  Reaching them needed a third endpoint that did not exist. A screen
  cannot draw a tool list without knowing which tools a surface has, and
  the two ways to find out from the client are both wrong — a list
  written in TypeScript is a second copy of the gateway's registry and
  drifts the first time a tool is added or dropped, and deriving whether
  each one is on means re-implementing the effective-authority order
  (structural, disabled, enabled, grant, default) in a screen, where
  getting it wrong shows a tool as on that a dispatch would refuse. So
  `GET /api/v1/butlers/{name}/tools` answers with the gateway's own
  manifest, resolved through that Butler's authority, and each
  unavailable tool carries the resolver's words rather than a sentence
  the Console made up.
- **The rest of the Butlers screen** — `POST /butlers/{name}/restore`
  and `POST /butlers/{name}/duplicate`, because "retiring is not
  deleting" was a sentence with no button behind it and a client-side
  duplicate is six calls that can half-fail on the narrowing, leaving a
  copy quietly wider than its original. `GET /butlers/{name}/impact`
  counts what retiring would break, so the confirmation states a
  consequence instead of asking whether somebody is sure. And
  `butler_sessions` gained a `butler` stamp (migration 0036) with a
  `?butler=` filter: a Sessions tab that filtered by SURFACE would list
  every household Butler's conversations under whichever name the
  drawer was showing, which is the one lie the named-Butler design
  exists to make impossible.
- **Household members** — recording a person. The authority screen could
  admit, disable and offboard people and had no way to record one, so
  every step after it was unreachable on a fresh deployment.
- **Routines** — authoring and editing the document. A routine IS its
  document, so both are a text editor with a name on it rather than a
  form with a field per concept; a form would have to enumerate the
  steps a routine may take and would be wrong the first time somebody
  wanted one it did not list.

## Open, by cluster

Ordered roughly by how badly the absence bites.

**Notices** — `preferences`, `kinds`, `acknowledge`, `answer`, `expire`.
All five. A notice can be raised and a person cannot set quiet hours or
answer one from the Console at all, which leaves ADR-W030 P15's whole
policy configurable only over HTTP.

**Identity** — principal `enable`/`disable`/`tokens`, member `links`,
`offboarding`. Token minting in particular: a deployment whose operator
has mislaid their token has no route back through the Console, which is
how one session ended up stopping the service to run `claim-code`.

**Recovery** — `restore/quarantine`, `restore/reconcile`, and
`coordinator/promote`. The coordinator routes are new; promotion is the
only exit from "another coordinator holds this database", and it is
reachable only by curl.

**Integrations** — `adapter`, `capabilities`, `secrets`,
`placement/remove`.

**Household** — `publishers/{capability}`, `review/token`,
`automations/replay`.

**Butler conversations** — `conversations`, `conversations/{id}/handoff`,
`grants`, `grants/revoke`, `routes/{name}/readiness`.

## The thing to check next time

A work package is not finished when its store, its policy, its route and
its evidence run are done. Ask what a person does with the thing
afterwards — change it, hand it to somebody, take it back, stop it — and
whether any of that is reachable without curl.
