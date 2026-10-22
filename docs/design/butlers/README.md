# Handoff: Butlers screen (create, configure, manage)

## Overview

`console/src/views/Butlers.tsx` today does three things: list Butlers grouped by
surface, switch a Butler's model, and create one. Everything else the API can
already do — scope, access, tool narrowing, memory, sessions, retire, readiness
detail — has no screen. This handoff replaces that view with a roster plus a
tabbed detail drawer, a single create dialog, and two confirmations.

The design also fixes a structural mistake in the current screen: Butler cards
are rendered as `<li>` inside `.work-evidence`, which is a `display:flex;
align-items:baseline` row — so each Butler's name, directive and model select sit
side by side on one line instead of reading as a row of a roster.

One product rule drives the whole tool model: **a tool exists only while the
package behind it is connected.** There is no `send.email` without the mail
server connection. Counts, the Tools tab and the Reach tab all derive from that.

## About the Design Files

The files in `prototype/` are **design references created in HTML** — working
prototypes of the intended look and behaviour, not production code to copy.
The task is to recreate them in `console/` using its existing environment:
TypeScript/React, `useLoad`/`useAction`, `StatusPill`, `Drawer`, and the
Nocturne token set in `console/src/styles.css`.

Two hard repo rules apply to the recreation:

1. **No colour literals below the token block in `styles.css`.** The prototype
   uses hex values inline because it is a single standalone file; in the console
   every one of them must be the corresponding `var(--…)`. The mapping is in
   *Design Tokens* below.
2. **Console, CLI and Butler use the same public API.** No console-only
   endpoints. Where the design needs something the API cannot do yet, it is
   listed as a keystone change in `IMPLEMENTATION_PLAN.md`, not as a UI-only
   shortcut.

| File | What it is |
|---|---|
| `prototype/butlers-console-standalone.html` | **The high-fidelity prototype.** Self-contained, opens offline in a browser, fully clickable. |
| `prototype/Butlers Console.dc.html` | Source of the above (template + logic), if you want to read the state machine directly. |
| `prototype/Butlers Wireframes.dc.html` | The low-fidelity round it came from, including a faithful recreation of today's screen for before/after. |

## Fidelity

**High-fidelity.** Colours, type, spacing, radii and interaction states are
final and taken from `console/src/styles.css` (ADR-W027 Nocturne). Recreate the
UI to match, using the console's existing classes where they exist (`.btn`,
`.st`, `.chip`, `.card`, `.row`, `.muted`, `.sub`, `.drawer`) and adding the new
ones listed in the implementation plan.

Data in the prototype is fixture data (ten Butlers, five model routes). It is
representative, not real; the household in the prototype is the scale the design
targets (~10 Butlers).

---

## Screens / Views

### 1. Roster (`Butlers`)

**Purpose** — see every Butler, judge at a glance where its words go and whether
it is working, and get into one to change it.

**Layout** — inside the standard `.screen` box (padding `var(--space-6)`,
`max-width: 1180px`, own scroll). Top to bottom:

