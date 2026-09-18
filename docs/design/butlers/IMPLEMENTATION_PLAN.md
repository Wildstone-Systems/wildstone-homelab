# Implementation plan — Butlers screen

Read `README.md` first for the design itself. This file is the order of work,
the API gaps that must close first, and the code shapes to write.

Target: `console/` (TypeScript/React, Vite). Verification loop is what CI runs:
`npm run build` (tsc --noEmit + vite build), `npm run render-check`,
`npm run contrast-check`.

---

## Phase 0 — close the API gaps (keystone + proto first)

The design needs seven things the public API cannot do today. Console, CLI and
Butler share one API (ground rule 3), so each is a keystone change with a proto
schema, not a UI workaround.

| # | Need | Today | Change |
|---|---|---|---|
| 1 | Rename / re-word a Butler | `createButler`, `setButlerRoute`, `retireButler` only | `PATCH /api/v1/butlers/{name}` → `{ directive?, displayName? }`. Prefer a stable id + mutable display name; if the name stays the key, the handler must move scope, controls and sessions with it. |
| 2 | Un-retire | `DELETE` retires; nothing restores | `POST /api/v1/butlers/{name}/restore` |
| 3 | Duplicate | — | `POST /api/v1/butlers/{name}/duplicate` → `{ name }`, copying directive, surface, route, connections and tool states, copying **no** access, memory or sessions. Client-side duplicate would need six calls and could half-fail. |
| 4 | Per-Butler tool narrowing | `setButlerToolControl({ profile, tool, state, reason })` is **per profile**, and `state` is only `enabled\|disabled` | Add butler scope and the third state: `PUT /api/v1/butlers/{name}/tools` → `{ tool, state: "enabled"\|"confirm"\|"disabled", reason }`. `ButlerScope.narrowedTools` already carries a per-Butler map, so the read side exists. **If `confirm` is not implemented in the Butler loop, ship the tri-state as a two-state control and drop "ask first" from the UI** — do not render a control the engine ignores. |
| 5 | Per-Butler connections | `ButlerScope.connections` is readable, not writable | `PUT /api/v1/butlers/{name}/connections` → `{ connection, allow }` |
| 6 | Ask one named Butler | `askButler(message, surface)` hits the surface, not the Butler | `POST /api/v1/butlers/{name}/ask` → the existing `AskResponse`, so the answer card can name the route it used |
| 7 | Retire impact + probe | — | `GET /api/v1/butlers/{name}/impact` → `{ people, routines, openSessions, memoryCount, connectionsUsedNowhereElse }` for the *WHAT BREAKS* list; `POST /api/v1/butler/routes/{name}/probe` for `Re-check` |

Sessions per Butler: `butlerSessions(limit, surface)` returns both surfaces'
sessions with a `surface` stamp but no Butler stamp. Either add `butler` to
`ButlerSession` and a `?butler=` filter, or drop the Sessions tab from the first
release — filtering client-side by surface would show another Butler's
conversations under this one's name, which is the one lie this screen must not
tell.

### api.ts additions

```ts
// console/src/api.ts — alongside the existing butler calls
updateButler: (name: string, patch: { directive?: string; displayName?: string }) =>
  call<ButlerRow>("PATCH", `/api/v1/butlers/${encodeURIComponent(name)}`, patch),
restoreButler: (name: string) =>
  call<ButlerRow>("POST", `/api/v1/butlers/${encodeURIComponent(name)}/restore`, {}),
duplicateButler: (name: string) =>
  call<{ name: string }>("POST", `/api/v1/butlers/${encodeURIComponent(name)}/duplicate`, {}),
setButlerTool: (name: string, tool: string, state: ToolState, reason = "") =>
  call<{ note?: string }>("PUT", `/api/v1/butlers/${encodeURIComponent(name)}/tools`,
    { tool, state, reason }),
setButlerConnection: (name: string, connection: string, allow: boolean) =>
  call<{ connections: string[] | null }>("PUT",
    `/api/v1/butlers/${encodeURIComponent(name)}/connections`, { connection, allow }),
askOneButler: (name: string, message: string) =>
  call<AskResponse>("POST", `/api/v1/butlers/${encodeURIComponent(name)}/ask`, { message }),
butlerImpact: (name: string) =>
  call<ButlerImpact>("GET", `/api/v1/butlers/${encodeURIComponent(name)}/impact`),
probeRoute: (route: string) =>
  call<{ route: ModelRoute }>("POST",
    `/api/v1/butler/routes/${encodeURIComponent(route)}/probe`, {}),
```

