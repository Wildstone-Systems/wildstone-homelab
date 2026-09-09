# ADR-W027: Console — the redesign, and what ADR-W016 A1 still holds

- **Status:** accepted
- **Supersedes:** ADR-W016 A1 on the visual axis only; A2's screen list
  is extended, not replaced
- **Relates to:** ADR-W016 (console IA and MVP), ADR-W026 (chat
  bindings — the Chat screen this ADR places in the nav), F75 (a plan
  waiting for a human is not a fault)
- **Audience:** decision record AND implementation spec for the
  redesign.

## Conditional amendment from ADR-W030

This amendment becomes binding when ADR-W030 is accepted. A session remains a
principal-bound conversation, while multiple conversations participate in one
permission-filtered logical history across explicit Household/Operator
handoffs. The current surface is derived from the destination endpoint and
stored task/conversation state, never model-selected. Retrieval rechecks
person, profile, audience, admission, and current denials before constructing
each model context.

Historical tool traffic still does not replay as fresh truth. Durable work
resumes from its validated step ledger and refreshes stale observations.
Console adds active-work states for waiting, stalled/resumable, partially
complete, completed with an undelivered notice, and terminal failure. Plan
cards and approval remain authenticated Console actions; chat deep links carry
only a Plan identifier and destination.

The active-work detail renders W030's structured trace as a task timeline:
steps, source freshness, tool attempts, waits/retries, approvals/denials,
delivery, and completion evidence. Permission checks occur server-side before
trace fields are returned. People may label an outcome for improvement and
create a sanitized regression candidate; the screen never presents hidden
chain-of-thought or raw unredacted provider/tool payloads.

Console adds a narrow household-routine review route for an admitted adult's
`household-member` session. It renders only the routine preview, behavior diff,
effects/audience, version, and approve/reject controls for a Plan already
classified server-side as household-routine. It does not reveal the general
Plans screen, infrastructure state, credentials, or administrative navigation.

---

# Part A — Decisions (binding)

### A1. "Deliberately boring" is superseded on the visual axis, and only there

ADR-W016 A1 recorded "no UI framework, no component library, no state
manager" and the scope doc's "deliberately boring visual design at
MVP". That decision bought two things: an auditable console, and no
time spent on branding before the product worked. The first is still
worth having. The second has been paid for — the console works, and
what it costs now is legibility.

So: the visual half is superseded, and the structural half is not.

Still binding, unchanged:

- No UI framework, no component library, no state manager. React's own
  state, hand-written CSS, one typed client.
- `src/api.ts` stays the only fetch path (A3), the token stays in
  sessionStorage, and a 401 anywhere still returns to the gate.
- No console-only endpoints. Anything the redesign needs from the API
  is public API that `wild` and the Butler can use too. Where the API
  cannot answer yet, the console derives an honest approximation and
  says so, or shows nothing.
- Dense where density helps. This is an operator's console, not a
  marketing page.

Superseded:

- "No branding pass." There is now a token set, a type scale, an icon
  set and a shared component vocabulary, all recorded below.

Two dependencies are added, and they are the whole list:
`@phosphor-icons/react` (imported per icon, so the bundle carries the
dozen glyphs the nav uses and not a web font) and
`@fontsource-variable/inter` (the latin subsets, bundled into `dist`).

### A2. The font ships with the console; nothing is fetched at runtime

The console is the screen you open when the house is broken, which is
exactly when it may have no route to a font CDN. Inter's latin and
latin-ext subsets are bundled into `dist` and served from the same
origin as everything else. There is no third-party request from the
console at runtime, and there is not going to be one.

### A3. Dark is the ground; light is a token swap; the OS decides

The design was drawn on a dark ground and that is the default. The
light theme is the same sheet with a different `:root` block and no
other change — no rule below the token block may name a colour
literal. `prefers-color-scheme` picks between them, and an explicit
`data-theme` on `<html>` overrides it in both directions, so a toggle
is one attribute and no new CSS if one is ever wanted.

### A4. Status colour is semantic, and never the accent

Three status colours (`--st-ok` / `--st-warn` / `--st-bad`) and one
accent, deliberately disjoint: an operator has to be able to tell
"this is selected" from "this is broken" without reading. The status
colours are used as a fill in exactly one place — the status pill.
Everywhere else they are ink or a 3px rule, so a screen with several
amber things on it does not read as a screen on fire.

Three states, not two (F75). A resource held back because a plan is
waiting for a human is amber, never the red of a fault. A restart once
painted most of Home red while every container was up and healthy, and
an operator could not tell that from a homelab that had actually
broken.

### A5. One place decides what a wire value means

`kindOf()` in `src/ui/status.tsx` maps every state string the API
serves onto one of four kinds. Screens ask it; they do not grow their
own opinion of what `skipped` looks like. A state that has not been
seen before lands in `mute` rather than being quietly coloured wrong.

### A6. Contrast is a property of the token set, checked once

Muted text is a mix of the ink with the ground: never below 58% on
dark, never below 70% on light. The asymmetry is real — mixing pale
ink toward a pale ground loses more contrast than mixing bright ink
toward a dark one, and 58% on light measures 3.5:1.

`console/scripts/contrast-check.mjs` reads the literal token values out
of `styles.css` and asserts every text pairing at 4.5:1 and every
large-numeral and mark pairing at 3:1, in both themes. It runs in CI.
This is checked once, about the tokens, rather than argued again in
every PR that adds a label.

### A7. Every action resolves in place

