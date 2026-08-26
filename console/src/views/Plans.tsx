import { useState } from "react";
import { api } from "../api";
import { Badge, ErrorLine, useLoad, when } from "../lib";

// Plans (ADR-W016 A2): the signature interaction. The review pane shows
// everything approval needs — summary, risk, who proposed for whom, and
// the stored plan JSON (steps included) — with Approve/Reject.
export function Plans() {
  const [selected, setSelected] = useState<string | null>(null);
  const list = useLoad(() => api.plans());
  if (list.error) return <ErrorLine error={list.error} />;
  const plans = list.data ?? [];

  return (
    <>
      <h2>Plans</h2>
      <div className="sub">
        Every intentional change is a plan; Medium/High wait here for a human.
      </div>
      <table>
        <thead>
          <tr>
            <th>id</th>
            <th>state</th>
            <th>risk</th>
            <th>summary</th>
            <th>proposed by</th>
            <th>created</th>
          </tr>
        </thead>
        <tbody>
          {plans.map((p) => (
            <tr className="clickable" key={p.id} onClick={() => setSelected(p.id)}>
              <td>{p.id}</td>
              <td>
                <Badge value={p.state} />
              </td>
              <td>
                <Badge value={p.risk} />
              </td>
              <td>{p.summary}</td>
              <td>
                {p.principal}
                {p.actingFor ? ` (for ${p.actingFor})` : ""}
              </td>
              <td>{when(p.createdAt)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {selected && <PlanReview id={selected} onDone={list.reload} />}
    </>
  );
}

function PlanReview({ id, onDone }: { id: string; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const plan = useLoad(() => api.plan(id), [id]);
  if (plan.error) return <ErrorLine error={plan.error} />;
  const p = plan.data;
  if (!p) return null;

  const act = (fn: (id: string) => Promise<unknown>) => {
    setBusy(true);
    setActionError(null);
    fn(id)
      .then(() => {
        plan.reload();
        onDone();
      })
      .catch((e: Error) => setActionError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <div className="card" style={{ marginTop: 16 }}>
      <h3>
        {p.id} — {p.summary}
      </h3>
      <div className="row">
        <Badge value={p.state} />
        <Badge value={p.risk} />
        <span className="muted">
          proposed by {p.principal}
          {p.actingFor ? ` acting for ${p.actingFor}` : ""} at {when(p.createdAt)}
          {p.resolvedBy ? `; resolved by ${p.resolvedBy}` : ""}
        </span>
      </div>
      <PlanSteps planJson={p.planJson} />
      {p.state === "proposed" && (
        <div className="row">
          <button className="action primary" disabled={busy} onClick={() => act(api.approvePlan)}>
            Approve
          </button>
          <button className="action danger" disabled={busy} onClick={() => act(api.rejectPlan)}>
            Reject
          </button>
        </div>
      )}
      <ErrorLine error={actionError} />
    </div>
  );
}

interface StoredStep {
  Description?: string;
  Kind?: string;
  Name?: string;
  Risk?: number;
  BlastRadius?: string;
}

// PlanSteps renders the stored plan's step list when it parses, and the
// raw JSON otherwise — review must never show less than what approval
// commits to.
function PlanSteps({ planJson }: { planJson?: string }) {
  if (!planJson) return null;
  let steps: StoredStep[] | null = null;
  try {
    const parsed = JSON.parse(planJson) as { plan?: { Steps?: StoredStep[] } };
    steps = parsed.plan?.Steps ?? null;
  } catch {
    steps = null;
  }
  if (!steps || steps.length === 0) {
    return <pre>{planJson}</pre>;
  }
  return (
    <table style={{ margin: "8px 0" }}>
      <thead>
        <tr>
          <th>step</th>
          <th>target</th>
          <th>blast radius</th>
        </tr>
      </thead>
      <tbody>
        {steps.map((s, i) => (
          <tr key={i}>
            <td>{s.Description ?? ""}</td>
            <td>
              {s.Kind ?? ""}/{s.Name ?? ""}
            </td>
            <td>{s.BlastRadius ?? ""}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