```ts
export type ToolState = "enabled" | "confirm" | "disabled";

export interface ButlerImpact {
  butler: string;
  people: string[] | null;
  routines: { name: string; state: string }[] | null;
  openSessions: number;
  memoryCount: number;
  connectionsUsedNowhereElse: string[] | null;
}
```

---

## Phase 1 — the tool model

This is the part to get right first, because the roster counts, the Tools tab
and the Reach tab all read from it. **A tool exists only while the package
behind it is connected.**

The surface's tool map is the structural ceiling (`ToolControl`'s comment: a
control "can never add one"). Availability is that ceiling intersected with the
capabilities of the connections in this Butler's scope. `IntegrationCapability`
already carries `kind: "event" | "query" | "action"`, `name` and `available`, so
the ceiling entries should name the capability they need rather than the
connection, and the connection is whatever provides it.

```ts
// console/src/views/butlers/tools.ts
export interface ToolDef {
  name: string;                 // "send.email"
  blurb: string;                // "send mail as the household"
  needs?: string;               // capability name, e.g. "mail.send"
  surfaces: ButlerSurface[];    // propose.plan is admin-only
}

/** Which capability names this Butler's connected packages currently offer. */
export function capabilitiesOf(
  scope: ButlerScope,
  integrations: { connection: IntegrationConnection; capabilities: IntegrationCapability[] | null }[],
): Set<string> {
  const mine = new Set(scope.connections ?? []);
  const out = new Set<string>();
  for (const row of integrations) {
    if (!mine.has(row.connection.name)) continue;
    if (row.connection.state === "unavailable") continue;
    for (const c of row.capabilities ?? []) if (c.available) out.add(c.name);
  }
  return out;
}

export interface ToolView { def: ToolDef; state: ToolState; provider?: string }

export function toolsFor(
  b: Butler,
  caps: Set<string>,
  scope: ByutlerScope,
  ceiling: ToolDef[],
): { available: ToolView[]; locked: { def: ToolDef; needsConnection: string }[] } {
  const narrowed = scope.narrowedTools ?? {};
  const forSurface = ceiling.filter((t) => t.surfaces.includes(b.surface));
  const available: ToolView[] = [];
  const locked: { def: ToolDef; needsConnection: string }[] = [];
  for (const def of forSurface) {
    if (!def.needs || caps.has(def.needs)) {
      available.push({ def, state: (narrowed[def.name] as ToolState) ?? "enabled" });
    } else {
      // Only offer a package this household actually has — never an
      // operator package on a household Butler.
      const provider = providerOf(def.needs);           // from the integrations list
      if (provider) locked.push({ def, needsConnection: provider });
    }
  }
  return { available, locked };
}
```

Counts, used verbatim by both the roster and the drawer:

```ts
const allowed = available.filter((t) => t.state !== "disabled").length;
const total   = available.length;                 // NOT the ceiling length
const more    = locked.length;
// roster:  `${allowed} of ${total} tools`
// drawer:  `${allowed} of ${total} allowed` + (more ? ` · ${more} more come with connections` : "")
```

Two mistakes the prototype hit and fixed — assert against both:

- the denominator must be *available* tools, never the whole ceiling
  (`5 of 6`, not `5 of 13`);
- `· may propose plans` on the roster is true only when `propose.plan` is
  available **and** not disabled — an operator Butler with no keystone
  connection cannot propose anything.

---

## Phase 2 — file layout

```
console/src/views/Butlers.tsx              roster: header, banner, filters, groups, rows
console/src/views/butlers/ButlerDrawer.tsx drawer shell + tab strip + footer
console/src/views/butlers/TabIdentity.tsx
console/src/views/butlers/TabModel.tsx      on-now card, route lists, try-it
console/src/views/butlers/TabReach.tsx      people + connections
console/src/views/butlers/TabTools.tsx      tri-state rows + locked section
console/src/views/butlers/TabMemory.tsx
console/src/views/butlers/TabSessions.tsx
console/src/views/butlers/NewButlerDialog.tsx
console/src/views/butlers/tools.ts          the model above
console/src/ui/Confirm.tsx                  shared confirmation dialog
console/src/ui/Tabs.tsx                     shared tab strip (Work may want it too)
```

`App.tsx` is unchanged: `butlers: ["Who answers", "Butlers"]` and the
`UsersThree` rail item already exist, and the screen keeps scrolling inside
`.screen` (it is not full-bleed).

### Roster

