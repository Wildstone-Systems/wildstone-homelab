import { Archive, ArrowCounterClockwise, Play } from "@phosphor-icons/react";
import { useState } from "react";
import { api, BackupPlanView, BackupRun, BackupTargetView } from "../api";
import type { BackupPlanSpec, BackupTargetSpec } from "../generated/resources";
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
  return <BackupsView runs={ledger.data?.runs ?? []} plans={ledger.data?.plans ?? []} targets={ledger.data?.targets ?? []} onRan={ledger.reload} loading={ledger.loading} />;
}

export interface PlanLedger {
  name: string;
  runs: BackupRun[];
  last?: BackupRun;
  health: Kind;
  config?: BackupPlanView;
}

// Newest run first within each plan; plans whose last run failed first
// overall. A ledger sorted by time answers "what happened"; this one
// answers "what needs me".
export function byPlan(runs: BackupRun[], configured: BackupPlanView[] = []): PlanLedger[] {
  const RANK: Record<Kind, number> = { bad: 0, warn: 1, mute: 2, ok: 3 };
  const map = new Map<string, BackupRun[]>();
  for (const r of runs) map.set(r.planName, [...(map.get(r.planName) ?? []), r]);
  for (const p of configured) if (!map.has(p.name)) map.set(p.name, []);
  const configs = new Map(configured.map((p) => [p.name, p]));
  return [...map.entries()]
    .map(([name, list]) => {
      const sorted = [...list].sort((a, b) => b.seq - a.seq);
      const last = sorted[0];
      return { name, runs: sorted, last, health: kindOf(last?.status), config: configs.get(name) };
    })
    .sort((a, b) => RANK[a.health] - RANK[b.health] || a.name.localeCompare(b.name));
}

export function BackupsView({
  runs,
  onRan,
  loading,
  plans = [],
  targets = [],
}: {
  runs: BackupRun[];
  onRan: () => void;
  loading?: boolean;
  plans?: BackupPlanView[];
  targets?: BackupTargetView[];
}) {
  const ledgers = byPlan(runs, plans);
  const classOrder = ["critical", "important", "rebuildable", "unclassified"];
  const groups = classOrder.map((name) => ({ name, ledgers: ledgers.filter((p) => (p.config?.spec.class ?? "unclassified") === name) })).filter((g) => g.ledgers.length > 0);
  return (
    <>
      <div className="sub">
        Scheduled runs are the engine's standing duty. The buttons here run one now, or restore
        alongside the live data rather than over it.
      </div>
      <TargetManager targets={targets} onChanged={onRan} />
      {ledgers.length === 0 && !loading && (
        <div className="muted">
          No BackupPlans are configured yet.
        </div>
      )}
      {groups.map((group) => <section key={group.name} style={{ marginTop: "var(--space-5)" }}>
        <div className="eyebrow">{group.name} data</div>
        <div className="bands">
          {group.ledgers.map((p) => <BackupPlanCard key={p.name} ledger={p} targets={targets} onRan={onRan} />)}
        </div>
      </section>)}
    </>
  );
}

export function BackupPlanCard({ ledger, targets = [], onRan }: { ledger: PlanLedger; targets?: BackupTargetView[]; onRan: () => void }) {
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
  const latest = {
    local: ledger.runs.find((r) => r.runKind === "backup"),
    offsite: ledger.runs.find((r) => r.runKind === "offsite"),
    check: ledger.runs.find((r) => r.runKind === "offsite-check" || r.runKind === "check"),
    drill: ledger.runs.find((r) => r.runKind === "offsite-drill" || r.runKind === "drill"),
  };

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
                "restore completed alongside the live data",
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
          <div className="muted" style={{ marginTop: "var(--space-2)" }}>
            {Object.entries(latest).map(([label, item]) => <div key={label}>{label}: {item ? `${item.status} · ${ago(item.startedAt)}` : "never"}</div>)}
          </div>
          {running && (
            <div style={{ marginTop: "var(--space-3)" }}>
              <FillBar kind="warn" caption="running — the engine reports no progress figure" />
            </div>
          )}
          {ledger.config && <PlanRouteEditor plan={ledger.config} targets={targets} onChanged={onRan} />}
        </>
      }
    />
  );
}

