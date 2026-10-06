import { useState } from "react";
import { ArrowRight, Plus } from "@phosphor-icons/react";
import {
  api,
  EnablePreview,
  HomeAutomationComponent,
  HomeAutomationComponentId,
  HomeAutomationContinuity,
  HomeAutomationContinuityView,
  HomeAutomationNode,
  HomeAutomationRadioOutcome,
  HomeAutomationStatus,
  PackageView,
  PlanRecord,
} from "../api";
import { ago, ErrorLine, useLoad } from "../lib";
import { Kind, StatusPill } from "../ui/status";
import { PlanSteps } from "./Plans";
import {
  ActionHandlers,
  BlockerCard,
  ComponentPill,
  Draft,
  Goal,
  GOAL_COPY,
  GOALS,
  goalComponents,
  radioIdentity,
  radioName,
} from "./HomeAutomation";

// The composed status page (#451, epic #431): once anything for home
// automation is installed, the screen opens here rather than on the
// onboarding choices. One card per part — Home Assistant, the broker,
// each protocol — with where it runs, how it is reached, its radio, when
// it was last seen healthy, any plan in flight, its backup, and exactly
// what to do about anything wrong.
//
// Every repair here is one the API offers, done the way the rest of the
// Console does it:
//
//   - "Check again" re-asks the aggregate, which re-probes the machine.
//   - Opening a service, a plan or Backups goes to that screen; the
//     generic Services detail stays the place for advanced diagnosis.
//   - An upgrade or a settings change is the package's own Reconfigure
//     on Services: the capability pipeline's preview, then a plan.
//   - A replacement radio is an ordinary capability reconfigure: the
//     installed params with the radio param the server names, previewed
//     (dry run) and then filed as a plan. Never approved here.
//   - A conflicting owner is released from ITS service, opened from here.
//   - Removal is the capability removal's own dry run, then its plan.
//     Stored data is never deleted (ADR-W004 A4): the page says what
//     stays, and refuses to offer a removal whose plan would touch it.
//
// What a radio swap keeps and what a removal stops come from GET
// /api/v1/home-automation/continuity, sourced from what the packages
// rendered (their volumes and backup classes) and how each protocol
// keeps its network; nothing is guessed here.
//
// Adding a protocol later re-enters only that slice of the setup: the
// readiness check and setup sequence for what was added, with what is
// already installed reading as done.

// The calls the repair panels make, as an interface so the render check
// drives them with a fake.
export interface CareClient {
  packages: () => Promise<PackageView[]>;
  previewCapability: (pkg: string, params: Record<string, string>) => Promise<EnablePreview>;
  enableCapability: (pkg: string, params: Record<string, string>) => Promise<PlanRecord>;
  previewDisablePackage: (pkg: string) => Promise<PlanRecord>;
  disablePackage: (pkg: string) => Promise<PlanRecord>;
}

export type Panel = { kind: "radio" | "remove"; component: HomeAutomationComponentId } | null;

// A component on the status page: installed, or on its way in.
export function present(c: HomeAutomationComponent): boolean {
  return c.selection === "installed" || c.selection === "pending";
}

// The parts that need the household: anything degraded or blocked, a
// failed plan, a warning (stale, backup), or a step left outside.
export function needsAttention(c: HomeAutomationComponent): boolean {
  return (
    c.state === "degraded" ||
    c.state === "blocked" ||
    c.state === "external_step" ||
    !!c.failedPlan ||
    c.blockers.length > 0
  );
}

// When the part was last seen, and whether it was healthy then. Every
// observed resource healthy: the newest observation. Anything not: said
// so, with when it was seen that way.
export function lastHealthy(c: HomeAutomationComponent): { kind: Kind; text: string } | undefined {
  const rs = (c.resources ?? []).filter((r) => r.observed);
  if (rs.length === 0) return undefined;
  const newest = (xs: typeof rs) =>
    xs.map((r) => r.observedAt).filter((t): t is string => !!t).sort().pop();
  const bad = rs.filter((r) => !r.healthy || r.appHealth === "unhealthy");
  if (bad.length === 0) return { kind: "ok", text: `Healthy, last checked ${ago(newest(rs))}` };
  return { kind: "bad", text: `Not healthy when last checked ${ago(newest(bad))}` };
}