```tsx
export function Butlers() {
  const butlers = useLoad(() => api.butlers());
  const routes = useLoad(() => api.modelRoutes());
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState<Sort>("active");
  const [open, setOpen] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  const rows = butlers.data?.butlers ?? [];
  const attention = rows.filter((r) => !r.butler.retiredAt && r.butler.readiness !== "ready");
  const shown = useMemo(() => select(rows, { filter, query, sort }), [rows, filter, query, sort]);

  return (
    <>
      <div className="roster-head">
        <div>
          <h2>Butlers</h2>
          <div className="sub">
            One model each, its own directive and its own reach. A Butler on a cloud model
            sends what it is told outside the house.
          </div>
        </div>
        <div className="row">
          <button className="btn btn-primary" onClick={() => setCreating(true)}>
            <Plus size={15} /> New Butler
          </button>
          <button className="btn btn-secondary">Model routes</button>
        </div>
      </div>

      <ErrorLine error={butlers.error} />
      <ErrorLine error={routes.error} />

      {attention.length > 0 && filter !== "attention" && (
        <AttentionBanner rows={attention} onShow={() => setFilter("attention")} />
      )}

      <RosterFilters rows={rows} filter={filter} onFilter={setFilter}
                     query={query} onQuery={setQuery} sort={sort} onSort={setSort} />

      <div className="roster-cols" aria-hidden>
        <div>BUTLER</div><div>MODEL · WHERE ITS WORDS GO</div><div>WORKING?</div>
        <div>REACH</div><div>LAST ASKED</div><div />
      </div>

      {SURFACES.map((g) => {
        const mine = shown.filter((r) => r.butler.surface === g.key);
        if (mine.length === 0) return null;
        return (
          <section key={g.key} className="roster-group">
            <div className="row roster-grouphead">
              <h4>{g.title} · {mine.length}</h4>
              <div className="muted">{g.blurb}</div>
            </div>
            {mine.map((r) => (
              <RosterRow key={r.butler.name} row={r}
                         selected={open === r.butler.name}
                         onOpen={() => setOpen(r.butler.name)} />
            ))}
          </section>
        );
      })}

      {shown.length === 0 && !butlers.loading && (
        <NoMatches filter={filter} onClear={() => { setFilter("all"); setQuery(""); }} />
      )}

      {open && (
        <ButlerDrawer name={open} routes={routes.data?.routes ?? []}
                      onClose={() => setOpen(null)} onChanged={butlers.reload} />
      )}
      {creating && (
        <NewButlerDialog routes={routes.data?.routes ?? []}
                         onClose={() => setCreating(false)}
                         onCreated={(name, tab) => { butlers.reload(); setCreating(false); setOpen(name); }} />
      )}
    </>
  );
}
```

`RosterRow` is a `<button className="rowcard">` (whole row is the target — the
prototype's `div` is a prototype convenience; make it a real button or add
`role="button"` + key handling):

```tsx
function RosterRow({ row, selected, onOpen }: { row: ButlerRow; selected: boolean; onOpen: () => void }) {
  const b = row.butler;
  const h = healthOf(row);          // { kind: Kind, label, detail }
  return (
    <button type="button" onClick={onOpen}
      className={`rowcard${selected ? " on" : ""}${h.kind === "ok" ? "" : " attn"}${b.retiredAt ? " retired" : ""}`}>
      <div className="cell">
        <div className="row rowcard-name">
          <strong>{b.name}</strong>
          {b.retiredAt && <span className="st st-mute">retired</span>}
          {!b.retiredAt && row.people === 0 && <span className="st st-mute">nobody can talk to it</span>}
        </div>
        <div className="muted ellip">{b.directive}</div>
      </div>
      <div className="cell">
        <div className="ellip">{b.route} · {b.provider}</div>
        <StatusPill kind={row.leavesTheHouse ? "warn" : "ok"} icon={<Dot kind={row.leavesTheHouse ? "warn" : "ok"} />}>
          {row.leavesTheHouse ? "leaves the house" : "stays in the house"}
        </StatusPill>
      </div>
      <div className="cell">
        <div className="row"><Dot kind={h.kind} /><span className={`ink-${h.kind}`}>{h.label}</span></div>
        <div className="muted ellip">{h.detail}</div>
      </div>
      <div className="cell muted">
        <div>{reachLine(row)}</div>
        <div>{toolLine(row)}</div>
      </div>
      <div className="muted tnum">{ago(b.lastAskedAt)}</div>
      <CaretRight size={14} />
    </button>
  );
}
```

`healthOf` is the one place readiness becomes words — keep it beside
`kindOf` in `ui/status.tsx` so no screen grows its own opinion:

