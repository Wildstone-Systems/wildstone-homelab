import { useState } from "react";
import { CaretDown, Sparkle } from "@phosphor-icons/react";
import { modelLabel, type ButlerRow, type ButlerSurface } from "../../api";
import { Dot, StatusPill } from "../../ui/status";

export const SURFACE_LABEL: Record<ButlerSurface, string> = {
  admin: "Operator",
  family: "Household",
};

// What a Butler's opening screen says, derived from its own record.
//
// The old screen had two of these, one per surface, which was right
// while a surface had one Butler and became a lie the moment it had
// three: every household Butler greeted you as though it were the only
// one, in words about the surface rather than about itself.
//
// It is derived rather than a table keyed by name. A table would cover
// the Butlers somebody thought of and greet the fourth one — the one
// this household actually made — with a blank. The directive is what
// its owner already wrote about it, and the ceiling sentence is a fact
// about its surface, so both are always available.
export function openingFor(row: ButlerRow): {
  title: string;
  blurb: string;
  placeholder: string;
} {
  const b = row.butler;
  const ceiling =
    b.surface === "admin"
      ? "It can propose changes to the home lab — every proposal lands on Plans and waits for your approval."
      : "It reads the home and cannot touch the home lab.";
  // Where its words go comes FIRST when they leave the house. Somebody
  // deciding what to type is owed that before they are told what the
  // Butler is for.
  const destination = row.leavesTheHouse
    ? `Runs on ${modelLabel(b)}, outside the house: anything you tell it goes to ${b.provider ?? "the provider"}.`
    : `Runs on ${modelLabel(b)}, on your own hardware.`;
  const directive = (b.directive ?? "").trim();
  return {
    title: `What should ${b.name} look into?`,
    blurb: [directive ? `${directive}.` : "", destination, ceiling].filter(Boolean).join(" "),
    placeholder: `Ask ${b.name}…`,
  };
}

// Which Butler the screen opens on.
//
// A ready one, operator preferred, and never silently a broken one: the
// screen's first impression should not be a Butler that cannot answer,
// because a person reads that as the screen being broken rather than
// that Butler. If every Butler is unwell the first is shown anyway,
// with its blocked strip, which says which of the two it is.
export function preferred(rows: ButlerRow[]): ButlerRow | undefined {
  const live = rows.filter((r) => !r.butler.retiredAt);
  return (
    live.find((r) => r.reachable && r.butler.surface === "admin") ??
    live.find((r) => r.reachable) ??
    live[0]
  );
}

export function matches(row: ButlerRow, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return `${row.butler.name} ${row.butler.directive ?? ""}`.toLowerCase().includes(q);
}

// The selector that replaced the surface toggle.
//
// Surface survives as GROUPING rather than as a choice, because it is
// still what sets the ceiling — but it is not a correspondent, and a
// control that let you send a message to "Household" was asking which
// of three Butlers you meant and not waiting for the answer.
export function ButlerPicker({
  rows,
  current,
  onPick,
  onManage,
}: {
  rows: ButlerRow[];
  current?: ButlerRow;
  onPick: (name: string) => void;
  onManage: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");

  const live = rows.filter((r) => !r.butler.retiredAt);
  const retired = rows.length - live.length;
  const shown = live.filter((r) => matches(r, query));
  const groups = (["family", "admin"] as ButlerSurface[])
    .map((key) => ({
      key,
      label: SURFACE_LABEL[key].toUpperCase(),
      rows: shown.filter((r) => r.butler.surface === key),
    }))
    .filter((g) => g.rows.length > 0);

  return (
    <div className="picker-wrap">
      <button
        type="button"
        className={open ? "picker on" : "picker"}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Mark size={26} />
        <span className="picker-text">
          {current ? (
            <>
              <span className="picker-name">{current.butler.name}</span>
              <span className="muted picker-model">{modelLabel(current.butler)}</span>
              <span className="picker-pills">
                <StatusPill
                  kind={current.leavesTheHouse ? "warn" : "ok"}
                  icon={<Dot kind={current.leavesTheHouse ? "warn" : "ok"} />}
                >
                  {current.leavesTheHouse ? "leaves the house" : "stays in the house"}
                </StatusPill>
                {!current.reachable && <StatusPill kind="bad">unavailable</StatusPill>}
                {current.reachable && !current.carriesActions && (
                  <StatusPill kind="warn">questions only</StatusPill>
                )}
              </span>
            </>
          ) : (
            <span className="picker-name muted">No Butler yet</span>
          )}
        </span>
        <CaretDown size={13} />
      </button>

      {open && (
        <div className="picker-menu" role="listbox" aria-label="Which Butler?">
          <input
            type="search"
            className="picker-filter"
            placeholder="Which Butler?"
            aria-label="Which Butler?"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {groups.map((g) => (
            <div key={g.key} className="picker-group">
              {/* A label, never a target: there is no way to send a
                  message to a surface, which is the ambiguity this
                  control exists to remove. */}
              <div className="eyebrow picker-grouphead">{g.label}</div>
              {g.rows.map((r) => (
                <button
                  key={r.butler.name}
                  type="button"
                  role="option"
                  aria-selected={r.butler.name === current?.butler.name}
                  className={`picker-row${r.butler.name === current?.butler.name ? " on" : ""}`}
                  onClick={() => {
                    onPick(r.butler.name);
                    setOpen(false);
                    setQuery("");
                  }}
                >
                  <Mark size={22} />
                  <span className="picker-text">
                    <span className="picker-name">{r.butler.name}</span>
                    <span className="muted picker-model">{modelLabel(r.butler)}</span>
                    <span className="picker-pills">
                      <StatusPill kind={r.leavesTheHouse ? "warn" : "ok"}>
                        {r.leavesTheHouse ? "leaves the house" : "stays in the house"}
                      </StatusPill>
                    </span>
                  </span>
                  <Dot kind={!r.reachable ? "bad" : !r.carriesActions ? "warn" : "ok"} />
                </button>
              ))}
            </div>
          ))}
          {shown.length === 0 && <div className="muted picker-empty">No Butler by that name.</div>}
          <div className="picker-foot">
            {/* Retired Butlers are never listed: one answers nothing, so
                offering it would start a conversation that cannot. */}
            {retired > 0 && (
              <span className="muted">
                {retired} retired {retired === 1 ? "Butler is" : "Butlers are"} not listed
              </span>
            )}
            <button className="btn btn-ghost btn-small" onClick={onManage}>
              Manage Butlers →
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// The same mark a Butler's turns use, so the thing you picked and the
// thing answering are visibly one.
function Mark({ size }: { size: number }) {
  return (
    <span className="picker-mark" style={{ width: size, height: size }} aria-hidden>
      <Sparkle size={size * 0.55} weight="fill" />
    </span>
  );
}