const CLASS_WORDS: Record<string, string> = {
  critical: "irreplaceable",
  important: "painful to rebuild",
  rebuildable: "can be rebuilt",
};

// The part's backup, in one line: whether its data is covered, how the
// newest run went, and how much the data matters (its plan's class).
export function backupLine(c: HomeAutomationComponent, cont?: HomeAutomationContinuity): { kind: Kind; text: string } | undefined {
  const b = c.backup;
  if (!b) return undefined;
  const cls = cont?.data.find((d) => !d.repository && d.class)?.class;
  const why = cls ? ` Its data is ${CLASS_WORDS[cls] ?? cls}.` : "";
  if (b.uncovered.length > 0) return { kind: "bad", text: `Not backed up: ${b.uncovered.join(", ")}.${why}` };
  if (b.lastRunStatus === "failed")
    return {
      kind: "bad",
      text: `The last backup failed${b.lastSuccessAt ? `; the last good one was ${ago(b.lastSuccessAt)}` : ", and none has succeeded yet"}.${why}`,
    };
  if (!b.lastSuccessAt) return { kind: "warn", text: `Covered, but no backup has finished yet.${why}` };
  return { kind: "ok", text: `Backed up ${ago(b.lastSuccessAt)}.${why}` };
}

// The params a replacement radio is proposed with: what the package is
// installed with now, and the radio param the server named set to the
// new radio's stable path. Nothing else changes.
export function replacementParams(pkg: PackageView, param: string, path: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const p of pkg.params ?? []) if (p.installed !== undefined) out[p.name] = p.installed;
  out[param] = path;
  return out;
}

// What a removal's dry run would delete, refused outright if it names
// stored data: Wildstone never deletes a volume or a seeded file, and a
// preview that says otherwise is not one to act on.
export function removalSteps(plan: PlanRecord | null | undefined): { deletes: { kind: string; name: string }[]; touchesData: boolean } {
  const deletes = (plan?.plan?.steps ?? [])
    .filter((s) => s.kind && s.name)
    .map((s) => ({ kind: s.kind as string, name: s.name as string }));
  return { deletes, touchesData: deletes.some((d) => d.kind === "Volume" || d.kind === "SeedFile") };
}

export function StatusPage({
  status,
  continuity,
  continuityError = null,
  draft,
  onDraft,
  handlers,
  client = api,
  initialPanel = null,
}: {
  status: HomeAutomationStatus;
  continuity: HomeAutomationContinuityView | null;
  continuityError?: string | null;
  draft: Draft;
  onDraft: (d: Draft) => void;
  handlers: ActionHandlers;
  client?: CareClient;
  // For the render check: a panel already open.
  initialPanel?: Panel;
}) {
  const [panel, setPanel] = useState<Panel>(initialPanel);
  const parts = status.components.filter(present);
  const attention = parts.filter(needsAttention);
  const conts = new Map((continuity?.components ?? []).map((c) => [c.component, c]));
  const titles = new Map(status.components.map((c) => [c.id, c.title]));
  const toggle = (p: NonNullable<Panel>) =>
    setPanel(panel && panel.kind === p.kind && panel.component === p.component ? null : p);
  const managed = parts.some((c) => c.id === "home-assistant");
  const topology = managed ? "managed" : "external";
  const leftOutside = parts.filter((c) => c.externalSteps.some((s) => s.status !== "done"));
  const pending = parts.filter((c) => c.pendingPlan);
  const settled = parts.filter((c) => c.selection === "installed");

  return (
    <section className="ha-status" aria-labelledby="ha-status-title">
      <div className="sect">
        <h2 id="ha-status-title" className="ha-legend">
          Your home automation
        </h2>
        <span className="fade" />
        <span className="muted ha-checked" aria-live="polite">
          {handlers.loading ? "Checking…" : `Checked ${ago(status.generatedAt)}`}
        </span>
        <button type="button" className="btn btn-ghost" disabled={handlers.loading} onClick={handlers.onRefresh}>
          Check again
        </button>
      </div>

      {attention.length === 0 ? (
        <div className="callout callout-ok" role="status">
          Everything is running: {settled.length === 1 ? "the one part installed is" : `all ${settled.length} parts installed are`}{" "}
          healthy.
          {pending.length > 0 && ` ${pending.map((c) => c.title).join(" and ")}: a change is in flight, below.`}
        </div>
      ) : (
        <div className={attention.some((c) => c.state === "degraded" || c.state === "blocked") ? "callout callout-bad" : "callout callout-warn"} role="status">
          {attention.length === 1 ? `${attention[0].title} needs` : `${attention.length} parts need`} attention. Each card says what
          was found and what you can do about it.
        </div>
      )}
      <ErrorLine error={continuityError ? `What each part keeps could not be read: ${continuityError}` : null} />

      <ul className="ha-status-list">
        {parts.map((c) => (
          <StatusCard
            key={c.id}
            c={c}
            cont={conts.get(c.id)}
            status={status}
            titles={titles}
            handlers={{ ...handlers, onChooseRadio: (id) => setPanel({ kind: "radio", component: id ?? c.id }) }}
            panel={panel && panel.component === c.id ? panel.kind : null}
            onPanel={(kind) => toggle({ kind, component: c.id })}
            client={client}
          />
        ))}
      </ul>

      {(leftOutside.length > 0 || pending.length > 0) && (
        <div className="card ha-card">
          <h3>Setup in progress</h3>
          <div className="muted">
            {pending.length > 0 && `${pending.map((c) => c.title).join(", ")}: a plan is in flight. `}
            {leftOutside.length > 0 && `${leftOutside.map((c) => c.title).join(", ")}: steps in Home Assistant are left.`}
          </div>
          <div className="row ha-actions">
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              onClick={() => onDraft({ ...draft, topology: draft.topology ?? topology, step: "setup" })}
            >
              Continue setup <ArrowRight size={14} aria-hidden />
            </button>
          </div>
        </div>
      )}

      <AddMore status={status} draft={draft} topology={draft.topology ?? topology} onDraft={onDraft} />
    </section>
  );
}

