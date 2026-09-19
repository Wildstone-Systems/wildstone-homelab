# ADR-W036: A theme belongs to a person, not to the household

- **Status:** accepted, and built. Everything in Part A is implemented:
  `keystone/theme`, migrations 0060 and 0061, `handlers_appearance.go`,
  the Console's rail picker and the household dashboard's.
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
Migration 0060 keys it on `person_id`, per 0043, so a rename moves the
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

## A2.1 A theme's picture is the household's to supply

An Omarchy theme is not only a palette: every one of them ships a
`backgrounds/` directory, and tokyo-night has eight wallpapers in it. A
theme without its picture is half a skin, so themes here carry one too.

The pictures are **not** vendored, and the palettes are, and the
difference is size and provenance. A palette is twenty-six hex values —
a few kilobytes for all twenty-two, and a list of colours is not
somebody's artwork. The wallpapers run between half a megabyte and a
megabyte and a half **each**: one per shipped theme would add fifteen
megabytes to a binary that gets downloaded onto home servers, and the
full sets over a hundred. They are also art by various hands, which is
the call `packages/dashboard/README.md` already declines to make
incidentally about third-party logos.

So a household supplies its own, uploaded from the theme's
`backgrounds/` directory — the same act as pasting the `colors.toml`,
one file further, and still nothing fetched by the control plane.

A picture attaches **by theme name**, in its own table (migration 0061),
independent of whether the palette ships in the binary or was pasted.
Otherwise putting a wallpaper behind Nord would mean re-pasting Nord's
palette first.

**Consequence.** Removing a pasted theme removes its picture too —
unlike a person's choice, which is a few bytes of text that re-attach by
themselves, an orphaned wallpaper is megabytes that nothing lists and
nothing can remove.

## A2.2 The picture is the page's ground, and the inks are derived for it

The wallpaper is the **background of the household dashboard**, not its
banner: a layer in the page's own background stack, fixed, covering,
behind everything. The household banner is a separate thing and is
untouched.

That means everything drawn directly on the page — headings, the muted
labels between cards, the eyebrow rows — is drawn over an arbitrary
photograph, and a photograph has no contrast floor. It can be white
where the ink is white and black where the ink is black, in one image.

So the page colour stays in front of the picture at a fixed opacity
(`theme.Veil`), and every ink is derived against the two extremes that
veil can produce — the ground over a white pixel and the ground over a
black one. Checking the extremes is sufficient because luminance is
monotonic along the blend, and the repair is given a slightly thinner
veil than the one that ships, so run-time compositing rounds into
headroom rather than out of it.

**Consequence, worth stating because it is visible.** A person who
chooses a theme *with* a picture gets slightly brighter ink than the same
theme without one. That is what it costs to stay legible over an unknown
image, and `TestEveryShippedThemeIsLegibleOverAnyWallpaper` is what holds
it. The veil is one number and it is the only judgement in the
derivation: lower shows more picture and pushes the inks further.

Cards stay fully opaque, so everything inside one is exactly as it was.
The Console gets no wallpaper at all — it is the screen somebody opens
when the house is broken, and ADR-W027's density is drawn on a flat
ground on purpose. It has no read route for the bytes; it only manages
them.

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

## A9. Choosing is a Member's act, on the surface a Member has

A household Member has no Console — that is ADR-W032 and ADR-W033 A1,
and nothing here changes it. So the picker is on the **household
dashboard**, reachable through the portal door, which holds no RBAC verb
at all: a Member switches theme from the page they already use, with no
Console account and no Owner in the loop.

The Console's picker is the same choice for whoever is on the Console,
plus the Owner-only acts — adding a theme, and adding or removing its
picture — because those change what the whole household can choose from.

## What this does not do

- **No per-surface themes.** One choice, every Wildstone screen. Two
  would be two things to keep in sync and two places to be confused by.
- **No `omarchy theme install <url>`.** See A2.
- **No fonts, spacing or radii from a theme.** A palette says nothing
  about them, and letting a theme move them would let a theme break a
  layout.
- **No wallpaper on the Console, and none on the embedded widgets.**
  See A2.2 and A8.
- **No theme in desired state.** A restore does not need to reproduce
  what each person likes to read on, and a statefile diff should not
  churn because somebody tried Nord for an afternoon.
