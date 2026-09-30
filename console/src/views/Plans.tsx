import { useEffect, useState } from "react";
import {
  ArrowCounterClockwise,
  CaretDown,
  CaretRight,
  Pulse,
  SealCheck,
  WarningCircle,
  XCircle,
} from "@phosphor-icons/react";
import { api, ApiError, type EventRow, type Plan, type PlanRecord } from "../api";
import { ago, ErrorLine, useAction, useDismissals, useLoad, when } from "../lib";
import { useConsoleState } from "../state";
import { kindOf, KIND_COLOR, StatusPill } from "../ui/status";
import { ProblemCard } from "../ui/ProblemCard";

// How many resolved plans the history shows before you ask for more.
// A household with three hundred plans behind it is still a page that
// opens instantly.
const PAGE = 10;

// Plans (ADR-W016 A2, ADR-W027 A7): the signature interaction, and the
// one screen where a click changes the household.
//
// Everything approval needs is on the card — summary, risk, who
// proposed it for whom, and every step with its blast radius — so
// approving never means opening something else first. The result lands
// on the same card: who approved it and when, or why it failed. The old
// screen fired the request and left the page exactly as it was.
export function Plans({ onNavigate }: { onNavigate?: (view: "activity") => void }) {
  const { snap } = useConsoleState();
  const list = useLoad(() => loadReviewPlans(), []);
  if (list.error) return <ErrorLine error={list.error} />;
  // Poll snapshots contain summaries; approvals wait for complete records.
  const plans = list.data ?? [];
  return (<>
    <ReviewNotificationPreference />
    <PlansView
      plans={plans}
      events={snap?.events ?? []}
      onResolved={list.reload}
      onNavigate={onNavigate}
    />
  </>);
}

// Pending approvals always load complete records. Summary snapshots cannot
// render an approval button while the exact proposal is still loading.
export async function loadReviewPlans(client: Pick<typeof api, "plans" | "ownerReviewQueue" | "plan"> = api): Promise<PlanRecord[]> {
  const [history, queue] = await Promise.all([client.plans(), client.ownerReviewQueue().catch(error => {
    // Bootstrap administrators can inspect infrastructure before an Owner exists.
    if (error instanceof ApiError && error.status === 403) return [];
    throw error;
  })]);
  const full = new Map(queue.map(plan => [plan.id, plan]));
  const records = await Promise.all(history.map(plan => plan.state === "proposed" ? (full.get(plan.id) ?? client.plan(plan.id)) : plan));
  const seen = new Set(records.map(plan => plan.id));
  return [...records, ...queue.filter(plan => !seen.has(plan.id))];
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
  onNavigate,
}: {
  plans: PlanRecord[];
  events?: EventRow[];
  onResolved: () => void;
  onNavigate?: (view: "activity") => void;
}) {
  const pending = plans.filter((p) => p.state === "proposed");
  const resolved = plans.filter((p) => p.state !== "proposed");
  const dismissals = useDismissals();
  // A failure the console can still explain is worth raising out of the
  // history table: it is the reason the next thing you try will fail
  // too. One whose event has aged out of the kept window stays in the
  // table, because a card with nothing to say is just a louder row.
  //
  // It stops being worth raising once you have read it, and until now
  // there was no way to say so — the poll re-rendered the same three
  // cards above every approval, forever.
  const explained = resolved
    .map((p) => ({ plan: p, why: whyItFailed(p, events) }))
    .filter((x): x is { plan: PlanRecord; why: string } => x.why !== undefined)
    .filter((x) => !dismissals.ids.includes(x.plan.id));

  return (
    <>
      <div className="sub">
        Owner Review Queue. Review the exact version before approving. Editing requires a new approval.
      </div>

      {explained.length > 0 && (
        <div className="bands" style={{ marginBottom: "var(--space-6)" }}>
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
              onDismiss={() => dismissals.dismiss(plan.id)}
            >
              {/* The reconciler's own words. It names what to do; the
                  console's job is to put them where the failure is,
                  not to paraphrase them. */}
              <pre className="failure">{why}</pre>
            </ProblemCard>
          ))}
        </div>
      )}

      {/* Always present, so the page reads the same whether or not
          anything has been dismissed — and so "where did it go?" has an
          answer on the screen rather than in someone's memory. */}
      <div className="muted plans-note">
        <Pulse size={14} />
        {explained.length > 0
          ? "Dismissing a failure keeps it in the history below and on the stream."
          : "Every reconcile outcome is on the stream."}
        {onNavigate && (
          <button className="linklike" onClick={() => onNavigate("activity")}>
            Open Activity
          </button>
        )}
        {dismissals.ids.length > 0 && (
          <button className="linklike" onClick={dismissals.restoreAll}>
            Show dismissed ({dismissals.ids.length})
          </button>
        )}
      </div>

      <div className="sect">
        <span className="eyebrow">Waiting for you</span>
        <span className="muted tnum">{pending.length}</span>
        <span className="fade" />
      </div>

      {pending.length === 0 ? (
        <div className="allwell">Nothing is waiting for you.</div>
      ) : (
        <PendingBand plans={pending} onResolved={onResolved} />
      )}

      {resolved.length > 0 && <ResolvedHistory plans={resolved} />}
    </>
  );
}

