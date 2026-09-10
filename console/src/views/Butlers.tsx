import { useMemo, useState } from "react";
import { CaretRight, Plus } from "@phosphor-icons/react";
import { api, modelLabel, type ButlerRow, type ButlerSurface, type Model } from "../api";
import { ErrorLine, useLoad, when } from "../lib";
import { Dot, healthOf, StatusPill } from "../ui/status";
import { Toast, useToast } from "../ui/Toast";
import { ButlerDrawer } from "./butlers/ButlerDrawer";
import { NewButlerDialog } from "./butlers/NewButlerDialog";

// The household's Butlers (ADR-W031, work package B6).
//
// There is not one Butler. There are as many as the household made,
// each with its own model, its own directive and its own scope — a
// health Butler on the local model, a nutrition Butler on a cloud one.
//
// This screen works hard on exactly one thing: making it impossible to
// look at a Butler without seeing where its words go. That is A2 and A4
// rendered. Somebody deciding to tell a Butler something sensitive is
// relying entirely on which model sits behind it, and a screen that
// puts that a click away has failed at the only moment it mattered.
// It is why the destination is a column in the roster and a pill in the
// drawer header, rather than something you find by opening one.
//
// Switching a model applies at once (A8): the person here owns every
// Butler, so a proposal they would then approve themselves is ceremony
// with nobody able to refuse it. What they get instead is the
// consequence — and going local-to-cloud, a dialog saying what happens
// to a conversation already open.

export type Filter = "all" | "family" | "admin" | "cloud" | "attention" | "retired";
export type Sort = "active" | "name" | "health";

export const SURFACES: { key: ButlerSurface; title: string; blurb: string }[] = [
  { key: "family", title: "Household", blurb: "Reads the home. Cannot touch the home lab." },
  { key: "admin", title: "Operator", blurb: "Can propose changes to the home lab. Never approves them." },
];

export function needsAttention(row: ButlerRow): boolean {
  return !row.butler.retiredAt && (!row.reachable || !row.carriesActions);
}

// Which rows survive the filter, the search and the sort.
//
// It is a plain function rather than three hooks so it can be tested
// without rendering, and so the "Retired" filter's inversion lives in
// one readable place: every other filter narrows the LIVE Butlers, and
// Retired is the only one that looks at the other set entirely. Written
// as a chain of conditions it reads as a bug the first time somebody
// wonders why retired Butlers vanish from "All".
export function select(
  rows: ButlerRow[],
  opts: { filter: Filter; query: string; sort: Sort },
): ButlerRow[] {
  const q = opts.query.trim().toLowerCase();
  const kept = rows.filter((r) => {
    const b = r.butler;
    if (opts.filter === "retired") {
      if (!b.retiredAt) return false;
    } else if (b.retiredAt) return false;
    if (opts.filter === "family" && b.surface !== "family") return false;
    if (opts.filter === "admin" && b.surface !== "admin") return false;
    if (opts.filter === "cloud" && !r.leavesTheHouse) return false;
    if (opts.filter === "attention" && !needsAttention(r)) return false;
    if (q && !`${b.name} ${b.directive ?? ""}`.toLowerCase().includes(q)) return false;
    return true;
  });
  const sorted = kept.slice();
  if (opts.sort === "name") {
    sorted.sort((a, b) => a.butler.name.localeCompare(b.butler.name));
  } else if (opts.sort === "health") {
    sorted.sort((a, b) => Number(needsAttention(b)) - Number(needsAttention(a)));
  } else {
    // Most recently touched first, and a Butler nobody has ever asked
    // sorts last rather than first — "never" is not the freshest thing
    // on the list.
    const at = (r: ButlerRow) => new Date(r.butler.updatedAt || r.butler.createdAt).getTime() || 0;
    sorted.sort((a, b) => at(b) - at(a));
  }
  return sorted;
}

// What the reach cell says. Singularised, because "1 people" is the
// kind of detail that makes a reader distrust the rest of the row.
export function reachLine(row: ButlerRow): string {
  const noun = row.butler.surface === "admin" ? "operator" : "person";
  const plural = row.butler.surface === "admin" ? "operators" : "people";
  const who = row.people === 0 ? "no people" : `${row.people} ${row.people === 1 ? noun : plural}`;
  return `${who} · ${row.connections} ${row.connections === 1 ? "connection" : "connections"}`;
}

export function toolLine(row: ButlerRow): string {
  return `${row.toolsAllowed} of ${row.toolsAvailable} tools`;
}