function TargetManager({ targets, onChanged }: { targets: BackupTargetView[]; onChanged: () => void }) {
  const [adding, setAdding] = useState(false);
  // Credentials are written before the target proposal can be approved. Give
  // every proposal fresh names so a rejected or failed proposal can leave only
  // unused secrets behind, never replace credentials referenced by a live
  // target.
  const [draft, setDraft] = useState(() => {
    const suffix = crypto.randomUUID().slice(0, 8);
    return { name: "offsite", node: "", endpoint: "", bucket: "", prefix: "", region: "", accessName: `offsite-${suffix}-access`, secretName: `offsite-${suffix}-secret`, accessValue: "", secretValue: "", pathStyle: false, insecureHTTP: false, appendOnly: true };
  });
  const save = useAction();
  const test = useAction();
  const submit = () => save.run(async () => {
    const accessRef = `secret/${draft.accessName}`;
    const secretRef = `secret/${draft.secretName}`;
    const liveRefs = new Set(targets.flatMap((target) => [target.spec.accessKeySecret, target.spec.secretKeySecret]));
    if (draft.accessValue && liveRefs.has(accessRef)) throw new Error("That access-key secret is used by a live target. Choose a new secret name before saving credentials.");
    if (draft.secretValue && liveRefs.has(secretRef)) throw new Error("That secret-key secret is used by a live target. Choose a new secret name before saving credentials.");
    if (draft.accessValue && draft.secretValue && accessRef === secretRef) throw new Error("Access-key and secret-key credentials need different secret names.");
    if (draft.accessValue) await api.setSecret(draft.node, draft.accessName, draft.accessValue);
    if (draft.secretValue) await api.setSecret(draft.node, draft.secretName, draft.secretValue);
    const spec: BackupTargetSpec = { type: "s3", endpoint: draft.endpoint, bucket: draft.bucket, prefix: draft.prefix || undefined, region: draft.region || undefined, pathStyle: draft.pathStyle, insecureHTTP: draft.insecureHTTP, appendOnly: draft.appendOnly, accessKeySecret: accessRef, secretKeySecret: secretRef };
    const plan = await api.proposeBackupTarget({ name: draft.name, node: draft.node, spec });
    onChanged(); setAdding(false); return plan;
  }, "target proposed — approve it on Plans, then test the connection");
  return <div className="panel" style={{ margin: "var(--space-4) 0" }}>
    <div className="row" style={{ justifyContent: "space-between" }}><strong>Off-site targets</strong><button className="btn btn-secondary" onClick={() => setAdding(!adding)}>Add S3 target…</button></div>
    {targets.length === 0 && !adding && <div className="muted">No off-site target configured.</div>}
    {targets.map((t) => <div className="row" key={t.name} style={{ justifyContent: "space-between" }}><span><strong>{t.name}</strong> · {t.spec.bucket} · {t.node}</span><button className="btn btn-secondary" disabled={test.state.phase === "busy"} onClick={() => test.run(() => api.testBackupTarget(t.name), "read-only bucket check succeeded")}>Test</button></div>)}
    {adding && <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit,minmax(220px,1fr))", gap: "var(--space-3)", marginTop: "var(--space-3)" }}>
      {([['name','Target name'],['node','Node'],['endpoint','S3 endpoint'],['bucket','Bucket'],['prefix','Prefix (optional)'],['region','Region'],['accessName','Access-key secret name'],['secretName','Secret-key secret name'],['accessValue','Access key value'],['secretValue','Secret key value']] as const).map(([key,label]) => <div className="field" style={{ marginBottom: 0 }} key={key}><label>{label}</label><input type={key.endsWith('Value') ? 'password' : 'text'} value={draft[key]} onChange={(e) => setDraft(d => ({...d,[key]:e.target.value}))} /></div>)}
      <label className="checkrow"><input type="checkbox" checked={draft.pathStyle} onChange={e => setDraft(d => ({...d,pathStyle:e.target.checked}))}/> Path-style addressing</label>
      <label className="checkrow"><input type="checkbox" checked={draft.insecureHTTP} onChange={e => setDraft(d => ({...d,insecureHTTP:e.target.checked}))}/> Allow HTTP (LAN only)</label>
      <label className="checkrow"><input type="checkbox" checked={draft.appendOnly} onChange={e => setDraft(d => ({...d,appendOnly:e.target.checked}))}/> Append-only credential</label>
      <div className="row"><button className="btn btn-primary" disabled={save.state.phase === "busy" || !draft.node || !draft.endpoint || !draft.bucket} onClick={submit}>Save credentials & propose target</button></div>
    </div>}
    {save.state.phase === "done" && <StatusPill kind="ok">{save.state.message}</StatusPill>}
    {save.state.phase === "error" && <ErrorLine error={save.state.message} />}{test.state.phase === "done" && <StatusPill kind="ok">{test.state.message}</StatusPill>}{test.state.phase === "error" && <ErrorLine error={test.state.message} />}
  </div>;
}

