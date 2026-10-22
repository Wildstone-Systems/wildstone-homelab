import { useEffect, useState } from "react";
import { api, ApiError, MorningReport as Report } from "../api";
import { ago, ErrorLine, when } from "../lib";
import { Kind, kindOf, StatusPill } from "../ui/status";

// Morning report (ADR-W017 A5): the deterministic daily summary, read
// from GET /api/v1/reports/morning. It is built per request from
// runtime-DB reads with no model involved, so what this screen shows
// does not depend on the Butler's mode; it only reads, and approving a
// pending plan stays on Plans.
export type ReportState =
  | { phase: "loading" }
  | { phase: "loaded"; report: Report }
  | { phase: "unavailable" } // 503: the report (or Butler provider) is off
  | { phase: "denied" } // 403: this role may not read it
  | { phase: "error"; message: string };

export function stateOf(err: unknown): ReportState {
  if (err instanceof ApiError && err.status === 503) return { phase: "unavailable" };
  if (err instanceof ApiError && err.status === 403) return { phase: "denied" };
  return { phase: "error", message: err instanceof Error ? err.message : String(err) };
}

export function MorningReport({ onNavigate }: { onNavigate?: (view: "plans" | "backups") => void }) {
  const [state, setState] = useState<ReportState>({ phase: "loading" });
  const load = () => {
    setState({ phase: "loading" });
    api
      .morningReport()
      .then((report) => setState({ phase: "loaded", report }))
      .catch((e) => setState(stateOf(e)));
  };
  useEffect(load, []);
  return <MorningReportView state={state} onRefresh={load} onNavigate={onNavigate} />;
}

export function MorningReportView({
  state,
  onRefresh,
  onNavigate,
}: {
  state: ReportState;
  onRefresh?: () => void;
  onNavigate?: (view: "plans" | "backups") => void;
}) {
  switch (state.phase) {
    case "loading":
      return <div className="muted">Loading the morning report…</div>;
    case "unavailable":
      return (
        <div className="muted">
          The morning report is not available right now. The Butler provider is off or the service
          is not ready; it will appear here once it is.
        </div>
      );
    case "denied":
      return (
        <div className="muted">
          Your role is not allowed to read the morning report. Ask an admin if you need it.
        </div>
      );
    case "error":
      return <ErrorLine error={state.message} />;
  }
  const r = state.report;
  const unhealthy = r.unhealthy ?? [];
  const pending = r.pendingPlans ?? [];
  const backups = r.backups ?? [];
  const alerts = r.backupAlerts ?? [];
  const counts = Object.entries(r.eventCounts24h ?? {}).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const quiet = unhealthy.length === 0 && pending.length === 0 && r.backupsOk && r.eventTotal24h === 0;
  return (
    <>
      <div className="sub">
        Generated <span title={when(r.generatedAt)}>{ago(r.generatedAt)}</span>
        {onRefresh && (
          <>
            {" · "}
            <button className="link" onClick={onRefresh}>Refresh</button>
          </>
        )}
      </div>
      {r.summary && <p>{r.summary}</p>}
      {quiet && <div className="muted">Nothing needs attention.</div>}

      <section style={{ marginTop: "var(--space-5)" }}>
        <div className="eyebrow">Unhealthy</div>
        {unhealthy.length === 0 ? (
          <div className="muted">Every observed resource is healthy.</div>
        ) : (
          <ul>
            {unhealthy.map((o) => (
              <li key={`${o.kind}/${o.name}`}>
                <StatusPill kind="bad">unhealthy</StatusPill> {o.kind}/{o.name}
                {o.node ? ` on ${o.node}` : ""}
                {o.detail ? ` — ${o.detail}` : ""}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section style={{ marginTop: "var(--space-5)" }}>
        <div className="eyebrow">Waiting for your approval</div>
        {pending.length === 0 ? (
          <div className="muted">No plans are waiting.</div>
        ) : (
          <ul>
            {pending.map((p) => (
              <li key={p.id}>
                <button className="link" onClick={() => onNavigate?.("plans")} title="Open Plans">
                  {p.id}
                </button>{" "}
                <StatusPill kind={kindOf(p.risk)}>{p.risk}</StatusPill> {p.summary}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section style={{ marginTop: "var(--space-5)" }}>
        <div className="eyebrow">Backups</div>
        {backups.length === 0 && alerts.length === 0 ? (
          <div className="muted">No backup runs have been recorded.</div>
        ) : (
          <>
            <StatusPill kind={r.backupsOk ? "ok" : "bad"}>
              {r.backupsOk ? "all backups ok" : "backups need attention"}
            </StatusPill>
            <ul>
              {backups.map((b) => {
                const k: Kind = kindOf(b.status);
                return (
                  <li key={b.planName}>
                    {b.planName} <StatusPill kind={k}>{b.status}</StatusPill>{" "}
                    <span title={when(b.at)}>{ago(b.at)}</span>
                    {b.detail ? ` — ${b.detail}` : ""}
                  </li>
                );
              })}
              {alerts.map((a, i) => (
                <li key={`${a.planName}/${a.kind}/${i}`}>
                  {a.planName} <StatusPill kind="bad">{a.kind}</StatusPill> {a.detail}
                </li>
              ))}
            </ul>
          </>
        )}
        {onNavigate && (
          <button className="link" onClick={() => onNavigate("backups")}>
            Open Backups
          </button>
        )}
      </section>

      <section style={{ marginTop: "var(--space-5)" }}>
        <div className="eyebrow">Last 24 hours</div>
        {counts.length === 0 ? (
          <div className="muted">No events in the last 24 hours.</div>
        ) : (
          <>
            <div>{r.eventTotal24h} events</div>
            <ul>
              {counts.map(([type, n]) => (
                <li key={type}>
                  {type}: {n}
                </li>
              ))}
            </ul>
          </>
        )}
      </section>
    </>
  );
}