function StatusCard({
  c,
  cont,
  status,
  titles,
  handlers,
  panel,
  onPanel,
  client,
}: {
  c: HomeAutomationComponent;
  cont?: HomeAutomationContinuity;
  status: HomeAutomationStatus;
  titles: Map<HomeAutomationComponentId, string>;
  handlers: ActionHandlers;
  panel: "radio" | "remove" | null;
  onPanel: (kind: "radio" | "remove") => void;
  client: CareClient;
}) {
  const node = status.nodes.find((n) => n.id === c.node);
  const health = lastHealthy(c);
  const backup = backupLine(c, cont);
  const installed = c.selection === "installed";
  // Blocker remedies are rendered on the blockers; the part's own
  // actions only add what is not already there.
  const offered = new Set(c.blockers.flatMap((b) => b.actions.map((a) => `${a.kind}|${a.package ?? ""}`)));
  const service = c.actions.find((a) => a.kind === "open_service");
  const extra = c.actions.filter(
    (a) => a.kind !== "open_service" && a.kind !== "open_plan" && !offered.has(`${a.kind}|${a.package ?? ""}`),
  );
  const outside = c.externalSteps.filter((s) => s.status !== "done");
  const titleId = `ha-status-${c.id}`;

  return (
    <li className="card ha-card ha-status-card" id={`ha-comp-${c.id}`} aria-labelledby={titleId}>
      <div className="ha-readiness-row">
        <h3 id={titleId}>{c.title}</h3>
        <ComponentPill c={c} />
      </div>
      <div className="muted">{c.reason}</div>
      <dl className="ha-facts">
        {c.node && (
          <>
            <dt>Machine</dt>
            <dd>
              {c.node}
              {node && !node.connected && <StatusPill kind="bad">Offline, last seen {ago(node.lastSeenAt)}</StatusPill>}
            </dd>
          </>
        )}
        {(c.addresses?.length ?? 0) > 0 && (
          <>
            <dt>Reached at</dt>
            <dd>
              {c.addresses!.map((a) => (
                <span key={a.url} className="ha-pill-line">
                  <span className="ha-mono">{a.url}</span>
                  <StatusPill kind={a.healthy ? "ok" : "bad"} title={a.detail}>
                    {a.healthy ? "Answering" : "Not answering"}
                  </StatusPill>
                </span>
              ))}
            </dd>
          </>
        )}
        {(c.radios?.length ?? 0) > 0 && (
          <>
            <dt>Radio</dt>
            <dd>
              <RadioFact c={c} node={node} />
            </dd>
          </>
        )}
        {health && (
          <>
            <dt>Last seen</dt>
            <dd>
              <StatusPill kind={health.kind}>{health.text}</StatusPill>
            </dd>
          </>
        )}
        {(c.installedVersion || c.version) && installed && (
          <>
            <dt>Version</dt>
            <dd>
              <span className="ha-mono">
                {c.package} {c.installedVersion ?? c.version}
              </span>
              {c.stale && <StatusPill kind="warn">Update available: {c.version}</StatusPill>}
            </dd>
          </>
        )}
        {c.pendingPlan && (
          <>
            <dt>In flight</dt>
            <dd>
              <span>
                {c.pendingPlan.summary} (plan <span className="ha-mono">{c.pendingPlan.id}</span>,{" "}
                {c.pendingPlan.state === "proposed" ? "waiting for your approval" : "being applied"})
              </span>
            </dd>
          </>
        )}
        {cont?.holds && (
          <>
            <dt>Keeps</dt>
            <dd>{cont.holds}</dd>
          </>
        )}
        {backup && (
          <>
            <dt>Backup</dt>
            <dd>
              <StatusPill kind={backup.kind}>{backup.text}</StatusPill>
            </dd>
          </>
        )}
      </dl>

      {c.blockers.map((b, i) => (
        <BlockerCard key={`${b.code}-${i}`} b={b} handlers={handlers} checkedAt={status.generatedAt} />
      ))}
      {outside.length > 0 && (
        <div className="muted ha-setup-p">
          {outside.length === 1 ? "One step in Home Assistant is" : `${outside.length} steps in Home Assistant are`} left:{" "}
          {outside.map((s) => s.title).join("; ")}.
        </div>
      )}

      <div className="row ha-actions">
        {c.pendingPlan && handlers.onNavigate && (
          <button type="button" className="btn btn-secondary btn-sm" onClick={() => handlers.onNavigate!("plans")}>
            Review the plan
          </button>
        )}
        {extra.map((a, i) => (
          <button
            key={i}
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() => (a.package ? handlers.onOpenPackage(a.package) : handlers.onOpenServices())}
          >
            {a.label}
          </button>
        ))}
        {installed && cont?.radio && (
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            aria-expanded={panel === "radio"}
            aria-controls={`ha-panel-radio-${c.id}`}
            onClick={() => onPanel("radio")}
          >
            Replace or move the radio
          </button>
        )}
        {(service || (installed && c.package)) && (
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => handlers.onOpenPackage((service?.package ?? c.package)!)}>
            {cont?.removal.setting ? "Open Home Assistant's settings" : "Open the service"}
          </button>
        )}
        {installed && cont && (
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            aria-expanded={panel === "remove"}
            aria-controls={`ha-panel-remove-${c.id}`}
            onClick={() => onPanel("remove")}
          >
            {cont.removal.setting ? "Turn off…" : "Remove…"}
          </button>
        )}
      </div>

      {panel === "radio" && cont?.radio && (
        <RadioReplace c={c} cont={cont} node={node} handlers={handlers} client={client} />
      )}
      {panel === "remove" && cont && (
        <Removal c={c} cont={cont} titles={titles} handlers={handlers} client={client} onClose={() => onPanel("remove")} />
      )}
    </li>
  );
}