```ts
export function healthOf(row: ButlerRow): { kind: Kind; label: string; detail: string } {
  const b = row.butler;
  if (b.retiredAt) return { kind: "mute", label: `retired ${when(b.retiredAt)}`, detail: "kept, not deleted" };
  if (!row.reachable) return { kind: "bad", label: "unavailable", detail: b.readinessDetail ?? "" };
  if (!row.carriesActions) return { kind: "warn", label: "questions only", detail: b.readinessDetail ?? "failed tool check" };
  return { kind: "ok", label: "ready", detail: `${b.route} probed ${ago(b.probedAt)}` };
}
```

### Drawer + tabs

Reuse `ui/Drawer.tsx` (it already owns the scrim, Escape and focus) with
`wide`, and give it a tab strip:

```tsx
export function ButlerDrawer({ name, routes, onClose, onChanged }: Props) {
  const [tab, setTab] = useState<Tab>("model");
  const scope = useLoad(() => api.butlerScope(name), [name]);
  const integrations = useLoad(() => api.integrations());
  const row = /* from the roster's rows, passed in or re-read */;

  return (
    <Drawer open onClose={onClose} wide title={`Butler ${name}`}>
      <DrawerHead row={row} />
      <Tabs
        current={tab}
        onSelect={setTab}
        items={[
          { id: "identity", label: "Identity" },
          { id: "model", label: "Model" },
          { id: "reach", label: "Reach" },
          { id: "tools", label: "Tools", count: `${allowed}/${total}` },
          { id: "memory", label: "Memory", count: scope.data?.scope.memoryCount || undefined },
          { id: "sessions", label: "Sessions", count: sessions.data?.length || undefined },
        ]}
      />
      <div className="drawer-body">
        {tab === "identity" && <TabIdentity row={row} onSaved={onChanged} />}
        {tab === "model" && <TabModel row={row} routes={routes} onChanged={onChanged} />}
        {tab === "reach" && <TabReach row={row} scope={scope} onChanged={onChanged} />}
        {tab === "tools" && <TabTools row={row} scope={scope} integrations={integrations} onChanged={onChanged} />}
        {tab === "memory" && <TabMemory row={row} />}
        {tab === "sessions" && <TabSessions row={row} />}
      </div>
      <DrawerFoot row={row} onChanged={onChanged} onClose={onClose} />
    </Drawer>
  );
}
```

### Model switch — keep the response

`setButlerRoute` returns `{ crossesToCloud?, warning?, previousModel? }`, and
that warning is the only place the crossing sentence exists. The current screen
already keeps it rather than using `useAction`; keep doing that, and put the
confirmation *before* the call:

```tsx
function pick(route: ModelRoute) {
  if (route.name === b.route) return;
  if (!route.enabled) { toast(`${route.name} is turned off in Model routes`, "warn"); return; }
  if (route.kind === "cloud" && b.routeKind === "local") { setConfirm({ kind: "cloud", route: route.name }); return; }
  apply(route.name);
}

async function apply(route: string) {
  setBusy(true);
  try {
    const res = await api.setButlerRoute(b.name, route);
    setWarning(res.warning ?? null);   // stated, not asked
    onChanged();
  } catch (e) { setError((e as Error).message); } finally { setBusy(false); }
}
```

