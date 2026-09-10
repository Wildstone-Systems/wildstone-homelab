import { useState } from "react";
import { Archive, ArrowClockwise, Warning, X } from "@phosphor-icons/react";
import { api, type ButlerRow, type ModelRoute } from "../../api";
import { ErrorLine, useLoad, when } from "../../lib";
import { Confirm } from "../../ui/Confirm";
import { Dot, healthOf, Kind, StatusPill } from "../../ui/status";
import { Tabs } from "../../ui/Tabs";
import { TabIdentity } from "./TabIdentity";
import { TabMemory } from "./TabMemory";
import { TabModel } from "./TabModel";
import { TabReach } from "./TabReach";
import { TabSessions } from "./TabSessions";
import { TabTools } from "./TabTools";

export type Tab = "identity" | "model" | "reach" | "tools" | "memory" | "sessions";
export type Say = (text: string, kind?: Kind) => void;

// Everything about one Butler, without leaving the roster.
//
// A drawer rather than a screen because the roster is the thing being
// compared — "which of these leaves the house" is a question about the
// list, and answering it by navigating away and back loses both the
// scroll position and the comparison.
//
// It opens on Model, not Identity. Identity is the tab a person visits
// once; the model is the one they came to check.
export function ButlerDrawer({
  name,
  rows,
  routes,
  initialTab,
  say,
  onClose,
  onChanged,
  onOpenOther,
}: {
  name: string;
  rows: ButlerRow[];
  routes: ModelRoute[];
  initialTab?: string;
  say: Say;
  onClose: () => void;
  onChanged: () => void;
  onOpenOther: (name: string) => void;
}) {
  const [tab, setTab] = useState<Tab>((initialTab as Tab) ?? "model");
  const [confirm, setConfirm] = useState<{ kind: "retire" } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pointRoutines, setPointRoutines] = useState(false);

  const row = rows.find((r) => r.butler.name === name);
  const scope = useLoad(() => api.butlerScope(name), [name]);
  const tools = useLoad(() => api.butlerTools(name), [name]);
  const impact = useLoad(() => api.butlerImpact(name), [name]);

  if (!row) return null;
  const b = row.butler;
  const route = routes.find((r) => r.name === b.route);
  const h = healthOf(row, route);
  const memoryCount = scope.data?.scope.memoryCount ?? 0;

  function reloadAll() {
    scope.reload();
    tools.reload();
    impact.reload();
    onChanged();
  }

  async function run(fn: () => Promise<unknown>, done: () => void) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      done();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const retired = !!b.retiredAt;

  return (
    <>
      <div className="drawer-scrim" onClick={onClose}>
        <aside
          className="drawer drawer-wide butler-drawer"
          role="dialog"
          aria-modal="true"
          aria-label={`Butler ${b.name}`}
          onClick={(e) => e.stopPropagation()}
        >
          <header className="butler-drawer-head">
            <div className="row">
              <h3>{b.name}</h3>
              <span className="st st-mute">{b.surface === "admin" ? "Operator" : "Household"}</span>
              <StatusPill
                kind={row.leavesTheHouse ? "warn" : "ok"}
                icon={<Dot kind={row.leavesTheHouse ? "warn" : "ok"} />}
              >
                {row.leavesTheHouse ? "leaves the house" : "stays in the house"}
              </StatusPill>
              <button
                type="button"
                className="btn btn-ghost drawer-close"
                onClick={onClose}
                aria-label="Close"
              >
                <X size={16} />
              </button>
            </div>
            <div className="muted tnum">
              made by {b.createdBy} · {when(b.createdAt)}
              {retired && ` · retired ${when(b.retiredAt)}`}
            </div>
          </header>

          <Tabs
            label={`${b.name} detail`}
            current={tab}
            onSelect={setTab}
            items={[
              { id: "identity", label: "Identity" },
              { id: "model", label: "Model" },
              { id: "reach", label: "Reach" },
              { id: "tools", label: "Tools", count: `${row.toolsAllowed}/${row.toolsAvailable}` },
              { id: "memory", label: "Memory", count: memoryCount },
              { id: "sessions", label: "Sessions" },
            ]}
          />

          <div className="drawer-body" role="tabpanel" id={`tabpanel-${tab}`} aria-labelledby={`tab-${tab}`}>
            <ErrorLine error={error} />
            <ErrorLine error={scope.error} />
            {tab === "identity" && <TabIdentity row={row} say={say} onChanged={onChanged} />}
            {tab === "model" && (
              <TabModel row={row} routes={routes} health={h} say={say} onChanged={onChanged} />
            )}
            {tab === "reach" && (
              <TabReach row={row} scope={scope.data} say={say} onChanged={reloadAll} />
            )}
            {tab === "tools" && (
              <TabTools row={row} tools={tools.data} error={tools.error} say={say} onChanged={reloadAll} />
            )}
            {tab === "memory" && <TabMemory row={row} say={say} />}
            {tab === "sessions" && <TabSessions row={row} />}
          </div>

          <footer className="butler-drawer-foot">
            <button
              type="button"
              className="btn btn-secondary"
              disabled={busy}
              onClick={() =>
                run(
                  async () => {
                    const copy = await api.duplicateButler(b.name);
                    onOpenOther(copy.butler.name);
                    say(`${copy.butler.name} made — nobody can talk to it yet`, "warn");
                  },
                  reloadAll,
                )
              }
            >
              Duplicate
            </button>
            {retired ? (
              <button
                type="button"
                className="btn btn-ok drawer-foot-end"
                disabled={busy}
                onClick={() =>
                  run(
                    async () => {
                      await api.restoreButler(b.name);
                      say(`${b.name} is answering again`);
                    },
                    reloadAll,
                  )
                }
              >
                Bring back
              </button>
            ) : (
              <button
                type="button"
                className="btn btn-danger drawer-foot-end"
                disabled={busy}
                onClick={() => setConfirm({ kind: "retire" })}
              >
                Retire
              </button>
            )}
          </footer>
        </aside>
      </div>

      {confirm?.kind === "retire" && (
        <Confirm
          kind="bad"
          icon={<Archive size={15} />}
          title={`Retire ${b.name}?`}
          body="It stops answering immediately. Nothing is deleted — you can bring it back from the Retired filter."
          listTitle="WHAT BREAKS"
          lines={retireLines(impact.data)}
          option={
            (impact.data?.routines?.length ?? 0) > 0 && (impact.data?.successors?.length ?? 0) > 0
              ? {
                  label: `Point those routines at ${impact.data?.successors?.[0]} instead`,
                  checked: pointRoutines,
                  onToggle: () => setPointRoutines((v) => !v),
                }
              : undefined
          }
          confirmLabel="Retire it"
          cancelLabel="Keep it"
          busy={busy}
          onCancel={() => setConfirm(null)}
          onConfirm={() =>
            run(
              async () => {
                await api.retireButler(b.name);
                say(`${b.name} retired. Nothing was deleted.`, "warn");
              },
              () => {
                setConfirm(null);
                reloadAll();
              },
            )
          }
        />
      )}
    </>
  );
}