1. **Header row** — `display:flex; align-items:flex-end; gap:16px; flex-wrap:wrap`.
   Left: title `20px/500`, letter-spacing `-0.015em`; subtitle `.sub`
   ("One model each, its own directive and its own reach. A Butler on a cloud
   model sends what it is told outside the house."). Right: `New Butler`
   (`.btn .btn-primary`, plus icon) and `Model routes` (`.btn .btn-secondary`).
2. **Attention banner** — shown only when at least one non-retired Butler has
   `readiness !== "ready"`, and hidden while the `Needs attention` filter is
   active. Background `var(--st-warn-bg)`, `box-shadow: 0 0 0 1px` at 40% of
   `--st-warn`, radius `--radius-md`, padding `9px 12px`. Copy:
   `"2 Butlers need attention — nutrition can't reach its model, ops answers
   questions only"`, with a `Show only these` button on the right that sets the
   filter.
3. **Filter row** — chips `All / Household / Operator / Leaves the house /
   Needs attention / Retired`, each with its count. Selected chip: border
   `--color-accent`, background `color-mix(in srgb, var(--color-accent) 16%,
   transparent)`, ink `--mark`. Right side: search input (name + directive,
   case-insensitive substring) and a sort select (`last asked` / `name` /
   `needs attention`).
4. **Column header** — `10.5px/600`, letter-spacing `0.1em`, `--muted`, bottom
   hairline: `BUTLER · MODEL · WHERE ITS WORDS GO · WORKING? · REACH · LAST ASKED`.
5. **Surface groups** — `Household · 7` / `Operator · 3` with the existing
   blurbs, each followed by its rows. A group with no matching rows is not
   rendered. Retired Butlers appear only under the `Retired` filter.
6. **Rows** — one grid per Butler:
   `grid-template-columns: minmax(0,1.5fr) minmax(0,1.2fr) minmax(0,.85fr) minmax(0,1.1fr) 78px 34px; gap:12px; align-items:center; padding:10px 12px`,
   background `--color-surface`, radius `--radius-md`, `1px` border of `--hair`
   (amber at 40% when the Butler needs attention, `--color-accent` when it is
   the open one), `opacity:.62` when retired. Hover raises the border to
   `color-mix(in srgb, var(--color-text) 34%, transparent)`. Whole row is the
   click target; a caret sits in the last cell.

**Row cells**

| Cell | Content | Type |
|---|---|---|
| Butler | name `14px/500`; grey pill `retired` or `nobody can talk to it` when no person is ticked; directive below, `12px`, `--muted`, ellipsised | |
| Model · where its words go | `llama3.1:8b · studio` at `13px`, then a `.st` pill: `stays in the house` (`st-ok`) or `leaves the house` (`st-warn`), each with a 7px dot | pill |
| Working? | dot + word: `ready` (green dot, normal ink), `questions only` (amber dot and ink), `unavailable` (red dot and ink), `retired 12 Aug` (muted). Second line `11px --muted`: `llama3.1:8b probed 2m ago`, or the failure detail (`401 from provider`, `failed tool check`), or `kept, not deleted` | |
| Reach | `3 people · 2 connections` (singularised; `no people` when none; `operators` on the operator surface) and `5 of 6 tools`, with `· may propose plans` appended only when `propose.plan` is actually available and not off | |
| Last asked | `4m ago` / `never`, tabular numerals, `--muted` | |

**Empty result** — when no group has rows: a `.card`-like panel, "Nothing
matches that.", a hint line, and a `Clear filters` button.

### 2. Detail drawer

**Purpose** — everything about one Butler, without leaving the roster.

**Layout** — right-hand panel, `width: min(600px, 94vw)`, full height, ground
`#1b1d2b` (see token note), left border `--hair`, shadow
`-16px 0 40px rgba(0,0,0,.5)`, entering with `translateX(24px) → 0` and opacity
`.4 → 1` over 160ms ease-out. Scrim `rgba(10,11,20,.45)`, fades in over 140ms,
click closes. Escape closes (the existing `Drawer` component already does both).

Three bands: header, tab strip + scrolling body, sticky footer.

- **Header** — name `18px/500`; surface pill (`Household` / `Operator`, muted
  chip); destination `.st` pill; close button `28px` square. Second line
  `11px --muted` tabular: `made by tom · 14 Mar · 218 sessions`.
- **Tab strip** — `Identity · Model · Reach · Tools (5/6) · Memory (3) ·
  Sessions (3)`. Active tab: ink `--color-text`, 2px bottom border
  `--color-accent`; inactive: `--muted`, transparent border. Horizontally
  scrollable on narrow widths. Default tab on open: **Model**.
- **Footer** — `Save` (accent, disabled until the Identity draft is dirty),
  `Duplicate` (secondary), and `Retire` / `Bring back` pushed right in
  `--st-bad` / `--st-ok` outline.

**Identity tab** — `Name` text input; `What it is for` textarea with the note
"Shapes how it talks. It grants nothing — reach, tools and memory are set on
their own tabs."; a surface card stating the surface, its blurb, and "A surface
is granted at creation and cannot be changed. Duplicate this Butler onto the
other surface instead."; a provenance line.

**Model tab**

1. **ON NOW** card (`--color-surface`, `--shadow-sm`): eyebrow `ON NOW`, route
   `16px/500`, dot + `ready · tool check passed`, then
   `ollama · http://192.168.1.99:11434 · probed 2m ago` and a `Re-check` button
   that shows `probing…` for ~900ms and then restates readiness.
2. **ON YOUR HARDWARE** / **OUTSIDE THE HOUSE** — two labelled radio lists of
   every route. Row: 14px radio, `name · host`, and a right-aligned note in the
   matching colour (`ready`, `questions only`, `leaves the house`, `turned off
   in Model routes`). Selected row: accent border and `12%` accent fill.
   Disabled routes are dimmed and only produce a toast when clicked.
3. **TRY IT** — "asks this Butler on the model above": text input + `Ask`
   (shows `asking…` for ~1.1s). The answer lands in a surface card with a
   provenance line: `used 3 tools · 1.9s · llama3.1:8b · stayed in the house ·
   session bs-1843`. On a `qa-only` model the answer says it looked nothing up;
   on `unavailable`, Ask refuses with a toast instead.

**Reach tab**

- **WHO MAY TALK TO IT** — checkbox rows: 16px box (accent fill + check when
  on), person, address (`matrix · @tom:home`, or `no address linked`), and
  `asked 4m ago` / `never asked` right-aligned. Note below: normally "An
  unticked person cannot reach this Butler at all — it does not appear for
  them."; when nobody is ticked it becomes "Nobody can talk to this Butler. It
  will answer nothing until somebody here is ticked."
- **CONNECTIONS IT MAY READ** — chips, each labelled with its tool count
  (`withings ✓ · 1 tool`). Toggling one says what appeared or vanished:
  `grocery connected — write.list now available`. Note: "A tool exists only
  while the package behind it is connected — there is no send.email without the
  mail server."

**Tools tab**

- Header: `TOOLS — 5 of 6 allowed · 7 more come with connections`, and the
  profile default on the right.
- One row per **available** tool: monospace name, blurb, a tri-state
  `on / ask first / off` pill group, and usage (`92x / 7d`, or `off`).
- Amber panel when every tool is off: "Every tool is off. health will still
  talk, but it cannot look anything up."
- **Locked section** — `7 MORE TOOLS COME WITH CONNECTIONS`: dashed rows with
  the tool name muted, a `needs email` chip, the blurb, and a `Connect it`
  button that turns that connection on. Only packages the household actually
  has are listed — never an operator package on a household Butler.

**Memory tab** — one card per note: subject chip (accent tint), provenance
(`learned from chat · 6d ago`, `corrected by tom · 3mo ago`), `Correct` and
`Forget` buttons, and the note text. `Correct` opens a textarea inline with
`Save correction` / `Cancel`; `Forget` removes the note and warns in a toast.
Empty: "health has not been told anything worth remembering yet."

**Sessions tab** — one collapsed row per session: id (monospace), the opening
question, and `tom · 4m ago · 3 turns`. Expanding shows the turns
(`who` / `text` in a 56px column pair, separated by hairlines) and the trace
line `read.timeline, read.connection(withings), summarise · 1.9s`. Empty:
"Nobody has asked this Butler anything yet."

### 3. New Butler dialog

Centred over a `rgba(10,11,20,.6)` scrim, `min(620px,96vw)`, radius
`--radius-lg`, `--shadow-lg`, entering with `translateY(8px) scale(.985) → none`
over 160ms.

Fields in order: **START FROM** template chips (`Blank / Health / Kitchen /
Operator`, which fill the rest of the form), `Name`, `What it is for` +
"Shapes how it talks. It grants nothing.", `Surface` as two selectable cards,
`Model` as the full route radio list with local/cloud notes, and a
**YOU ARE MAKING** summary card:

> a household Butler called health on llama3.1:8b, which stays in the house.
> Nobody can talk to it until you give it an address.

The summary card turns amber (`--st-warn-bg` + amber ring) and switches to
"sends what it is told outside the house" the moment a cloud route is selected.

Footer: `Create`, `Create and open Reach` (opens the drawer on the Reach tab),
`Cancel`. Validation: empty name → "Give it a name — this is what people will
say to reach it."; duplicate name → "There is already a Butler called health.".
Either way a toast follows: "health made — give it an address so somebody can
talk to it".

### 4. Confirmations

Same dialog shell, `min(520px,96vw)`, with a 26px square status-tinted icon.

**Local → cloud** (fires only when the current route is local and the chosen one
is cloud):

> **This sends health's material outside the house**
> Everything anyone tells health from now on goes to anthropic. Its 3 remembered
> notes stay where they are.
> *WHAT CHANGES* — the conversation open right now carries over on its next
> message · 2 people talk to this Butler · Activity will record the crossing,
> with your name on it
> [Yes, switch it] [Keep llama3.1:8b]

**Retire**:

> **Retire nutrition?**
> It stops answering immediately. Nothing is deleted — you can bring it back
> from the Retired filter.
> *WHAT BREAKS* — 3 people lose the Butler they message · 2 routines call it —
> "sunday shop" and "meal plan" will fail · 2 sessions stay readable in the
> audit trail · 2 remembered notes are kept, not deleted
> [ ] Point those routines at kitchen instead
> [Retire it] [Keep it]

Cloud→local needs no confirmation; it is strictly a narrowing.

### 5. Empty state (no Butlers at all)

Wireframed in `Butlers Wireframes.dc.html` (option 2e), not built in the
hi-fi prototype: heading "Nobody answers yet.", the per-job explanation, three
template rows, an amber line when no model route exists ("A Butler needs a model
to be anything"), and `+ New Butler`.

---

## Interactions & Behavior

| Trigger | Result |
|---|---|
| Click row | Drawer opens on Model tab; row border turns accent; roster keeps scroll |
| Scrim click / Escape / ✕ | Drawer closes |
| Filter chip | Filters roster; `Retired` inverts the retired predicate |
| Search | Substring over `name + directive`, lowercased |
| Sort | `last asked` (age ascending, `never` last), `name`, `needs attention` first |
| Attention banner → Show only these | Sets filter to `attention`; banner hides itself |
| Pick a local route | Applies immediately, toast: "health now answers on qwen2.5:14b" |
| Pick a cloud route while local | Confirm dialog first; applying adds "— outside the house" to the toast |
| Pick a disabled route | No change; toast "gpt-4o is turned off in Model routes" |
| Re-check | ~900ms busy state, then readiness restated from the route probe |
| Ask | ~1.1s busy state, then an answer card stamped with tools used, latency, route and whether it left the house |
| Toggle person | Persists; toast names the consequence; the reach note flips when the last person is unticked |
| Toggle connection | Persists; tools that need it appear or disappear from the Tools tab; counts move |
| Connect it (locked tool) | Turns that connection on and moves the tool into the tri-state list |
| Tri-state tool | Persists per Butler; usage cell reads `off` when off; all-off shows the amber panel |
| Forget / Correct | Removes or rewrites the note, provenance becomes "corrected by tom · just now" |
| Duplicate | Copies directive, surface, route and tool states (never connections, secret grants or credentials); clears people, memory, sessions; opens the copy; toast "health copy made — nobody can talk to it yet" |
| Retire → Retire it | Sets `retiredAt`; row leaves the active roster; footer button becomes `Bring back` |
| Save (Identity) | Only enabled when dirty; renames the Butler and follows the rename in the drawer |
| Toasts | Bottom-centre pill, 2.8s, coloured dot (ok / warn / bad) |

Animations: `slidein` 160ms ease-out (drawer), `fadein` 120–180ms (scrims,
answer card), `pop` 160ms ease-out (dialogs, toast). All are transform/opacity
only; honour `prefers-reduced-motion` as `.pulse` already does.

## State Management

Server state through `useLoad`, writes through `useAction`, exactly as the rest
of the console does. Client state:

```ts
// roster
const [filter, setFilter] = useState<Filter>("all");   // all|family|admin|cloud|attention|retired
const [query, setQuery]   = useState("");
const [sort, setSort]     = useState<Sort>("active");   // active|name|health
const [open, setOpen]     = useState<string | null>(null); // butler name

// drawer
const [tab, setTab] = useState<Tab>("model");           // identity|model|reach|tools|memory|sessions
const [draft, setDraft] = useState<{ name: string; directive: string } | null>(null);
const [question, setQuestion] = useState("");
const [answer, setAnswer] = useState<AskResponse | null>(null);
const [openSession, setOpenSession] = useState<string | null>(null);
const [correcting, setCorrecting] = useState<number | null>(null);

// modals
const [confirm, setConfirm] = useState<
  { kind: "cloud"; route: string } | { kind: "retire" } | null
>(null);
const [creating, setCreating] = useState(false);
```

Data the screen needs per Butler: `api.butlers()` for the roster,
`api.modelRoutes()` for the route lists, and — lazily, when the drawer opens —
`api.butlerScope(name)`, `api.butlerToolControls(profile)`, `api.butlerMemory()`
and the Butler's sessions. Reload the roster after any write; the counts in the
roster come from the same rows.

Derived, never stored:

```ts
const available = (b: Butler, caps: Set<string>) =>
  TOOL_MAP[b.profile].filter((t) => !t.needs || caps.has(t.needs));
const allowed = (b, caps, controls) =>
  available(b, caps).filter((t) => stateOf(controls, t.name) !== "off");
```

`leavesTheHouse`, `carriesActions` and `reachable` stay server-computed on
`ButlerRow` — do not re-derive them in the client.

## Design Tokens

Every value below is already in `console/src/styles.css`. The prototype's hex
literals map one-to-one:

| Prototype literal | Token |
|---|---|
| `#161826` | `--color-bg` |
| `#232532` | `--color-surface` |
| `#1b1d2b` (drawer ground) | **new**: `--color-elevated`, defined per theme next to the other grounds |
| `#e9e9ed` | `--color-text` |
| `#9184d9` | `--color-accent` |
| `#d2cefd` | `--mark` |
| `rgba(233,233,237,.11)` | `--hair` |
| `rgba(233,233,237,.07)` | `--faint` |
| `rgba(233,233,237,.58)` | `--muted` |
| `#5fae86` / `#1b2c25` | `--st-ok` / `--st-ok-bg` |
| `#d9a24b` / `#2e2718` | `--st-warn` / `--st-warn-bg` |
| `#d9736f` / `#301f21` | `--st-bad` / `--st-bad-bg` |
| `0 0 0 1px #3f424d` | `--shadow-sm` |
| `0 0 0 1px #595d6c, 0 16px 40px rgba(0,0,0,.65)` | `--shadow-lg` |

Spacing: `--space-1: 3px`, `-2: 6px`, `-3: 8px`, `-4: 11px`, `-6: 17px`,
`-8: 22px`. The prototype's `10px`/`12px` paddings round to `--space-4`, its
`gap:12px` to `--space-4`, its `gap:6px` to `--space-2`.

Radii: `--radius-sm: 4px` (chips' inner), `--radius-md: 8px` (rows, inputs,
buttons), `--radius-lg: 14px` (dialogs). Pills are `999px`.

Type: `--font-body` (bundled Inter Variable) at `14px/1.55` base; headings
`500` weight with `-0.015em` tracking; eyebrows `11px/600` at `0.13em`
uppercase; section eyebrows inside the drawer `10.5px/600` at `0.1em`;
`--font-mono` for tool names, ids, endpoints and every number in a column
(`font-variant-numeric: tabular-nums`).

Contrast: all new muted text sits on `--color-surface` or `--color-bg`, so
`npm run contrast-check` covers it unchanged. Do not introduce alpha-muted ink
on the status backgrounds — status pills keep full-opacity `--st-*` ink.

## Assets

- **Icons**: `@phosphor-icons/react`, already a dependency. Used here: `Plus`,
  `X`, `CaretRight`, `ArrowsClockwise`, `CircleNotch` (spinner), `Check`,
  `Warning`, `Archive`, `UsersThree`. Rail icons are unchanged from `App.tsx`.
  (The standalone prototype loads the Phosphor *web font* from a CDN because it
  is a single file — the console must keep using the React package and its
  bundled font; nothing is fetched at runtime.)
- **Fonts**: bundled Inter Variable via `@fontsource-variable/inter`. No new
  fonts.
- No images.

## Files

```
design_handoff_butlers_screen/
  README.md                                  this document
  IMPLEMENTATION_PLAN.md                     phased plan, code examples, API gaps
  prototype/
    butlers-console-standalone.html          hi-fi clickable prototype (open this)
    Butlers Console.dc.html                  its source
    Butlers Wireframes.dc.html               lo-fi round + recreation of today's screen
```

Source files this design replaces or touches in the repo:

| Repo file | Change |
|---|---|
| `console/src/views/Butlers.tsx` | replaced by the roster; card/create code moves out |
| `console/src/views/butlers/*` | new: drawer, six tabs, create dialog |
| `console/src/ui/Confirm.tsx` | new: the two confirmations |
| `console/src/ui/Drawer.tsx` | reused as-is (`wide`), plus a tab strip |
| `console/src/styles.css` | new roster/drawer/tool-row rules, token-only |
| `console/src/api.ts` | new calls listed in the implementation plan |
| `console/scripts/render-check.tsx` | new states to cover |
