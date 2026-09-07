import { ArrowCounterClockwise, SealCheck, WarningCircle, XCircle } from "@phosphor-icons/react";
import { api, type EventRow, type Plan, type PlanRecord } from "../api";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { useConsoleState } from "../state";
import { kindOf, StatusPill } from "../ui/status";
import { ProblemCard } from "../ui/ProblemCard";

// Plans (ADR-W016 A2, ADR-W027 A7): the signature interaction, and the
// one screen where a click changes the household.
//
// Everything approval needs is on the card — summary, risk, who
// proposed it for whom, and every step with its blast radius — so
// approving never means opening something else first. The result lands
// on the same card: who approved it and when, or why it failed. The old
// screen fired the request and left the page exactly as it was.
export function Plans() {
  const { snap } = useConsoleState();
  const list = useLoad(() => api.plans(), []);
  if (list.error) return <ErrorLine error={list.error} />;
  // The shared poll already has the plans; the fetch is what gives us a
  // reload after an approval without waiting for the next tick.
  const plans = list.data ?? snap?.plans ?? [];
  return <PlansView plans={plans} events={snap?.events ?? []} onResolved={list.reload} />;
}

// whyItFailed finds the reason a plan failed.
//
// The reconciler writes an excellent message when it refuses — it names
// the exact `wild secret set` commands, for instance — into a
// reconcile.failed event carrying the plan ids in its attrs. Nothing
// showed it. A plan that says only "failed" sends an operator to the
// Activity stream to correlate timestamps by hand, and the failure that
// prompted this change blocked every LATER plan too, so three unrelated
// plans failed with a reason none of them displayed.
export function whyItFailed(plan: PlanRecord, events: EventRow[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    const e = events[i];
    if (e.type !== "reconcile.failed") continue;
    const named = (e.attrs?.plans ?? "").split(",").map((s) => s.trim());
    if (named.includes(plan.id)) return e.message;
  }
  return undefined;
}