Cloud→local needs no confirmation. Only local→cloud does, and its body reads
from `api.butlerImpact(name)` so the counts are real ("2 people talk to this
Butler", "the conversation open right now carries over").

### Retire

```tsx
const impact = useLoad(() => api.butlerImpact(b.name), [b.name]);
// WHAT BREAKS lines, in this order, omitting any that is zero:
//   `${people.length} people lose the Butler they message`
//   `${routines.length} routines call it — "sunday shop" and "meal plan" will fail`
//   `${openSessions} conversations are open right now`
//   `${sessionCount} sessions stay readable in the audit trail`
//   `${memoryCount} remembered notes are kept, not deleted`
// The "point those routines at X instead" checkbox only renders when
// routines.length > 0 and another Butler on the same surface can take them.
```

---

## Phase 3 — styles

Add to `console/src/styles.css`, below the existing sections, **token values
only** (no colour literals — that rule is what makes the light theme a token
swap):

```css
/* ---------------------------------------------------------- butlers */
.roster-head { display: flex; align-items: flex-end; gap: var(--space-6); flex-wrap: wrap; margin-bottom: var(--space-4); }
.roster-head > div:first-child { flex: 1; min-width: 280px; }

.roster-cols,
.rowcard {
  display: grid;
  grid-template-columns: minmax(0,1.5fr) minmax(0,1.2fr) minmax(0,.85fr) minmax(0,1.1fr) 78px 34px;
  gap: var(--space-4);
}
.roster-cols { padding: 0 var(--space-4) var(--space-2); font-size: 10.5px; font-weight: 600; letter-spacing: .1em; color: var(--muted); border-bottom: 1px solid var(--hair); }
.roster-group { margin-top: var(--space-6); display: flex; flex-direction: column; gap: var(--space-2); }

.rowcard {
  align-items: center; text-align: left; width: 100%;
  padding: var(--space-4); border-radius: var(--radius-md);
  background: var(--color-surface); border: 1px solid var(--hair);
  color: var(--color-text); font: inherit; cursor: pointer;
  transition: border-color .12s, background .12s;
}
.rowcard:hover { border-color: color-mix(in srgb, var(--color-text) 34%, transparent); }
.rowcard.on { border-color: var(--color-accent); }
.rowcard.attn { border-color: color-mix(in srgb, var(--st-warn) 40%, transparent); }
.rowcard.retired { opacity: .62; }
.rowcard .cell { min-width: 0; }
.ellip { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ink-warn { color: var(--st-warn); } .ink-bad { color: var(--st-bad); }

.tool-row { display: grid; grid-template-columns: minmax(0,1.15fr) auto minmax(0,.9fr); gap: var(--space-4); align-items: center; padding: var(--space-3) var(--space-4); border-radius: var(--radius-md); background: var(--color-surface); box-shadow: var(--shadow-sm); }
.tool-row .name { font-family: var(--font-mono); font-size: 13.5px; }
.tool-locked { border: 1px dashed color-mix(in srgb, var(--color-text) 16%, transparent); background: color-mix(in srgb, var(--color-surface) 50%, transparent); }

.tri { display: flex; gap: 3px; }
.tri button { font-size: 11.5px; padding: 3px 9px; border-radius: 999px; border: 1px solid var(--hair); background: transparent; color: var(--muted); cursor: pointer; }
.tri button[aria-pressed="true"] { border-color: var(--color-accent); background: color-mix(in srgb, var(--color-accent) 16%, transparent); color: var(--mark); }

@media (max-width: 900px) {
  /* the roster becomes two lines per Butler rather than six columns */
  .roster-cols { display: none; }
  .rowcard { grid-template-columns: minmax(0,1fr) auto; row-gap: var(--space-2); }
}
```

Also add the drawer's slightly-lifted ground as a token next to the others
(`--color-elevated: #1b1d2b` dark / a light-theme equivalent) — the drawer must
not be `--color-surface`, or the cards inside it stop reading as cards.

---

## Phase 4 — checks

1. **`npm run render-check`** — add states: roster with 10 Butlers; roster
   filtered to `attention`; roster empty result; each of the six tabs; the
   locked-tools section non-empty; both confirmations; the create dialog with a
   cloud route selected; the no-Butlers empty state.
2. **`npm run contrast-check`** — passes unchanged if you used tokens. The two
   places to watch: the `attn` row border and the amber "every tool is off"
   panel, both of which must keep full-opacity `--st-warn` ink.
3. **`npm run build`** — `tsc --noEmit` will catch the `ToolState` widening in
   `setButlerToolControl` callers.
4. **`npm run ui-check`** (manual, needs a live Keystone) — the flow worth
   driving: open a household Butler, switch it to a cloud route, confirm, then
   check Activity recorded the crossing with the operator's name.

## Order of work, if you want it sequenced

1. Phase 0 keystone + proto changes (nothing below lands honestly without them).
2. `tools.ts` + `healthOf` + counts, with unit coverage on the two count bugs.
3. Roster (rows, filters, banner, groups, empty result) behind the existing view.
4. Drawer shell + Model tab (the tab that replaces today's inline select).
5. Reach and Tools tabs, connection→tool gating end to end.
6. Memory and Sessions tabs.
7. Create dialog, then the two confirmations.
8. Delete the old `Roster` / `ButlerCard` / `NewButler` code from `Butlers.tsx`,
   and drop the `.work-evidence` usage — that class is Work's, and reusing it
   here is what made every Butler render as one baseline-aligned line.

## Deliberate omissions

- **No approve-your-own-change ceremony.** Switching a model applies at once
  (ADR-W031 A8); what the person gets is the consequence, stated. The only
  friction is the local→cloud confirmation and the retire confirmation.
- **Surface is not editable.** It is granted at creation; the Identity tab says
  so and points at Duplicate.
- **The empty state is wireframe-only** (`Butlers Wireframes.dc.html`, 2e).
  Build it from that if a household can reach this screen with no Butlers and no
  model routes.