// The approval band: one row per waiting plan, one open at a time.
//
// Every pending plan used to be a full card with its own steps table,
// so four proposals were four screens and the newest was the only one
// anybody read. The row carries what triages — summary, risk, age — and
// opening it is what commits you to reading the rest.
export function PendingBand({ plans, onResolved }: { plans: PlanRecord[]; onResolved: () => void }) {
  // The newest waiting plan opens by default; one at a time thereafter.
  const [openID, setOpenID] = useState<string | null>(plans[0]?.id ?? null);
  return (
    <div className="rows">
      {plans.map((p) => {
        const open = openID === p.id;
        const kind = kindOf(p.risk);
        return (
          <div className={open ? "prow on" : "prow"} key={p.id}>
            <button
              className="prow-head"
              onClick={() => setOpenID(open ? null : p.id)}
              aria-expanded={open}
            >
              <span className="acc" style={{ background: KIND_COLOR[kind] }} aria-hidden />
              {open ? (
                <CaretDown size={12} className="caret" />
              ) : (
                <CaretRight size={12} className="caret" />
              )}
              <SealCheck size={15} weight="fill" color={KIND_COLOR[kind]} />
              <span className="prow-title">{p.summary}</span>
              <StatusPill kind={kind}>{p.risk} risk</StatusPill>
              <span className="muted tnum" title={when(p.createdAt)}>
                {p.id} · {ago(p.createdAt)}
              </span>
            </button>
            {open && (
              <div className="prow-body">
                {/* The row head already names the plan; `bare` is what
                    keeps the summary from appearing twice. */}
                <PendingPlan plan={p} onResolved={onResolved} bare />
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// The resolved history: ten rows, then more on request. A table on a
// desk and stacked rows on a phone — the same data either way, because
// six columns at 390px is a horizontal scrollbar nobody uses.
export function ResolvedHistory({ plans }: { plans: PlanRecord[] }) {
  const [shown, setShown] = useState(PAGE);
  // A poll can remove rows under you; clamping at render is what keeps
  // "Showing 24 of 12" from ever being printed.
  const count = Math.min(shown, plans.length);
  const visible = plans.slice(0, count);
  useEffect(() => {
    if (plans.length <= PAGE) setShown(PAGE);
  }, [plans.length]);

  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-8)" }}>
        <span className="eyebrow">Resolved</span>
        <span className="muted tnum">{plans.length}</span>
        <span className="fade" />
      </div>

      <table className="plans-table">
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
          {visible.map((p) => (
            <tr key={p.id}>
              <td className="tnum">{p.id}</td>
              <td>
                <StatusPill kind={kindOf(p.state)}>{p.state}</StatusPill>
              </td>
              <td>
                <StatusPill kind={kindOf(p.risk)}>{p.risk}</StatusPill>
              </td>
              <td>{p.summary}</td>
              <td>{proposedBy(p)}</td>
              {/* A plan resolved before the API recorded who and when
                  has neither, and "never" would be a worse answer than
                  saying nothing. */}
              <td title={when(p.resolvedAt)}>{resolvedBy(p)}</td>
            </tr>
          ))}
        </tbody>
      </table>

      <div className="plans-cards">
        {visible.map((p) => (
          <div className="plan-row" key={p.id}>
            <div className="plan-row-head">
              <span>{p.summary}</span>
              <StatusPill kind={kindOf(p.state)}>{p.state}</StatusPill>
            </div>
            <div className="plan-row-meta">
              <span>{p.id}</span>
              <span>·</span>
              <span>{p.risk} risk</span>
              <span>·</span>
              <span>{proposedBy(p)}</span>
              <span>·</span>
              <span title={when(p.resolvedAt)}>{resolvedBy(p)}</span>
            </div>
          </div>
        ))}
      </div>

      <div className="plans-more">
        {count < plans.length && (
          <button className="btn btn-secondary" onClick={() => setShown((n) => n + PAGE)}>
            Show more
          </button>
        )}
        <span className="muted tnum">
          Showing {count} of {plans.length}
        </span>
      </div>
    </>
  );
}

function proposedBy(p: PlanRecord): string {
  return p.principal + (p.actingFor ? ` (for ${p.actingFor})` : "");
}

function resolvedBy(p: PlanRecord): string {
  return [p.resolvedBy, p.resolvedAt && ago(p.resolvedAt)].filter(Boolean).join(" · ") || "—";
}

// One pending plan, with its decision on it. Approve and Reject share a
// single useAction, so the two buttons, the pill and the explanation all
// read from one state and cannot disagree about what happened.
//
// `bare` drops the title and icon for the accordion row, whose head
// already names the plan. Everything a decision needs — who proposed it,
// the callout, the steps and the two buttons — is the same in both
// placements, because it is the same component.
export function PendingPlan({
  plan,
  onResolved,
  bare,
}: {
  plan: PlanRecord;
  onResolved: () => void;
  bare?: boolean;
}) {
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
        ? (plan.research ? "provider configuration applied" : plan.routine ? "version approved; automatic execution is not available yet" : "approved — handed to the reconciler")
        : "rejected — the Butler is told why on its next session",
    );

  const research = plan.research;
  const reviewableResearch = !research || Boolean(
    research.providerId &&
    (research.operation === "create" || research.expectedConfigRevision) &&
    (research.operation === "delete" ||
      ((research.operation === "create" || research.operation === "update") &&
        research.config?.name && research.config.kind && research.config.limits &&
        [research.config.limits.searchesPerTask, research.config.limits.fetchesPerTask,
          research.config.limits.requestTimeoutSeconds, research.config.limits.downloadBytes,
          research.config.limits.extractedCharacters, research.config.limits.cacheSeconds]
          .every(value => Number.isInteger(value) && value > 0) &&
        Array.isArray(research.config.backends) && Array.isArray(research.config.fallbackOrder))),
  );
  const risky = kindOf(plan.risk) === "bad";
  const restores = (plan.plan?.steps ?? []).some((s) => (s.action ?? "").toLowerCase().includes("restore"));

  return (
    <ProblemCard
      severity={done ? "ok" : kindOf(plan.risk)}
      icon={
        bare ? undefined : (
          <SealCheck size={16} weight="fill" color={done ? "var(--st-ok)" : "var(--st-warn)"} />
        )
      }
      title={bare ? null : plan.summary}
      meta={
        // Inside a row the head already carries the state, the risk and
        // the id. What it cannot carry is the RESULT of a decision made
        // just now, so that — and only that — is repeated here.
        bare && !done ? undefined : (
          <>
            <StatusPill kind={done ? "ok" : kindOf(plan.state)}>
              {done ? done.message : plan.state}
            </StatusPill>
            {!bare && <StatusPill kind={kindOf(plan.risk)}>{plan.risk} risk</StatusPill>}
            {!bare && (
              <span className="muted tnum" title={when(plan.createdAt)}>
                {plan.id} · {ago(plan.createdAt)}
              </span>
            )}
          </>
        )
      }
      body={
        plan.auditOnly ? <>Maintenance tracker update by {plan.principal}. Recorded immediately in household history; no Owner approval was required.</> : research ? (
          <>Proposed by {plan.principal}. Approving applies the provider registry change below.
            No service is provisioned and no Agent tool permissions are granted.</>
        ) : plan.setting ? (
          // A setting, not desired state: there is no reconciler step to
          // read, so say exactly what approving changes.
          <>
            An agent proposed this{plan.actingFor ? ` after ${plan.actingFor} asked` : ""}.{" "}
            {plan.setting.timezone && (
              <>
                Approving sets the household's time zone to <strong>{plan.setting.timezone}</strong>: agents,
                the calendar and household rules read times in it from then on.
              </>
            )}
            {plan.setting.reason && <> They said: “{plan.setting.reason}”.</>}
          </>
        ) : (
          <>
            Proposed by {plan.principal}
            {plan.actingFor ? ` acting for ${plan.actingFor}` : ""}
            {plan.basedOnHead ? `, against statefile ${plan.basedOnHead.slice(0, 8)}` : ""}.{" "}
            {done
              ? "Nothing else is waiting on you for this one."
              : plan.routine
                ? "Approving records authorization for this exact version. Automatic execution is not available yet."
                : "Approving records who approved it and hands the plan to the reconciler."}
          </>
        )
      }
      actions={
        <>
          {!done && (
            <>
              <button className="btn btn-primary" disabled={busy || !reviewableResearch} onClick={resolve("approve")}>
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
      bare={bare}
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
      {plan.routine && <section className="work-section">
        <h3>{plan.routine.name} · version {plan.routine.revision}</h3>
        <p>Approval records this version. Automatic execution is not available yet.</p>
        {plan.routine.proposingAgent && <p>Proposed by {plan.routine.proposingAgent} for {plan.actingFor}</p>}
        <p>Runs as: {plan.routine.assignedButler || "No Agent assigned"}</p>
        <p>Who may receive the result: {plan.routine.outputAudience}</p>
        <p>Household secrets this version may use: {plan.routine.requiredSecrets?.length ? plan.routine.requiredSecrets.map(id=>`${plan.routine?.secretLabels?.[id] ?? id} (${id})`).join(", ") : "None"}</p>
        <p>Memory records this version may read: {plan.routine.requiredMemory?.length ? plan.routine.requiredMemory.map(id=>`#${id}`).join(", ") : "None"}</p>
        <p>Where and how it is delivered:</p><pre>{plan.routine.delivery}</pre>
        <details><summary>Exact routine being approved</summary><pre>{plan.routine.document}</pre></details>
      </section>}
      {research && <section className="work-section">
        <h3>Search provider: {research.operation}</h3>
        <p>Provider ID: {research.providerId}</p>
        <p>Expected configuration revision: {research.expectedConfigRevision || "New provider"}</p>
        {!reviewableResearch && <ErrorLine error="The provider change is incomplete. Reload before approving." />}
        {research.operation === "delete" ? (
          <p>Removes this provider. Agents selecting it become unavailable; their selection does not fall back to another provider.</p>
        ) : <>
          <p>Clears any previous test result. This configuration must pass a new test before it is ready.</p>
          {research.config && <>
            <p>Name: {research.config.name}</p>
            <p>Kind: {research.config.kind}</p>
            <p>Endpoint reference: {research.config.endpointRef || "None — provider remains unconfigured"}</p>
            <p>Credential reference: {research.config.credentialRef || "None"}</p>
            <p>Backends: {research.config.backends?.join(", ") || "None"}</p>
            <p>Fallback order: {research.config.fallbackOrder?.join(" → ") || "None"}</p>
            {research.config.limits && <dl>
              <dt>Searches per task</dt><dd>{research.config.limits.searchesPerTask}</dd>
              <dt>Fetches per task</dt><dd>{research.config.limits.fetchesPerTask}</dd>
              <dt>Request timeout (seconds)</dt><dd>{research.config.limits.requestTimeoutSeconds}</dd>
              <dt>Maximum download (bytes)</dt><dd>{research.config.limits.downloadBytes}</dd>
              <dt>Maximum extracted characters</dt><dd>{research.config.limits.extractedCharacters}</dd>
              <dt>Cache lifetime (seconds)</dt><dd>{research.config.limits.cacheSeconds}</dd>
            </dl>}
          </>}
        </>}
      </section>}
      {!research && <PlanSteps plan={plan.plan} />}
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
    <>
      <table className="steps-table" style={{ margin: "var(--space-3) 0 0" }}>
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
              <td>{target(s)}</td>
              <td>
                {restoring(s) && (
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

      {/* The same steps at 390px, where five columns is a horizontal
          scrollbar over the one thing you have to read before approving. */}
      <div className="steps-cards">
        {steps.map((s, i) => (
          <div className="step-card" key={i}>
            <div className="step-card-head">
              <span>{s.description ?? ""}</span>
              {s.risk && <StatusPill kind={kindOf(s.risk)}>{s.risk}</StatusPill>}
            </div>
            <div className="step-card-target">
              {restoring(s) && (
                <ArrowCounterClockwise size={12} style={{ verticalAlign: "-1px", marginRight: 4 }} />
              )}
              {target(s)} · {s.action ?? ""}
            </div>
            <div className="muted">{s.blastRadius ?? ""}</div>
          </div>
        ))}
      </div>
    </>
  );
}

function target(s: { kind?: string; name?: string }): string {
  return `${s.kind ?? ""}/${s.name ?? ""}`;
}

// A restore is the one action that can be taken a safer way, so it is
// marked wherever a step is shown.
function restoring(s: { action?: string }): boolean {
  return (s.action ?? "").toLowerCase().includes("restore");
}

function ReviewNotificationPreference() {
 const saved=useLoad(()=>api.ownerReviewPreference());
 const accounts=useLoad(()=>api.chatBindings());
 const [binding,setBinding]=useState(0);
 const [url,setUrl]=useState("");
 const action=useAction();
 useEffect(()=>{if(saved.data){setBinding(saved.data.bindingId);setUrl(saved.data.consoleUrl)}},[saved.data]);
 return <details className="work-section"><summary>Review notifications</summary>
   <p>Choose an Owner's connected account for a short notice and Console link. Plan details stay in Console.</p>
   <ErrorLine error={saved.error ?? accounts.error}/>
   <label>Notify <select value={binding} onChange={e=>setBinding(Number(e.target.value))}>
     <option value={0}>No external notifications</option>
     {(accounts.data??[]).filter(b=>b.role==="owner").map(b=><option key={b.id} value={b.id}>{b.identity} · {b.transport} · {b.displayName || b.account}</option>)}
   </select></label>
   {binding!==0 && <label>Household Console address <input type="url" value={url} onChange={e=>setUrl(e.target.value)} /></label>}
   <button className="btn btn-secondary" disabled={action.state.phase==="busy"} onClick={()=>action.run(()=>api.setOwnerReviewPreference(binding,url),"Review notification preference saved.")}>Save</button>
   {action.state.phase==="error" && <ErrorLine error={action.state.message}/>}
   {action.state.phase==="done" && <p>{action.state.message}</p>}
 </details>
}