// WHAT BREAKS, counted rather than guessed, with every zero omitted.
//
// A line that reads "0 routines call it" is noise standing where a real
// consequence would be, and a list padded with zeroes teaches a reader
// to skim the one line that mattered.
export function retireLines(impact?: {
  people: string[] | null;
  routines: { name: string; state: string }[] | null;
  sessions: number;
  memoryCount: number;
} | null): string[] {
  if (!impact) return [];
  const lines: string[] = [];
  const people = impact.people?.length ?? 0;
  if (people > 0) {
    lines.push(`${people} ${people === 1 ? "person loses" : "people lose"} the Butler they message`);
  }
  // Only routines that would actually stop working. A routine somebody
  // already disabled is not something retiring this Butler breaks.
  const live = (impact.routines ?? []).filter((r) => r.state !== "disabled");
  if (live.length > 0) {
    const named = live.slice(0, 2).map((r) => `“${r.name}”`).join(" and ");
    lines.push(
      `${live.length} ${live.length === 1 ? "routine calls" : "routines call"} it — ${named} will fail`,
    );
  }
  if (impact.sessions > 0) {
    lines.push(
      `${impact.sessions} ${impact.sessions === 1 ? "session stays" : "sessions stay"} readable in the audit trail`,
    );
  }
  if (impact.memoryCount > 0) {
    lines.push(
      `${impact.memoryCount} remembered ${impact.memoryCount === 1 ? "note is" : "notes are"} kept, not deleted`,
    );
  }
  return lines;
}

export { ArrowClockwise, Warning };
