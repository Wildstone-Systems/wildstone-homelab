import { Archive, ArrowCounterClockwise, Play } from "@phosphor-icons/react";
import { api, BackupRun } from "../api";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { Kind, kindOf, StatusPill } from "../ui/status";
import { FillBar } from "../ui/Metric";
import { ProblemCard } from "../ui/ProblemCard";
import { Sparkline } from "../ui/Sparkline";

// Backups (ADR-W016 A2, ADR-W027 A7): the standing duty, per plan.
//
// One card per backup plan rather than one row per run, because the
// question is "is this plan healthy" and the ledger only answered "what
// happened at 03:00". Run now moves the card to running and then to its
// result without a reload, and a failure explains itself rather than
// printing the engine's sentence and leaving.
export function Backups() {
  const ledger = useLoad(() => api.backups(), []);
  if (ledger.error) return <ErrorLine error={ledger.error} />;
  return <BackupsView runs={ledger.data?.runs ?? []} onRan={ledger.reload} loading={ledger.loading} />;
}

export interface PlanLedger {
  name: string;
  runs: BackupRun[];
  last?: BackupRun;
  health: Kind;
}

// Newest run first within each plan; plans whose last run failed first
// overall. A ledger sorted by time answers "what happened"; this one
// answers "what needs me".
export function byPlan(runs: BackupRun[]): PlanLedger[] {
  const RANK: Record<Kind, number> = { bad: 0, warn: 1, mute: 2, ok: 3 };
  const map = new Map<string, BackupRun[]>();
  for (const r of runs) map.set(r.planName, [...(map.get(r.planName) ?? []), r]);
  return [...map.entries()]
    .map(([name, list]) => {
      const sorted = [...list].sort((a, b) => b.seq - a.seq);
      const last = sorted[0];
      return { name, runs: sorted, last, health: kindOf(last?.status) };
    })
    .sort((a, b) => RANK[a.health] - RANK[b.health] || a.name.localeCompare(b.name));
}

export function BackupsView({
  runs,
  onRan,
  loading,
}: {
  runs: BackupRun[];
  onRan: () => void;
  loading?: boolean;
}) {
  const plans = byPlan(runs);
  return (
    <>
      <div className="sub">
        Scheduled runs are the engine's standing duty. The buttons here run one now, or restore
        alongside the live data rather than over it.
      </div>
      {plans.length === 0 && !loading && (
        <div className="muted">
          No runs recorded yet — the engine ticks on its own schedule, so a plan appears here
          after its first run rather than when it is written.
        </div>
      )}
      <div className="bands">
        {plans.map((p) => (
          <BackupPlanCard key={p.name} ledger={p} onRan={onRan} />
        ))}
      </div>
    </>
  );
}

export function BackupPlanCard({ ledger, onRan }: { ledger: PlanLedger; onRan: () => void }) {
  const run = useAction();
  const restore = useAction();
  const busy = run.state.phase === "busy" || restore.state.phase === "busy";
  const ranNow = run.state.phase === "done" ? run.state : null;
  const restored = restore.state.phase === "done" ? restore.state : null;

  const last = ledger.last;
  // The engine reports a run's status and its timestamps, and nothing
  // about how far through it is. An indeterminate bar says "working"
  // without claiming a percentage nobody has (ADR-W027 A9).
  const running = last?.status === "running" || run.state.phase === "busy";
  const health: Kind = ranNow ? "ok" : running ? "warn" : ledger.health;

  return (
    <ProblemCard
      severity={health}
      icon={<Archive size={16} weight="fill" color={`var(--st-${health === "mute" ? "warn" : health})`} />}
      title={ledger.name}
      meta={
        <>
          <StatusPill kind={health}>
            {ranNow ? ranNow.message : running ? "running" : (last?.status ?? "never run")}
          </StatusPill>
          {last && (
            <span className="muted" title={when(last.startedAt)}>
              {last.runKind} · started {ago(last.startedAt)}
            </span>
          )}
        </>
      }
      body={last ? explain(last, ledger) : "This plan has no recorded runs yet."}
      actions={
        <>
          <button
            className="btn btn-primary"
            disabled={busy}
            onClick={() => run.run(() => api.runBackup(ledger.name).then(onRan), "started")}
          >
            <Play size={14} />
            {run.state.phase === "busy" ? "Starting…" : "Run now"}
          </button>
          <button
            className="btn btn-secondary"
            disabled={busy}
            onClick={() =>
              restore.run(
                () => api.restoreBackup(ledger.name).then(onRan),
                "restore proposed — approve it on Plans",
              )
            }
          >
            <ArrowCounterClockwise size={14} />
            Restore alongside
          </button>
          {restored && <StatusPill kind="ok">{restored.message}</StatusPill>}
          {run.state.phase === "error" && <ErrorLine error={run.state.message} />}
          {restore.state.phase === "error" && <ErrorLine error={restore.state.message} />}
        </>
      }
      aside={
        <>
          <div className="eyebrow" style={{ marginBottom: "var(--space-2)" }}>
            {ledger.runs.length === 1 ? "Last run" : `Last ${Math.min(ledger.runs.length, 10)} runs`}
          </div>
          <Sparkline
            samples={ledger.runs.slice(0, 10).reverse().map((r) => kindOf(r.status))}
            height={22}
            min={1}
            label={`the last ${Math.min(ledger.runs.length, 10)} recorded runs, oldest first`}
          />
          {running && (
            <div style={{ marginTop: "var(--space-3)" }}>
              <FillBar kind="warn" caption="running — the engine reports no progress figure" />
            </div>
          )}
        </>
      }
    />
  );
}

// A failure that only repeats the engine's sentence leaves the operator
// where they started. Where the console knows what the sentence means,
// it says what to do about it.
export function explain(last: BackupRun, ledger: PlanLedger): string {
  const detail = last.detail ?? "";
  if (last.status === "failed" && /lock/i.test(detail)) {
    return `${detail} A stale lock is left by a run that was killed rather than finishing. Unlocking the repository is itself a change, so it goes through a plan — propose one and approve it, then run this again.`;
  }
  if (last.status === "failed") {
    return detail || "The engine recorded a failure with no detail. The Activity stream has the run's events.";
  }
  if (last.status === "running") {
    return `Started ${ago(last.startedAt)}${detail ? ` — ${detail}` : ""}.`;
  }
  const failures = ledger.runs.filter((r) => r.status === "failed").length;
  return [
    detail || `Finished ${ago(last.finishedAt ?? last.startedAt)}.`,
    failures > 0 ? `${failures} of the last ${ledger.runs.length} recorded runs failed.` : "",
  ]
    .filter(Boolean)
    .join(" ");
}