function PlanRouteEditor({ plan, targets, onChanged }: { plan: BackupPlanView; targets: BackupTargetView[]; onChanged: () => void }) {
  const [editing, setEditing] = useState(false);
  const [spec, setSpec] = useState<BackupPlanSpec>(() => structuredClone(plan.spec));
  const save = useAction();
  const check = useAction();
  const [estimate, setEstimate] = useState<{ sourceBytes: number; diskCheck: string; estimatedMonthlyCostUsd?: number; costNote?: string }>();
  const off = spec.offsite;
  const patchOff = (v: Partial<NonNullable<BackupPlanSpec["offsite"]>>) => setSpec(s => ({ ...s, offsite: { target: targets[0]?.name ?? "", route: "local-and-offsite", passwordSecret: "secret/offsite-restic-password", schedule: "daily", ...(s.offsite ?? {}), ...v } }));
  const source = plan.sourceBytes == null ? "not measured yet" : `${(plan.sourceBytes / 1e9).toFixed(1)} GB source`;
  return <div style={{ marginTop: "var(--space-3)" }}>
    <div className="muted">{plan.spec.class ?? "unclassified"} · {source}{plan.estimatedMonthlyCostUsd != null ? ` · about $${plan.estimatedMonthlyCostUsd.toFixed(2)}/month` : ""}</div>
    {plan.costNote && <div className="muted" title={plan.costNote}>Storage estimate only</div>}
    <button className="btn btn-ghost btn-small" onClick={() => setEditing(!editing)}>{editing ? "Close settings" : "Route & sources…"}</button>
    {editing && <div style={{ marginTop: "var(--space-2)" }}>
      <div className="field"><label>Data class</label><select value={spec.class ?? ""} onChange={e => setSpec(s => ({...s,class:e.target.value}))}><option value="">Choose…</option><option value="critical">Critical</option><option value="important">Important</option><option value="rebuildable">Rebuildable</option></select></div>
      <div className="field"><label>Local repository (volume or NAS path)</label><input value={spec.repository ?? ""} onChange={e => setSpec(s => ({...s,repository:e.target.value}))}/></div>
      <div className="field"><label>Local repository password secret</label><input value={spec.passwordSecret ?? ""} onChange={e => setSpec(s => ({...s,passwordSecret:e.target.value}))}/></div>
      <div className="field"><label>Local schedule</label><select value={spec.schedule ?? ""} onChange={e => setSpec(s => ({...s,schedule:e.target.value}))}><option value="">None</option><option>daily</option><option>weekly</option><option>monthly</option></select></div>
      <div className="field"><label>Disk reserve</label><input placeholder="20GiB or 15%" value={spec.reserve ?? ""} onChange={e => setSpec(s => ({...s,reserve:e.target.value || undefined}))}/></div>
      <div className="field"><label>Source paths (one per line)</label><textarea value={(spec.paths ?? []).join('\n')} onChange={e => setSpec(s => ({...s,paths:e.target.value.split(/\n/).filter(Boolean)}))}/></div>
      <div className="field"><label>Excludes (one per line)</label><textarea value={(spec.exclude ?? []).join('\n')} onChange={e => setSpec(s => ({...s,exclude:e.target.value.split(/\n/).filter(Boolean)}))}/></div>
      <div className="field"><label>Off-site route</label><select value={off?.route ?? "none"} onChange={e => {
        if (e.target.value === 'none') return setSpec(s => ({...s,offsite:undefined}));
        if (e.target.value === 'offsite-only') return setSpec(s => ({...s,repository:undefined,reserve:undefined,passwordSecret:undefined,schedule:undefined,retention:undefined,offsite:{target:s.offsite?.target ?? targets[0]?.name ?? "",route:'offsite-only',passwordSecret:s.offsite?.passwordSecret ?? 'secret/offsite-restic-password',schedule:s.offsite?.schedule ?? 'daily',retention:s.offsite?.retention}}));
        patchOff({route:e.target.value});
      }}><option value="none">Local only</option><option value="local-and-offsite">Local + off-site</option><option value="offsite-only">Off-site only</option></select></div>
      {off && <><div className="field"><label>Target</label><select value={off.target} onChange={e => patchOff({target:e.target.value})}>{targets.map(t => <option key={t.name}>{t.name}</option>)}</select></div><div className="field"><label>Off-site password secret</label><input value={off.passwordSecret} onChange={e => patchOff({passwordSecret:e.target.value})}/></div><div className="field"><label>Off-site schedule</label><select value={off.schedule} onChange={e => patchOff({schedule:e.target.value})}><option>daily</option><option>weekly</option><option>monthly</option></select></div></>}
      <div className="row"><button className="btn btn-secondary" disabled={check.state.phase === "busy"} onClick={() => check.run(async () => { const result = await api.estimateBackupPlan(plan.name, plan.node, spec); setEstimate(result); return result; }, "disk and cost estimate ready")}>Estimate size & space</button>
      <button className="btn btn-primary" disabled={save.state.phase === "busy"} onClick={() => save.run(() => api.proposeBackupPlan(plan.name, plan.node, spec).then(onChanged), "change proposed — approve it on Plans")}>Check disk & propose</button></div>
      {estimate && <div className="note">{estimate.diskCheck}{estimate.estimatedMonthlyCostUsd != null ? `; about $${estimate.estimatedMonthlyCostUsd.toFixed(2)}/month off-site` : ""}{estimate.costNote ? ` (${estimate.costNote})` : ""}</div>}
      {check.state.phase === "error" && <ErrorLine error={check.state.message}/>}
      {save.state.phase === "done" && <StatusPill kind="ok">{save.state.message}</StatusPill>}
      {save.state.phase === "error" && <ErrorLine error={save.state.message}/>}
    </div>}
  </div>;
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