function RadioFact({ c, node }: { c: HomeAutomationComponent; node?: HomeAutomationNode }) {
  const inv = node?.radioInventory?.radios ?? [];
  return (
    <>
      {c.radios!.map((o, i) => {
        const r = o.radioId ? inv.find((x) => x.id === o.radioId) : undefined;
        return (
          <span key={`${o.device}-${i}`} className="ha-pill-line">
            <span>{r ? radioName(r) : <span className="ha-mono">{o.source || o.device}</span>}</span>
            {o.radioId ? (
              !radioIdentity(o.radioId).stable && <StatusPill kind="warn">Tied to its USB port</StatusPill>
            ) : (
              <StatusPill kind="bad">Not plugged in</StatusPill>
            )}
          </span>
        );
      })}
    </>
  );
}

// ---------------------------------------------------------------- radio

export interface RadioReplaceState {
  choice?: string;
  busy?: boolean;
  preview?: EnablePreview | null;
  filed?: PlanRecord | null;
  error?: string | null;
}

function RadioReplace({
  c,
  cont,
  node,
  handlers,
  client,
}: {
  c: HomeAutomationComponent;
  cont: HomeAutomationContinuity;
  node?: HomeAutomationNode;
  handlers: ActionHandlers;
  client: CareClient;
}) {
  const [st, setSt] = useState<RadioReplaceState>({});
  const radio = cont.radio!;
  const params = async (radioId: string) => {
    const path = radio.replacements.find((r) => r.radioId === radioId)?.path;
    if (!path || !radio.param || !c.package) throw new Error("That radio is no longer offered. Check again.");
    const pkg = (await client.packages()).find((p) => p.name === c.package && p.enabled);
    if (!pkg) throw new Error(`${c.package} is not installed.`);
    return replacementParams(pkg, radio.param, path);
  };
  const run = async (propose: boolean) => {
    if (!st.choice || !c.package) return;
    setSt({ ...st, busy: true, error: null });
    try {
      const p = await params(st.choice);
      if (propose) {
        const filed = await client.enableCapability(c.package, p);
        setSt({ choice: st.choice, filed });
        handlers.onRefresh();
      } else {
        setSt({ choice: st.choice, preview: await client.previewCapability(c.package, p) });
      }
    } catch (e) {
      setSt({ ...st, busy: false, error: (e as Error).message });
    }
  };
  return (
    <RadioReplaceView
      c={c}
      cont={cont}
      node={node}
      state={st}
      handlers={handlers}
      onChoose={(id) => setSt({ choice: id })}
      onPreview={() => void run(false)}
      onPropose={() => void run(true)}
    />
  );
}

