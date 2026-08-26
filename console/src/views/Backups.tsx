import { useState } from "react";
import { api } from "../api";
import { Badge, ErrorLine, useLoad, when } from "../lib";

// Backups (ADR-W016 A2): the run ledger, plus on-demand run/restore.
// The API's approve-verb gate already refuses the Butler.
export function Backups() {
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const ledger = useLoad(() => api.backups());
  if (ledger.error) return <ErrorLine error={ledger.error} />;
  const runs = ledger.data?.runs ?? [];
  const plans = [...new Set(runs.map((r) => r.planName))].sort();

  const act = (fn: (plan: string) => Promise<unknown>, plan: string) => {
    setBusy(true);
    setActionError(null);
    fn(plan)
      .then(() => ledger.reload())
      .catch((e: Error) => setActionError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <h2>Backups</h2>
      <div className="sub">
        Scheduled runs are the engine's standing duty; buttons run or restore-alongside now.
      </div>
      {plans.map((plan) => (
        <div className="row" key={plan}>
          <strong style={{ fontSize: 13 }}>{plan}</strong>
          <button className="action" disabled={busy} onClick={() => act(api.runBackup, plan)}>
            Run now
          </button>
          <button className="action" disabled={busy} onClick={() => act(api.restoreBackup, plan)}>
            Restore alongside
          </button>
        </div>
      ))}
      <ErrorLine error={actionError} />
      <table>
        <thead>
          <tr>
            <th>plan</th>
            <th>kind</th>
            <th>status</th>
            <th>started</th>
            <th>finished</th>
            <th>detail</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.seq}>
              <td>{r.planName}</td>
              <td>{r.runKind}</td>
              <td>
                <Badge value={r.status} />
              </td>
              <td>{when(r.startedAt)}</td>
              <td>{when(r.finishedAt)}</td>
              <td>{r.detail ?? ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {runs.length === 0 && (
        <div className="muted">No runs recorded yet — the engine ticks on its own schedule.</div>
      )}
    </>
  );
}