export function Butlers() {
  const butlers = useLoad(() => api.butlers());
  const models = useLoad(() => api.models());
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState<Sort>("active");
  const [open, setOpen] = useState<string | null>(null);
  const [openTab, setOpenTab] = useState<string | undefined>(undefined);
  const [creating, setCreating] = useState(false);
  const { toast, say } = useToast();

  const rows = butlers.data?.butlers ?? [];
  const modelRows = (models.data?.models ?? []).map((r) => r.model);
  const attention = rows.filter(needsAttention);
  const shown = useMemo(() => select(rows, { filter, query, sort }), [rows, filter, query, sort]);
  const groups = SURFACES.map((g) => ({
    ...g,
    rows: shown.filter((r) => r.butler.surface === g.key),
  })).filter((g) => g.rows.length > 0);

  return (
    <>
      <div className="roster-head">
        <div>
          <h2>Butlers</h2>
          <div className="sub">
            One model each, its own directive and its own reach. A Butler on a cloud model sends
            what it is told outside the house.
          </div>
        </div>
        <div className="row">
          <button className="btn btn-primary" onClick={() => setCreating(true)}>
            <Plus size={15} /> New Butler
          </button>
        </div>
      </div>

      <ErrorLine error={butlers.error} />
      <ErrorLine error={models.error} />

      {attention.length > 0 && filter !== "attention" && (
        <AttentionBanner rows={attention} routes={modelRows} onShow={() => setFilter("attention")} />
      )}

      <RosterFilters
        rows={rows}
        filter={filter}
        onFilter={setFilter}
        query={query}
        onQuery={setQuery}
        sort={sort}
        onSort={setSort}
      />

      {groups.length > 0 && (
        <div className="roster-cols" aria-hidden>
          <div>BUTLER</div>
          <div>MODEL · WHERE ITS WORDS GO</div>
          <div>WORKING?</div>
          <div>REACH</div>
          <div>LAST TOUCHED</div>
          <div />
        </div>
      )}

      {groups.map((g) => (
        <section key={g.key} className="roster-group">
          <div className="row roster-grouphead">
            <h4>
              {g.title} · {g.rows.length}
            </h4>
            <div className="muted">{g.blurb}</div>
          </div>
          {g.rows.map((r) => (
            <RosterRow
              key={r.butler.name}
              row={r}
              model={modelRows.find((x) => x.name === r.butler.model)}
              selected={open === r.butler.name}
              onOpen={() => {
                setOpen(r.butler.name);
                setOpenTab(undefined);
              }}
            />
          ))}
        </section>
      ))}

      {groups.length === 0 && !butlers.loading && rows.length > 0 && (
        <NoMatches
          filter={filter}
          onClear={() => {
            setFilter("all");
            setQuery("");
          }}
        />
      )}

      {rows.length === 0 && !butlers.loading && (
        <NoButlers routes={modelRows} onNew={() => setCreating(true)} />
      )}

      {open && (
        <ButlerDrawer
          name={open}
          rows={rows}
          routes={modelRows}
          initialTab={openTab}
          say={say}
          onClose={() => setOpen(null)}
          onChanged={butlers.reload}
          onOpenOther={(name) => setOpen(name)}
        />
      )}

      {creating && (
        <NewButlerDialog
          routes={modelRows}
          existing={rows.map((r) => r.butler.name)}
          say={say}
          onClose={() => setCreating(false)}
          onCreated={(name, tab) => {
            butlers.reload();
            setCreating(false);
            setOpen(name);
            setOpenTab(tab);
          }}
        />
      )}

      <Toast toast={toast} />
    </>
  );
}

// The banner names the Butlers and what is wrong with each, rather than
// counting them.
//
// "2 Butlers need attention" is a number somebody then has to go and
// resolve into two names; "nutrition can't reach its model, ops answers
// questions only" is the same sentence already doing the work. It is
// also why the banner hides itself once the filter it offers is on —
// having taken the offer, the reader is looking at the answer.
export function AttentionBanner({
  rows,
  routes,
  onShow,
}: {
  rows: ButlerRow[];
  routes: Model[];
  onShow: () => void;
}) {
  const what = rows
    .map((r) => {
      const h = healthOf(r, routes.find((x) => x.name === r.butler.model));
      return `${r.butler.name} ${h.kind === "bad" ? "can't reach its model" : "answers questions only"}`;
    })
    .join(", ");
  return (
    <div className="attention-banner">
      <Dot kind="warn" />
      <div>
        {rows.length} {rows.length === 1 ? "Butler needs" : "Butlers need"} attention — {what}
      </div>
      <button className="btn btn-warn btn-small" onClick={onShow}>
        Show only these
      </button>
    </div>
  );
}