function outcomeWords(o: HomeAutomationRadioOutcome): { kind: Kind; label: string } {
  return o.outcome === "kept"
    ? { kind: "ok", label: "Devices keep working" }
    : { kind: "warn", label: "Kept by restoring a backup" };
}

// The radio panel, separated from its calls for the render check.
export function RadioReplaceView({
  c,
  cont,
  node,
  state,
  handlers,
  onChoose,
  onPreview,
  onPropose,
}: {
  c: HomeAutomationComponent;
  cont: HomeAutomationContinuity;
  node?: HomeAutomationNode;
  state: RadioReplaceState;
  handlers: ActionHandlers;
  onChoose: (radioId: string) => void;
  onPreview: () => void;
  onPropose: () => void;
}) {
  const radio = cont.radio!;
  const inv = node?.radioInventory?.radios ?? [];
  const where = c.node ?? "this machine";
  const current = c.radios?.find((o) => o.radioId);
  const portBound = current?.radioId && !radioIdentity(current.radioId).stable;
  const data = cont.data.filter((d) => !d.repository);
  const noBackup = !c.backup?.lastSuccessAt || (c.backup?.uncovered.length ?? 0) > 0;
  const moved = outcomeWords(radio.moved);
  const replaced = outcomeWords(radio.replaced);
  const fieldName = `ha-replace-${c.id}`;
  return (
    <div className="ha-panel" id={`ha-panel-radio-${c.id}`} role="region" aria-labelledby={`${fieldName}-title`}>
      <h4 id={`${fieldName}-title`} className="ha-legend-sub">
        Replacing or moving the {c.title} radio
      </h4>
      <p className="ha-setup-p">
        {radio.networkOn === "machine"
          ? `The ${c.title} network lives on ${where}, not on the radio.`
          : `The ${c.title} network lives partly on the radio itself and partly on ${where}.`}
        {cont.holds ? ` Wildstone keeps ${cont.holds} in ${data.map((d) => d.volume).join(", ") || "its data"}.` : ""}
      </p>
      <dl className="ha-facts">
        <dt>Same radio, another port</dt>
        <dd>
          <StatusPill kind={moved.kind}>{moved.label}</StatusPill>
          <span>{radio.moved.summary}</span>
          {portBound && (
            <span className="ha-option-note">
              This radio is recognised by the USB port it is in, so after moving it, choose it again below.
            </span>
          )}
        </dd>
        <dt>A new radio</dt>
        <dd>
          <StatusPill kind={replaced.kind}>{replaced.label}</StatusPill>
          <span>{radio.replaced.summary}</span>
          {radio.replaced.otherwise && <span className="muted">{radio.replaced.otherwise}</span>}
        </dd>
      </dl>
      {(radio.replaced.steps?.length ?? 0) > 0 && (
        <ol className="ha-instructions">
          {radio.replaced.steps!.map((s, i) => (
            <li key={i}>{s}</li>
          ))}
        </ol>
      )}
      {noBackup && (radio.replaced.outcome === "restore" || data.length > 0) && (
        <div className="callout callout-warn" role="note">
          {c.title} has no finished backup to fall back on. Take one on Backups before swapping the radio.
          {handlers.onNavigate && (
            <div className="row ha-actions">
              <button type="button" className="btn btn-secondary btn-sm" onClick={() => handlers.onNavigate!("backups")}>
                Open backups
              </button>
            </div>
          )}
        </div>
      )}

      {state.filed ? (
        <div className="callout callout-ok" role="status">
          Plan <span className="ha-mono">{state.filed.id}</span> is filed. Nothing changes until you approve it on the Plans
          screen.
          {handlers.onNavigate && (
            <div className="row ha-actions">
              <button type="button" className="btn btn-primary btn-sm" onClick={() => handlers.onNavigate!("plans")}>
                Open Plans <ArrowRight size={14} aria-hidden />
              </button>
            </div>
          )}
        </div>
      ) : (
        <fieldset className="ha-fieldset ha-nested">
          <legend className="ha-legend-sub">New radio for {c.title}</legend>
          {radio.replacements.length === 0 ? (
            <div className="muted">
              No free {c.title} radio is plugged into {where}. Plug the new one in and check again.
              <div className="row ha-actions">
                <button type="button" className="btn btn-secondary btn-sm" disabled={handlers.loading} onClick={handlers.onRefresh}>
                  Check again
                </button>
              </div>
            </div>
          ) : (
            <div className="ha-options">
              {radio.replacements.map((rep) => {
                const r = inv.find((x) => x.id === rep.radioId);
                const fit = r?.suitableFor.find((s) => s.component === c.id)?.fit;
                const on = state.choice === rep.radioId;
                return (
                  <label key={rep.radioId} className={on ? "ha-option on" : "ha-option"}>
                    <input type="radio" name={fieldName} value={rep.radioId} checked={on} onChange={() => onChoose(rep.radioId)} />
                    <span className="ha-option-body">
                      <span className="ha-option-title">{r ? radioName(r) : rep.radioId}</span>
                      <span className="ha-option-text">
                        {radioIdentity(rep.radioId).text}.
                        {fit === "possible" && ` It did not say it is a ${c.title} radio: choose it only if you know it is one.`}{" "}
                        <span className="ha-mono">{rep.path}</span>
                      </span>
                    </span>
                  </label>
                );
              })}
            </div>
          )}
          {state.preview && (
            <details className="ha-preview" open>
              <summary>What the change does</summary>
              {state.preview.plan?.summary && <div className="muted">{state.preview.plan.summary}</div>}
              <PlanSteps plan={state.preview.plan} />
            </details>
          )}
          <ErrorLine error={state.error ?? null} />
          {radio.replacements.length > 0 && (
            <div className="row ha-actions">
              <button
                type="button"
                className="btn btn-secondary btn-sm"
                disabled={!state.choice || state.busy}
                onClick={onPreview}
              >
                Preview the change
              </button>
              <button
                type="button"
                className="btn btn-primary btn-sm"
                disabled={!state.choice || !state.preview || state.busy}
                onClick={onPropose}
              >
                {state.busy ? "Working…" : "Propose this change"}
              </button>
            </div>
          )}
          <div className="muted ha-hint-inline">
            Proposing only files the plan. {c.title} keeps its current radio until you approve it on the Plans screen.
          </div>
        </fieldset>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- removal

function Removal({
  c,
  cont,
  titles,
  handlers,
  client,
  onClose,
}: {
  c: HomeAutomationComponent;
  cont: HomeAutomationContinuity;
  titles: Map<HomeAutomationComponentId, string>;
  handlers: ActionHandlers;
  client: CareClient;
  onClose: () => void;
}) {
  const pkg = cont.package ?? c.package;
  const setting = !!cont.removal.setting;
  const preview = useLoad(
    async () => (setting || !pkg ? null : client.previewDisablePackage(pkg)),
    [pkg, setting],
  );
  const [busy, setBusy] = useState(false);
  const [filed, setFiled] = useState<PlanRecord | null>(null);
  const [error, setError] = useState<string | null>(null);
  return (
    <RemovalView
      c={c}
      cont={cont}
      titles={titles}
      handlers={handlers}
      preview={preview.data}
      previewError={preview.error}
      loading={preview.loading}
      busy={busy}
      filed={filed}
      error={error}
      onClose={onClose}
      onPropose={async () => {
        if (!pkg) return;
        setBusy(true);
        setError(null);
        try {
          setFiled(await client.disablePackage(pkg));
          handlers.onRefresh();
        } catch (e) {
          setError((e as Error).message);
        }
        setBusy(false);
      }}
    />
  );
}

// The removal panel, in the household's words, separated from its calls
// for the render check.
export function RemovalView({
  c,
  cont,
  titles,
  handlers,
  preview,
  previewError = null,
  loading = false,
  busy = false,
  filed = null,
  error = null,
  onPropose,
  onClose,
}: {
  c: HomeAutomationComponent;
  cont: HomeAutomationContinuity;
  titles: Map<HomeAutomationComponentId, string>;
  handlers: ActionHandlers;
  preview: PlanRecord | null;
  previewError?: string | null;
  loading?: boolean;
  busy?: boolean;
  filed?: PlanRecord | null;
  error?: string | null;
  onPropose: () => void;
  onClose: () => void;
}) {
  const r = cont.removal;
  const id = `ha-panel-remove-${c.id}`;
  const dependents = r.dependents.map((d) => titles.get(d) ?? d);

  if (r.setting) {
    return (
      <div className="ha-panel" id={id} role="region" aria-labelledby={`${id}-title`}>
        <h4 id={`${id}-title`} className="ha-legend-sub">
          Turning off {c.title}
        </h4>
        <p className="ha-setup-p">{r.consequence}</p>
        <div className="row ha-actions">
          {c.package && (
            <button type="button" className="btn btn-secondary btn-sm" onClick={() => handlers.onOpenPackage(c.package!)}>
              Open Home Assistant's settings
            </button>
          )}
          <button type="button" className="btn btn-ghost btn-sm" onClick={onClose}>
            Keep it
          </button>
        </div>
      </div>
    );
  }

  const { deletes, touchesData } = removalSteps(preview);
  const blocked = loading || !!previewError || touchesData || deletes.length === 0 || busy;
  const kept = cont.data.filter((d) => !d.repository).map((d) => d.volume);
  const keptCopies = cont.data.filter((d) => d.repository).map((d) => d.volume);
  const otherKept = r.keeps.filter((k) => !kept.includes(k) && !keptCopies.includes(k));

  return (
    <div className="ha-panel" id={id} role="region" aria-labelledby={`${id}-title`}>
      <h4 id={`${id}-title`} className="ha-legend-sub">
        Removing {c.title}
      </h4>
      {filed ? (
        <div className="callout callout-ok" role="status">
          Removal proposed as plan <span className="ha-mono">{filed.id}</span>. Nothing is removed until you approve it on the
          Plans screen.
          {handlers.onNavigate && (
            <div className="row ha-actions">
              <button type="button" className="btn btn-primary btn-sm" onClick={() => handlers.onNavigate!("plans")}>
                Open Plans <ArrowRight size={14} aria-hidden />
              </button>
            </div>
          )}
        </div>
      ) : (
        <>
          <p className="ha-setup-p">{r.consequence}</p>
          {dependents.length > 0 && (
            <div className="callout callout-warn" role="note">
              {dependents.join(" and ")} {dependents.length === 1 ? "needs" : "need"} {c.title}, and will stop working too.
            </div>
          )}
          <dl className="ha-facts">
            {r.stops.length > 0 && (
              <>
                <dt>Stops</dt>
                <dd>{r.stops.join(", ")}</dd>
              </>
            )}
            {(kept.length > 0 || keptCopies.length > 0 || otherKept.length > 0) && (
              <>
                <dt>Stays</dt>
                <dd>
                  {[
                    ...kept.map((v) => `${v}${cont.holds ? ` (${cont.holds})` : ""}`),
                    ...keptCopies.map((v) => `${v} (its backup copies)`),
                    ...otherKept,
                  ].join("; ")}
                </dd>
              </>
            )}
          </dl>
          {r.keeps.length > 0 && (
            <div className="muted ha-setup-p">
              Wildstone never deletes stored data. What {c.title} kept stays on {c.node ?? "the machine"}, and reinstalling
              picks it back up; delete it there yourself if you want the space.
            </div>
          )}
          {loading && <div className="muted">Working out exactly what the removal would delete…</div>}
          {previewError && (
            <div className="callout callout-bad" role="alert">
              Could not work out what removing {c.title} would delete, so it cannot be proposed from here: {previewError}
            </div>
          )}
          {touchesData && (
            <div className="callout callout-bad" role="alert">
              This removal would delete stored data, which Wildstone never does. It is not offered; open the service to look
              into it.
            </div>
          )}
          {!loading && !previewError && deletes.length > 0 && (
            <details className="ha-preview">
              <summary>
                Exactly what the plan removes ({deletes.length === 1 ? "1 item" : `${deletes.length} items`})
              </summary>
              <ul className="ha-evidence">
                {deletes.map((d) => (
                  <li key={`${d.kind}/${d.name}`} className="ha-mono">
                    {d.kind}/{d.name}
                  </li>
                ))}
              </ul>
            </details>
          )}
          {!loading && !previewError && deletes.length === 0 && (
            <div className="muted">Nothing is installed for {c.title} that a removal would take away.</div>
          )}
          <ErrorLine error={error} />
          <div className="row ha-actions">
            <button type="button" className="btn btn-danger btn-sm" disabled={blocked} onClick={onPropose}>
              {busy ? "Proposing…" : `Propose removing ${c.title}`}
            </button>
            <button type="button" className="btn btn-ghost btn-sm" onClick={onClose}>
              Keep it
            </button>
          </div>
          <div className="muted ha-hint-inline">Proposing only files the plan. Nothing is removed until you approve it.</div>
        </>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- add

// Adding a kind of device later goes straight to that slice: the
// readiness check (on a machine Wildstone runs Home Assistant on) or the
// setup sequence, asked about only what is being added, on the machine
// the rest already runs on. What is installed reads as done there.
export function addGoalDraft(draft: Draft, status: HomeAutomationStatus, topology: "managed" | "external", g: Goal): Draft {
  const node = status.components.find((c) => c.selection === "installed" && c.node)?.node ?? status.node;
  return {
    ...draft,
    topology,
    node: draft.node ?? node,
    goals: [g],
    step: topology === "managed" ? "readiness" : "setup",
  };
}

function AddMore({
  status,
  draft,
  topology,
  onDraft,
}: {
  status: HomeAutomationStatus;
  draft: Draft;
  topology: "managed" | "external";
  onDraft: (d: Draft) => void;
}) {
  const comps = new Map(status.components.map((c) => [c.id, c]));
  const addable = GOALS.filter((g) => {
    const parts = goalComponents(g).map((id) => comps.get(id));
    if (parts.every((c) => c && present(c))) return false;
    return !parts.some((c) => c && c.state === "unavailable" && !present(c));
  });
  const add = (g: Goal) => onDraft(addGoalDraft(draft, status, topology, g));
  return (
    <div className="card ha-card">
      <h3>Add more</h3>
      {addable.length === 0 ? (
        <div className="muted">Every kind of device this catalog can set up is installed or on its way.</div>
      ) : (
        <>
          <div className="muted">Adding one checks only what it needs, and proposes only its own plans.</div>
          <div className="row ha-actions">
            {addable.map((g) => (
              <button key={g} type="button" className="btn btn-secondary btn-sm" onClick={() => add(g)}>
                <Plus size={14} aria-hidden /> Add {GOAL_COPY[g].title}
              </button>
            ))}
          </div>
        </>
      )}
      <div className="row ha-actions">
        <button type="button" className="btn btn-ghost btn-sm" onClick={() => onDraft({ ...draft, topology, step: "add" })}>
          See every choice
        </button>
      </div>
    </div>
  );
}
