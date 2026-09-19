# ADR-W036: A theme belongs to a person, not to the household

- **Status:** accepted, and built. Everything in Part A is implemented:
  `keystone/theme`, migration 0058, `handlers_appearance.go`, the
  Console's rail picker and the household dashboard's.
- **Number:** W036.
- **Relates to:** ADR-W027 (the Console redesign, and the token block
  this depends on existing), ADR-W016 A1/A3 (one fetch path, no
  console-only endpoints), ADR-W032 and ADR-W033 A1 (who opens the
  Console; Owner and Member), #197/#198 (the household portal's door),
  migration 0043 (a person is a `person_id`, not a handle).
- **Audience:** decision record AND implementation spec.

---

## The question

Wildstone has two household-facing surfaces — the Console and the
household portal — and one design system, Nocturne, drawn on both. The
household can already choose a colour: `accentHue` in the dashboard
capability's `settings.yaml`, which is desired state.

Can people choose a whole theme, and if so, whose choice is it?

## A1. A theme is per person; the household's hue stays per household

The hue in desired state is right where it is. It is small, declarative,
and an operator should be able to read the house's colour out of the
statefile. It is the colour on the tablet everybody walks past.

A theme is a different kind of thing. It is the ground somebody reads
on, and the people in one household do not agree about it: one reads at
night, one has low vision and wants the highest-contrast light theme
available, one just likes Gruvbox. As a household setting, the last
person to change it decides for everybody — the behaviour of a
thermostat, not of a preference.

So: the hue stays declarative, and a person's theme lives in the runtime
database, beside the other things a person chose for themselves
(`member_notice_preferences` is the same shape for the same reason).
Migration 0058 keys it on `person_id`, per 0043, so a rename moves the
label and detaches nothing.

**Consequence.** There is deliberately no administrative override. An
Owner decides which themes *exist* for this household; each person
decides which one they read on. An Owner who could set somebody else's
theme could make a screen their housemate cannot read, and no household
setting is worth that.

## A2. A theme is an Omarchy `colors.toml`, and nothing else

[Omarchy](https://omarchy.org) themes are directories, and most of what
is in one configures programs Wildstone does not run — a compositor, a
terminal, a status bar. The portable part is `colors.toml`: a mode and
about two dozen named colours. That is what this reads, and the rest is
ignored, which is also what Omarchy's own installer does to the
executable parts of a third-party theme.

**Consequence.** "Use a theme from omarchythemes.com" is one paste of
one small text file. Nothing is cloned, fetched or executed by the
control plane, which is the only version of this feature that belongs on
a machine that runs somebody's house.

Twenty-two palettes are vendored into the binary
(`keystone/theme/themes/`, see its `NOTICE.md`), because a household
with no route off its own network is a posture this project supports and
a theme that needs the internet is a theme that is not available.

## A3. A palette gives colours; a design system gives relationships

A `colors.toml` has grounds, an ink and named colours. It does not have
a card ground, a drawer ground, a muted ink, a hairline, a status tint
or a shadow ring. Those are relationships, and they belong to Nocturne.

So `keystone/theme` borrows the palette's colours and derives everything
between them. An imported theme comes out looking like Wildstone wearing
that palette, rather than like a terminal colour scheme pasted onto a
web page.

## A4. Legibility is enforced by repair, not by refusal

ADR-W027 gave the Console a contrast contract and
`console/scripts/contrast-check.mjs` to hold it, and that check reads
literal values out of `styles.css`. A theme chosen at run time never
appears in `styles.css`, so nothing in the browser can catch a palette
whose muted text fails 4.5:1 on its own ground.

The guarantee is therefore made in the derivation: every pair that
carries text is held to WCAG AA against every ground it is drawn on, and
body copy to AAA. Contrast is measured on the *quantised* colour, so the
number the test sees is the number the browser draws.

A palette that fails is **nudged until it passes**, along the line from
its own colour towards white or black — so its hue survives — rather
than refused. Taking somebody's theme away for failing an audit they
cannot see is the worse answer.

## A5. Status stays semantic

ADR-W027's token block says it already: "an operator must be able to
tell 'this is selected' from 'this is broken'." Terminal palettes are
under no such obligation — Omarchy's `matte-black` ships
`green = "#FFC107"`, which is amber.

So a palette's colour is taken for a status token only when it is in
that colour's family; otherwise the design's own semantic shade is used
and adapted to the theme's ground. A household can pick a theme it likes
without quietly losing the ability to see that something is wrong.

## A6. Choosing nothing serves no block at all

The default is the absence of a choice, not a theme called "Wildstone".
With no theme set, no token block is served and both surfaces render
exactly as they do today — the household's hue, out of desired state,
through the sheets that already ship.

**Consequence.** Wildstone's own appearance is never reimplemented as a
palette, so it cannot drift from the sheets, and the whole feature is
provably inert for a household that never opens the picker.

## A7. The portal is served themed; the Console wears it in the browser

These differ because the surfaces differ, not by accident.

A portal page is rendered by Keystone for a member the door has already
resolved, so the token block is injected into the document at a marker
(`<!--wildstone:theme-->`, placed after the stylesheet it must outrank).
Nothing is fetched and there is no flash of the default palette on a
wall tablet somebody is standing in front of.

The Console's bundle is served *before* anyone authenticates — the
static files go to a browser that has not yet sent its bearer token — so
there is no request at which a per-person sheet could be injected. It
applies the tokens as inline custom properties on the root element
instead, which outranks every rule in `styles.css` with no selector to
match, and caches the last block in `localStorage` so the first paint is
already right.

The derivation runs in exactly one place either way. There is no copy of
the contrast rules in TypeScript, because two implementations of a rule
is one of them being wrong.

## A8. The embedded widgets are not themed

`files-widget` and `assistant-widget` are iframed into somebody else's
dashboard card and painted to match that card, from a `?theme=` the
dashboard sets. A person's theme there would fight the panel it sits on.

## What this does not do

- **No per-surface themes.** One choice, every Wildstone screen. Two
  would be two things to keep in sync and two places to be confused by.
- **No `omarchy theme install <url>`.** See A2.
- **No fonts, spacing or radii from a theme.** A palette says nothing
  about them, and letting a theme move them would let a theme break a
  layout.
- **No theme in desired state.** A restore does not need to reproduce
  what each person likes to read on, and a statefile diff should not
  churn because somebody tried Nord for an afternoon.