function RosterFilters({
  rows,
  filter,
  onFilter,
  query,
  onQuery,
  sort,
  onSort,
}: {
  rows: ButlerRow[];
  filter: Filter;
  onFilter: (f: Filter) => void;
  query: string;
  onQuery: (q: string) => void;
  sort: Sort;
  onSort: (s: Sort) => void;
}) {
  const live = rows.filter((r) => !r.butler.retiredAt);
  const counts: Record<Filter, number> = {
    all: live.length,
    family: live.filter((r) => r.butler.surface === "family").length,
    admin: live.filter((r) => r.butler.surface === "admin").length,
    cloud: live.filter((r) => r.leavesTheHouse).length,
    attention: live.filter(needsAttention).length,
    retired: rows.filter((r) => r.butler.retiredAt).length,
  };
  const chips: { key: Filter; label: string }[] = [
    { key: "all", label: "All" },
    { key: "family", label: "Household" },
    { key: "admin", label: "Operator" },
    { key: "cloud", label: "Leaves the house" },
    { key: "attention", label: "Needs attention" },
    { key: "retired", label: "Retired" },
  ];
  return (
    <div className="roster-filters">
      {chips.map((c) => (
        <button
          key={c.key}
          type="button"
          className={filter === c.key ? "chip on" : "chip"}
          aria-pressed={filter === c.key}
          onClick={() => onFilter(c.key)}
        >
          {c.label} <span className="chip-count">{counts[c.key]}</span>
        </button>
      ))}
      <div className="roster-search">
        <input
          type="search"
          aria-label="Search Butlers"
          placeholder="Search name or directive"
          value={query}
          onChange={(e) => onQuery(e.target.value)}
        />
        <select aria-label="Sort" value={sort} onChange={(e) => onSort(e.target.value as Sort)}>
          <option value="active">Sort: last touched</option>
          <option value="name">Sort: name</option>
          <option value="health">Sort: needs attention</option>
        </select>
      </div>
    </div>
  );
}

// One Butler. The whole row is the target — a real button, so it is
// reachable by keyboard and announced as a control, which a clickable
// div is not.
export function RosterRow({
  row,
  model,
  selected,
  onOpen,
}: {
  row: ButlerRow;
  model?: Model;
  selected: boolean;
  onOpen: () => void;
}) {
  const b = row.butler;
  const h = healthOf(row, model);
  const classes = ["rowcard"];
  if (selected) classes.push("on");
  else if (needsAttention(row)) classes.push("attn");
  if (b.retiredAt) classes.push("retired");
  return (
    <button type="button" className={classes.join(" ")} onClick={onOpen}>
      <div className="cell">
        <div className="rowcard-name">
          <strong>{b.name}</strong>
          {b.retiredAt && <span className="st st-mute">retired</span>}
          {!b.retiredAt && row.people === 0 && (
            <span className="st st-mute">nobody can talk to it</span>
          )}
        </div>
        <div className="muted ellip">{b.directive}</div>
      </div>
      <div className="cell">
        <div className="ellip">
          {modelLabel(b)} · {b.provider ?? "?"}
        </div>
        <StatusPill kind={row.leavesTheHouse ? "warn" : "ok"} icon={<Dot kind={row.leavesTheHouse ? "warn" : "ok"} />}>
          {row.leavesTheHouse ? "leaves the house" : "stays in the house"}
        </StatusPill>
      </div>
      <div className="cell">
        <div className="rowcard-health">
          <Dot kind={h.kind} />
          <span className={h.kind === "ok" ? "" : `ink-${h.kind}`}>{h.label}</span>
        </div>
        <div className="muted ellip">{h.detail}</div>
      </div>
      <div className="cell muted">
        <div className="ellip">{reachLine(row)}</div>
        <div className="ellip">{toolLine(row)}</div>
      </div>
      <div className="muted tnum ellip">{when(b.updatedAt || b.createdAt)}</div>
      <CaretRight size={14} />
    </button>
  );
}

export function NoMatches({ filter, onClear }: { filter: Filter; onClear: () => void }) {
  return (
    <div className="card roster-empty">
      <div className="roster-empty-title">Nothing matches that.</div>
      <div className="muted">
        {filter === "retired"
          ? "No Butler has been retired."
          : "Try a different filter, or clear the search."}
      </div>
      <button className="btn btn-secondary" onClick={onClear}>
        Clear filters
      </button>
    </div>
  );
}

// The first-run state. It explains the unit before offering the button,
// because "New Butler" means nothing to somebody who has not yet been
// told that a Butler is one job with one model behind it.
export function NoButlers({ routes, onNew }: { routes: Model[]; onNew: () => void }) {
  const usable = routes.filter((r) => r.enabled);
  return (
    <div className="card roster-empty">
      <div className="roster-empty-title">Nobody answers yet.</div>
      <div className="muted">
        Make one Butler per job — a health Butler, a kitchen Butler — and give each the model its
        material should go to. Keeping them separate is what makes "which model saw this"
        answerable afterwards.
      </div>
      {usable.length === 0 && (
        <div className="attention-banner">
          <Dot kind="warn" />
          <div>
            A Butler needs a model to be anything. Add one first — there is nowhere for a
            Butler's questions to go.
          </div>
        </div>
      )}
      <button className="btn btn-primary" disabled={usable.length === 0} onClick={onNew}>
        <Plus size={15} /> New Butler
      </button>
    </div>
  );
}