`useAction()` is the write-half counterpart to `useLoad()`, and it
keeps the *result* in the state. The card that fired an action says
what happened — approved by whom, converged when, or the error — and
keeps the surrounding context. No mutation in the console reloads the
page and leaves it looking untouched.

### A8. One poll drives the shell

`ConsoleStateProvider` runs a single 4-second poll of
`/observed`, `/plans`, `/nodes` and `/events?after=`, and everything
that needs ambient state reads from it: the rail's pips, the header's
freshness line, Home's metric strip, Activity. One timer and one
context, not five `useLoad`s racing each other.

A dropped poll keeps the last good snapshot on screen and marks the
header `stale`. Blanking the console because one request failed is
worse than showing state that is four seconds old and labelled.

### A9. Sparklines show only what the console has seen

There is no observation-history endpoint. Health history is
accumulated client-side from the event stream the console already
polls, which means a fresh load has almost none. A sparkline draws
nothing below four samples and captions itself "since this page
loaded" rather than implying history we do not have.

When `GET /observed/history` exists it replaces the accumulator and
nothing else changes.

### A10. Navigation is an icon rail, and Chat sits with the Butler

Twelve screens, in this order:

    Home · Services · Butler · Chat · Household · Connect chat ·
    Plans · Activity · Backups · Nodes
    ─────
    Secrets · Identity

Events is renamed Activity. Chat (ADR-W026 A4) is placed immediately
after Butler because it answers "who may talk to the Butler, and as
whom" — it is read as part of the Butler's surface, not as another
piece of infrastructure. Household and Connect chat (ADR-W026 A2/A3)
follow it for the same reason: the three of them are one subject.
The rule below the divider stays what it was: the screens you visit
when something specific needs doing.

Twelve is a lot for one rail, and it is worth revisiting. Connect chat
is a setup flow run once per adapter, which is the argument for moving
it below the divider; it stays above for now because it is the screen
Chat sends you to when no adapter is installed.

Icons carry the nav, so every item keeps its label as both a `title`
and an `aria-label`, and a pip appears only when that screen has
something waiting. A healthy console shows no pips at all.

On a phone the rail becomes a bottom bar. The case that has to survive
there is the glance: is anything wrong, approve the plan, re-run the
thing that failed.

### A11. A Butler session becomes a conversation

The one API change the redesign needed, and it is public API rather
than a console convenience: `POST /api/v1/butler/sessions/{id}/ask`
appends a turn to an existing session, and `GET
/api/v1/butler/sessions/{id}` returns the whole conversation. The CLI
and the chat adapters get follow-ups from the same endpoint.

Four decisions inside it:

**The question is stored.** It never was. A session recorded what the
Butler said and not what it was asked, which was enough to audit the
answer and not enough to read the exchange or to ask a second question.
Migration 0014 backfills existing sessions with their answer alone —
a conversation that opens with an answer, which is true, rather than an
invented question.

**One endpoint, not one per surface.** ADR-W026 A1 gave each surface its
own ask endpoint so the Console could not send a household question to
the operator's Butler. That separation is preserved and in fact
strengthened here: the surface comes from the STORED session, so it is
decided by the endpoint the conversation started on and cannot be named
by the follow-up request at all.

**Only the person a session was recorded for may continue it.** An
operator can still read anyone's session — that is what the audit is
for — but reading a conversation and speaking into it are different
acts, and the second one is not an audit.

**History carries the text, not the tool traffic.** A tool result is a
reading taken at a moment. Replaying "jellyfin's health endpoint returns
500" into a turn an hour later hands the model a stale fact with the
authority of a fresh one. The earlier answer already summarises what
those reads found, in the model's own words, and if the follow-up needs
the current reading the model can take it again. `MaxIterations` bounds
each turn rather than the conversation: the cap exists to stop a runaway
loop, and a second question is not one.

Per-turn storage matters for the audit too. "Which reads produced THIS
answer, and which plans did THIS answer propose" stays answerable once
a session has answered three times; one merged trace per session
cannot answer it.

### A12. A proposal is the same card wherever you meet it

The Butler thread renders a proposal with the Plans screen's own
component and the same handlers. One component, two placements — which
is what keeps "the Butler can propose but never act" visible rather
than merely stated: the approve button is yours, in both places.

---

# Part B — Notes

## What the API still cannot answer

Recorded so the approximations above are not mistaken for the
intended end state. None of these blocks the redesign; each replaces a
client-side derivation when it lands.

| Want | Today | Console does |
| --- | --- | --- |
| Observation history | `/observed` is current state only | accumulates from the event stream, draws nothing below four samples (A9) |
| Multi-turn Butler | closed by A11 — `POST /butler/sessions/{id}/ask` | — |
| Statefile head | only via `PlanRecord.basedOnHead` | reads the newest plan's `basedOnHead`; a `GET /status` would serve it directly |
| Reconcile age | only via `reconcile.tick` events | reads the newest tick out of the polled stream |
| Volume fill | no volume usage on the wire | hides the bar rather than inventing one |
| Backup run progress | `BackupRun` has status and timestamps | indeterminate bar while running |

## Verification

The loop is unchanged — `gofmt -l`, `go vet ./...`, `go test -race
./...`, `npm run build` — with two console checks added to CI, neither
of which needs a browser or a control plane:

- `npm run render-check` renders every shared component and every
  screen, and asserts the contracts that are easy to break silently:
  that `kindOf` maps the vocabulary, that a sparkline stays quiet
  without data, that an indeterminate bar claims no value, that every
  rail item is named.
- `npm run contrast-check` holds A6.