export function PlansView({
  plans,
  events = [],
  onResolved,
}: {
  plans: PlanRecord[];
  events?: EventRow[];
  onResolved: () => void;
}) {
  const pending = plans.filter((p) => p.state === "proposed");
  const resolved = plans.filter((p) => p.state !== "proposed");
  // A failure the console can still explain is worth raising out of the
  // history table: it is the reason the next thing you try will fail
  // too. One whose event has aged out of the kept window stays in the
  // table, because a card with nothing to say is just a louder row.
  const explained = resolved
    .map((p) => ({ plan: p, why: whyItFailed(p, events) }))
    .filter((x): x is { plan: PlanRecord; why: string } => x.why !== undefined);

  return (
    <>
      <div className="sub">
        Every intentional change is a plan; medium and high risk wait here for a human.
      </div>

      {pending.length === 0 ? (
        <div className="allwell">Nothing is waiting for you.</div>
      ) : (
        <div className="bands">
          {pending.map((p) => (
            <PendingPlan key={p.id} plan={p} onResolved={onResolved} />
          ))}
        </div>
      )}

      {explained.length > 0 && (
        <div className="bands" style={{ marginTop: "var(--space-8)" }}>
          {explained.map(({ plan, why }) => (
            <ProblemCard
              key={plan.id}
              severity="bad"
              icon={<WarningCircle size={16} weight="fill" color="var(--st-bad)" />}
              title={plan.summary}
              meta={
                <>
                  <StatusPill kind="bad">{plan.state}</StatusPill>
                  <span className="muted tnum" title={when(plan.resolvedAt ?? plan.createdAt)}>
                    {plan.id} · {ago(plan.resolvedAt ?? plan.createdAt)}
                  </span>
                </>
              }
            >
              {/* The reconciler's own words. It names what to do; the
                  console's job is to put them where the failure is,
                  not to paraphrase them. */}
              <pre className="failure">{why}</pre>
            </ProblemCard>
          ))}
        </div>
      )}

      {resolved.length > 0 && (
        <>
          <div className="sect" style={{ marginTop: "var(--space-8)" }}>
            <span className="eyebrow">Resolved</span>
            <span className="muted">{resolved.length}</span>
            <span className="fade" />
          </div>
          <table>
            <thead>
              <tr>
                <th>id</th>
                <th>state</th>
                <th>risk</th>
                <th>summary</th>
                <th>proposed by</th>
                <th>resolved</th>
              </tr>
            </thead>
            <tbody>
              {resolved.map((p) => (
                <tr key={p.id}>
                  <td className="tnum">{p.id}</td>
                  <td>
                    <StatusPill kind={kindOf(p.state)}>{p.state}</StatusPill>
                  </td>
                  <td>
                    <StatusPill kind={kindOf(p.risk)}>{p.risk}</StatusPill>
                  </td>
                  <td>{p.summary}</td>
                  <td>
                    {p.principal}
                    {p.actingFor ? ` (for ${p.actingFor})` : ""}
                  </td>
                  {/* A plan resolved before the API recorded who and
                      when has neither, and "never" would be a worse
                      answer than saying nothing. */}
                  <td title={when(p.resolvedAt)}>
                    {[p.resolvedBy, p.resolvedAt && ago(p.resolvedAt)].filter(Boolean).join(" · ") || "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </>
  );
}

// One pending plan, with its decision on it. Approve and Reject share a
// single useAction, so the two buttons, the pill and the explanation all
// read from one state and cannot disagree about what happened.
export function PendingPlan({ plan, onResolved }: { plan: PlanRecord; onResolved: () => void }) {
  const act = useAction();
  // Narrowed rather than boolean, so the message and the phase cannot
  // drift apart: if it is done, it has a result to show.
  const done = act.state.phase === "done" ? act.state : null;
  const busy = act.state.phase === "busy";

  const resolve = (verb: "approve" | "reject") => () =>
    act.run(
      () =>
        (verb === "approve" ? api.approvePlan(plan.id) : api.rejectPlan(plan.id)).then(onResolved),
      verb === "approve"
        ? "approved — handed to the reconciler"
        : "rejected — the Butler is told why on its next session",
    );

  const risky = kindOf(plan.risk) === "bad";
  const restores = (plan.plan?.steps ?? []).some((s) => (s.action ?? "").toLowerCase().includes("restore"));

  return (
    <ProblemCard
      severity={done ? "ok" : kindOf(plan.risk)}
      icon={<SealCheck size={16} weight="fill" color={done ? "var(--st-ok)" : "var(--st-warn)"} />}
      title={plan.summary}
      meta={
        <>
          <StatusPill kind={done ? "ok" : kindOf(plan.state)}>
            {done ? done.message : plan.state}
          </StatusPill>
          <StatusPill kind={kindOf(plan.risk)}>{plan.risk} risk</StatusPill>
          <span className="muted tnum" title={when(plan.createdAt)}>
            {plan.id} · {ago(plan.createdAt)}
          </span>
        </>
      }
      body={
        <>
          Proposed by {plan.principal}
          {plan.actingFor ? ` acting for ${plan.actingFor}` : ""}
          {plan.basedOnHead ? `, against statefile ${plan.basedOnHead.slice(0, 8)}` : ""}.{" "}
          {done
            ? "Nothing else is waiting on you for this one."
            : "Approving records who approved it and hands the plan to the reconciler."}
        </>
      }
      actions={
        <>
          {!done && (
            <>
              <button className="btn btn-primary" disabled={busy} onClick={resolve("approve")}>
                <SealCheck size={14} />
                {busy ? "Working…" : "Approve"}
              </button>
              <button className="btn btn-danger" disabled={busy} onClick={resolve("reject")}>
                <XCircle size={14} />
                Reject
              </button>
            </>
          )}
          {act.state.phase === "error" && <ErrorLine error={act.state.message} />}
        </>
      }
    >
      {risky && (
        <div className="callout callout-bad">
          <strong>High risk.</strong> Read the blast radius on every step below before approving.
          {restores && (
            <>
              {" "}
              A restore can be taken alongside the live data instead of over it — Backups offers
              that, and it is reversible where this is not.
            </>
          )}
        </div>
      )}
      <PlanSteps plan={plan.plan} />
    </ProblemCard>
  );
}

// The API serves the plan as a structured object, so there is no JSON to
// parse here and no raw dump to fall back to — a plan with no steps says
// so rather than showing an escape sequence.
export function PlanSteps({ plan }: { plan?: Plan }) {
  const steps = plan?.steps ?? [];
  if (steps.length === 0) {
    return (
      <div className="muted" style={{ marginTop: "var(--space-3)" }}>
        This plan carries no expanded steps.
      </div>
    );
  }
  return (
    <table style={{ margin: "var(--space-3) 0 0" }}>
      <thead>
        <tr>
          <th>step</th>
          <th>target</th>
          <th>action</th>
          <th>risk</th>
          <th>blast radius</th>
        </tr>
      </thead>
      <tbody>
        {steps.map((s, i) => (
          <tr key={i}>
            <td>{s.description ?? ""}</td>
            <td>
              {s.kind ?? ""}/{s.name ?? ""}
            </td>
            <td>
              {(s.action ?? "").toLowerCase().includes("restore") && (
                <ArrowCounterClockwise size={12} style={{ verticalAlign: "-1px", marginRight: 4 }} />
              )}
              {s.action ?? ""}
            </td>
            <td>{s.risk ? <StatusPill kind={kindOf(s.risk)}>{s.risk}</StatusPill> : ""}</td>
            <td>{s.blastRadius ?? ""}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
