import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ArrowRight } from "@phosphor-icons/react";
import {
  api,
  ApiError,
  BluetoothMode,
  HomeAutomationAction,
  HomeAutomationChecklistStep,
  HomeAutomationComponentId,
  HomeAutomationPlanAnswer,
  HomeAutomationPlanRequest,
  HomeAutomationPlanStep,
  HomeAutomationStatus,
  HomeAutomationTopology,
} from "../api";
import { ago, ErrorLine } from "../lib";
import { useConsoleState } from "../state";
import { Kind, StatusPill } from "../ui/status";
import { PlanSteps } from "./Plans";
import {
  ActionHandlers,
  BLUETOOTH_COPY,
  BlockerCard,
  Draft,
  evidenceLine,
  queryOf,
  radioName,
  selectedNode,
  selectedRadios,
} from "./HomeAutomation";

// The setup sequence (#450, epic #431): the household's choices driven
// through POST /api/v1/home-automation/preview and /propose (#447), one
// package plan at a time, in the order things depend on each other.
//
// The orchestration stores nothing and this screen stores only choices.
// Every answer is rebuilt by the server from installed resources, plan
// records and observations, so a reload, a closed Console or a rebooted
// host resumes from what is true: the screen re-asks on load, after every
// action, when the tab comes back into view, when the shell's plan poll
// sees the step's plan change, and on a modest timer while something is
// waiting on approval, apply or observation.
//
// Two lines it never crosses. It files plans and never approves one:
// approval is the Plans screen's, and the only thing here about it is a
// way there. And it never says a thing is installed until the server
// does: proposing, approving or applying changes nothing on this page
// until the next answer says so.
//
// Steps in Home Assistant itself (the MQTT, Matter, Thread and Z-Wave
// integrations, the companion app's Thread credentials) are outside
// what Wildstone can see. They are checklist items with the server's
// exact instructions; the person ticks each one off, the tick is kept in
// the draft and sent as confirmedSteps, it reads as "you confirmed"
// rather than verified, and it can be unticked.

// What the screen holds about the sequence. Exported so the render check
// can put the step in each state.
export interface SetupState {
  answer: HomeAutomationPlanAnswer | null;
  error?: string | null;
  loading?: boolean;
  busy?: boolean;
  notice?: SetupNotice | null;
}

// What the last action did, said once beside the answer it produced.
export type SetupNotice =
  | { kind: "filed"; planId?: string; summary?: string }
  | { kind: "moved"; message: string }
  | { kind: "refused"; message: string };

// The orchestration calls, as an interface so the render check drives
// the same functions the screen does against a fake Keystone.
export interface SetupClient {
  homeAutomationPreview: (req: HomeAutomationPlanRequest) => Promise<HomeAutomationPlanAnswer>;
  homeAutomationPropose: (req: HomeAutomationPlanRequest) => Promise<HomeAutomationPlanAnswer>;
}

// The request for this draft: the same goals the aggregate is asked
// about, the radios the readiness step resolved (by inventory id, never
// a path), the Bluetooth mode, typed options and the steps the person
// confirmed. Sent whole on every call, because nothing is stored.
export function planRequestOf(
  draft: Draft,
  status: HomeAutomationStatus,
  topology: HomeAutomationTopology,
): HomeAutomationPlanRequest {
  const q = queryOf({ ...draft, topology });
  const req: HomeAutomationPlanRequest = { topology, select: q.select };
  const node = draft.node ?? status.node;
  if (node) req.node = node;
  const radios = selectedRadios(draft, status);
  if (Object.keys(radios).length) req.radios = radios;
  if (q.bluetooth) req.modes = { bluetooth: q.bluetooth };
  if (draft.options && Object.keys(draft.options).length) req.options = draft.options;
  if (draft.confirmedSteps?.length) req.confirmedSteps = [...draft.confirmedSteps].sort();
  return req;
}

export function stepKey(es: { component: HomeAutomationComponentId; id: string }): string {
  return `${es.component}/${es.id}`;
}

function isAnswer(v: unknown): v is HomeAutomationPlanAnswer {
  return !!v && typeof v === "object" && Array.isArray((v as HomeAutomationPlanAnswer).sequence) &&
    !!(v as HomeAutomationPlanAnswer).next;
}

export function isPackageStep(s: HomeAutomationPlanStep): boolean {
  return s.action === "install_package" || s.action === "reconfigure_package";
}

// Ask where things stand. A 409 is still an answer — a refused radio
// choice comes back with the full, fresh sequence and `rejected` set.
export async function fetchPreview(
  client: SetupClient,
  req: HomeAutomationPlanRequest,
): Promise<{ answer?: HomeAutomationPlanAnswer; error?: string }> {
  try {
    return { answer: await client.homeAutomationPreview(req) };
  } catch (e) {
    if (e instanceof ApiError && e.status === 409 && isAnswer(e.body)) return { answer: e.body };
    return { error: (e as Error).message };
  }
}

// File the step the person was looking at, and only that one: the
// previewed step's id goes as expectStep, so when the sequence moved on
// since (another tab proposed it, a plan was approved or failed, a radio
// was unplugged) nothing is filed and the fresh answer comes back.
export async function proposeStep(
  client: SetupClient,
  req: HomeAutomationPlanRequest,
  previewed: HomeAutomationPlanAnswer,
): Promise<{ answer?: HomeAutomationPlanAnswer; notice: SetupNotice }> {
  if (!isPackageStep(previewed.next)) {
    return { notice: { kind: "refused", message: "There is no plan to propose at this step." } };
  }
  try {
    const answer = await client.homeAutomationPropose({ ...req, expectStep: previewed.next.id });
    return { answer, notice: { kind: "filed", planId: answer.plan?.id, summary: answer.plan?.summary } };
  } catch (e) {
    if (e instanceof ApiError && e.status === 409 && isAnswer(e.body)) {
      return { answer: e.body, notice: { kind: "moved", message: e.message } };
    }
    return { notice: { kind: "refused", message: (e as Error).message } };
  }
}

// The draft with an outside step ticked or unticked.
export function withConfirmed(draft: Draft, key: string, on: boolean): Draft {
  const next = new Set(draft.confirmedSteps ?? []);
  if (on) next.add(key);
  else next.delete(key);
  return { ...draft, confirmedSteps: next.size ? [...next].sort() : undefined };
}

// The draft with a typed package param set, or cleared when empty.
export function withOption(draft: Draft, comp: HomeAutomationComponentId, name: string, value: string): Draft {
  const options = { ...draft.options };
  const params = { ...options[comp] };
  if (value) params[name] = value;
  else delete params[name];
  if (Object.keys(params).length) options[comp] = params;
  else delete options[comp];
  return { ...draft, options: Object.keys(options).length ? options : undefined };
}

// How soon to ask again on its own, or never: often while something is
// moving that the person is not doing (an apply, a first healthy
// observation), less often while it waits on the person (approval, an
// observable step outside Wildstone), and not at all otherwise.
export function pollDelay(a: HomeAutomationPlanAnswer): number | null {
  switch (a.next.action) {
    case "await_apply":
    case "await_observation":
      return 5_000;
    case "await_approval":
      return 15_000;
    case "external_step":
      return (a.next.externalSteps ?? []).some((s) => s.status === "pending") ? 15_000 : null;
  }
  return null;
}

// A step's standing in words. Distinct for each thing the household has
// to tell apart: waiting on THEM (approval), waiting on Wildstone
// (applying, first observation), waiting on Home Assistant (a step
// outside), something wrong, and set up.
export function stepStanding(s: HomeAutomationPlanStep): { kind: Kind; label: string } {
  switch (s.action) {
    case "done":
      return { kind: "ok", label: "Installed" };
    case "install_package":
      return s.status === "current" ? { kind: "mute", label: "Ready to propose" } : { kind: "mute", label: "Not installed yet" };
    case "reconfigure_package":
      return s.status === "current" ? { kind: "mute", label: "A change is ready to propose" } : { kind: "mute", label: "Needs a change" };
    case "await_approval":
      return { kind: "warn", label: "Waiting for your approval" };
    case "await_apply":
      return { kind: "warn", label: "Applying" };
    case "await_observation":
      return { kind: "warn", label: "Applied, waiting to see it running" };
    case "external_step":
      return { kind: "warn", label: "Steps left in Home Assistant" };
    case "resolve_blockers":
      if (s.blockers.some((b) => b.code === "plan_failed")) return { kind: "bad", label: "Plan failed" };
      if (s.state === "degraded") return { kind: "bad", label: "Needs attention" };
      return { kind: "bad", label: "Blocked" };
  }
  return { kind: "mute", label: s.action };
}

// An outside step's standing. "You confirmed" is never "Verified": one
// is the person's word, the other is something Wildstone saw.
export function externalStanding(
  es: HomeAutomationChecklistStep,
  confirmedByYou: boolean,
  componentTitle: string,
): { kind: Kind; label: string } {
  if (es.status === "done") return { kind: "ok", label: "Verified by Wildstone" };
  if (es.status === "unverified" && confirmedByYou) return { kind: "ok", label: "You confirmed" };
  if (!es.due) return { kind: "mute", label: `After ${componentTitle} is installed` };
  if (es.status === "pending") return { kind: "warn", label: "Waiting for Wildstone to see it" };
  return { kind: "warn", label: "To do in Home Assistant" };
}

export function SetupStep({
  status,
  topology,
  draft,
  onDraft,
  handlers,
  setup,
}: {
  status: HomeAutomationStatus;
  topology: HomeAutomationTopology;
  draft: Draft;
  onDraft: (d: Draft) => void;
  handlers: ActionHandlers;
  setup?: SetupState;
}) {
  if (setup) {
    return (
      <SetupView
        status={status}
        draft={draft}
        onDraft={onDraft}
        handlers={handlers}
        state={setup}
        onPropose={() => {}}
        onRecheck={handlers.onRefresh}
      />
    );
  }
  return <SetupLive status={status} topology={topology} draft={draft} onDraft={onDraft} handlers={handlers} />;
}

function SetupLive({
  status,
  topology,
  draft,
  onDraft,
  handlers,
}: {
  status: HomeAutomationStatus;
  topology: HomeAutomationTopology;
  draft: Draft;
  onDraft: (d: Draft) => void;
  handlers: ActionHandlers;
}) {
  const req = useMemo(() => planRequestOf(draft, status, topology), [draft, status, topology]);
  const key = JSON.stringify(req);
  const [answer, setAnswer] = useState<HomeAutomationPlanAnswer | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<SetupNotice | null>(null);
  // Only the newest question's answer is shown: a slow reply to an
  // older draft never overwrites a newer one.
  const asked = useRef(0);

  const refresh = useCallback(async () => {
    const n = ++asked.current;
    setLoading(true);
    const r = await fetchPreview(api, req);
    if (n !== asked.current) return;
    setLoading(false);
    if (r.answer) {
      setAnswer(r.answer);
      setError(null);
    } else {
      setError(r.error ?? "The setup could not be checked.");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Back in view (another tab approved the plan, the laptop woke up):
  // ask again rather than show what was true when it was left.
  useEffect(() => {
    const again = () => {
      if (typeof document === "undefined" || document.visibilityState === "visible") void refresh();
    };
    window.addEventListener("focus", again);
    document.addEventListener("visibilitychange", again);
    return () => {
      window.removeEventListener("focus", again);
      document.removeEventListener("visibilitychange", again);
    };
  }, [refresh]);

  const delay = answer ? pollDelay(answer) : null;
  useEffect(() => {
    if (delay === null) return;
    const t = setTimeout(() => void refresh(), delay);
    return () => clearTimeout(t);
  }, [delay, answer, refresh]);

  // The shell already polls the plan list. When the plan this step is
  // waiting on changes state there (approved, applied, rejected), ask
  // straight away rather than at the next tick.
  const { snap } = useConsoleState();
  const planId = answer?.next.planId;
  const planState = planId && snap ? (snap.plans.find((p) => p.id === planId)?.state ?? "gone") : "";
  const seenPlan = useRef(planState);
  useEffect(() => {
    const before = seenPlan.current;
    seenPlan.current = planState;
    // Only a change the shell SAW, not the step first appearing.
    if (before && planState && before !== planState) void refresh();
  }, [planState, refresh]);

  // When the sequence moves on, the aggregate behind the rest of the
  // screen ("Set up now") is stale too; when a radio choice was refused,
  // the inventory the picks came from is. Ask for both again.
  const nextId = answer?.next.id;
  const rejected = answer?.rejected?.map((b) => b.code).join() ?? "";
  const seenNext = useRef(nextId);
  useEffect(() => {
    if (seenNext.current !== undefined && seenNext.current !== nextId) handlers.onRefresh();
    seenNext.current = nextId;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nextId]);
  useEffect(() => {
    if (rejected) handlers.onRefresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rejected]);

  const onPropose = async () => {
    if (!answer || busy) return;
    setBusy(true);
    const r = await proposeStep(api, req, answer);
    setNotice(r.notice);
    if (r.answer) setAnswer(r.answer);
    setBusy(false);
    // Whatever propose said, the next screen is the server's fresh
    // answer, not this one's guess.
    await refresh();
    handlers.onRefresh();
  };

  return (
    <SetupView
      status={status}
      draft={draft}
      onDraft={onDraft}
      handlers={handlers}
      state={{ answer, error, loading: loading || handlers.loading, busy, notice }}
      onPropose={onPropose}
      onRecheck={() => {
        handlers.onRefresh();
        void refresh();
      }}
    />
  );
}

// The setup step, separated from its fetching so the render check can
// put it in every state.
export function SetupView({
  status,
  draft,
  onDraft,
  handlers,
  state,
  onPropose,
  onRecheck,
}: {
  status: HomeAutomationStatus;
  draft: Draft;
  onDraft: (d: Draft) => void;
  handlers: ActionHandlers;
  state: SetupState;
  onPropose: () => void;
  onRecheck: () => void;
}) {
  const { answer, error, loading = false, busy = false, notice } = state;
  const titles = new Map(status.components.map((c) => [c.id, c.title]));
  const title = (id?: HomeAutomationComponentId) => (id && titles.get(id)) || id || "";
  const confirmed = new Set(draft.confirmedSteps ?? []);

  const setConfirmed = (k: string, on: boolean) => onDraft(withConfirmed(draft, k, on));
  const setOption = (comp: HomeAutomationComponentId, name: string, value: string) =>
    onDraft(withOption(draft, comp, name, value));
  const setMode = (m: BluetoothMode) => onDraft({ ...draft, bluetooth: m });
  const recheck: ActionHandlers = { ...handlers, onRefresh: onRecheck, loading };

  return (
    <section className="ha-setup" aria-labelledby="ha-setup-title">
      <div className="sect">
        <h2 id="ha-setup-title" className="ha-legend">
          Set up, one plan at a time
        </h2>
        <span className="fade" />
        <span className="muted ha-checked" aria-live="polite">
          {loading ? "Checking…" : answer ? `Checked ${ago(answer.generatedAt)}` : ""}
        </span>
        <button type="button" className="btn btn-ghost" disabled={loading} onClick={onRecheck}>
          Check again
        </button>
      </div>
      <div className="muted ha-setup-lead">
        Wildstone proposes one plan at a time, in the order things depend on each other. You approve each one on the
        Plans screen; this page never approves anything, and shows a part as installed only once Wildstone sees it
        running.
      </div>

      {error && (
        <div className="ha-setup-error">
          <ErrorLine error={error} />
          {draft.options && (
            <button type="button" className="btn btn-ghost btn-sm" onClick={() => onDraft({ ...draft, options: undefined })}>
              Clear the values you typed
            </button>
          )}
        </div>
      )}
      <NoticeLine notice={notice ?? null} handlers={handlers} />

      {!answer ? (
        !error && <div className="muted">Working out the next step…</div>
      ) : (
        <>
          {(answer.rejected ?? []).map((b, i) => (
            <BlockerCard key={`rej-${b.code}-${i}`} b={b} handlers={recheck} />
          ))}
          <NextCard
            answer={answer}
            status={status}
            draft={draft}
            busy={busy}
            title={title}
            handlers={recheck}
            onPropose={onPropose}
            onOption={setOption}
            onMode={setMode}
          />
          <Sequence answer={answer} />
          <Checklist
            steps={answer.externalSteps}
            confirmed={confirmed}
            title={title}
            loading={loading}
            onConfirm={setConfirmed}
            handlers={recheck}
          />
        </>
      )}

      <div className="row">
        <button type="button" className="btn btn-secondary" onClick={handlers.onBack}>
          Back
        </button>
      </div>
    </section>
  );
}

function NoticeLine({ notice, handlers }: { notice: SetupNotice | null; handlers: ActionHandlers }) {
  if (!notice) return null;
  switch (notice.kind) {
    case "filed":
      return (
        <div className="callout callout-ok" role="status">
          Plan {notice.planId ? <span className="ha-mono">{notice.planId}</span> : null} is filed
          {notice.summary ? `: ${notice.summary}` : ""}. Nothing changes until you approve it on the Plans screen.
        </div>
      );
    case "moved":
      return (
        <div className="callout callout-warn" role="status">
          Nothing was proposed: the next step changed since this page last checked ({notice.message}). Below is where
          things stand now.
        </div>
      );
    case "refused":
      return (
        <div className="callout callout-bad" role="alert">
          Nothing was proposed: {notice.message}
          {handlers.onNavigate && (
            <>
              {" "}
              <button type="button" className="btn btn-ghost btn-sm" onClick={() => handlers.onNavigate!("plans")}>
                Open Plans
              </button>
            </>
          )}
        </div>
      );
  }
}

function NextCard({
  answer,
  status,
  draft,
  busy,
  title,
  handlers,
  onPropose,
  onOption,
  onMode,
}: {
  answer: HomeAutomationPlanAnswer;
  status: HomeAutomationStatus;
  draft: Draft;
  busy: boolean;
  title: (id?: HomeAutomationComponentId) => string;
  handlers: ActionHandlers;
  onPropose: () => void;
  onOption: (comp: HomeAutomationComponentId, name: string, value: string) => void;
  onMode: (m: BluetoothMode) => void;
}) {
  const next = answer.next;
  const standing = answer.complete
    ? { kind: "ok" as Kind, label: "Set up" }
    : next.action === "done"
      ? { kind: "warn" as Kind, label: "Not finished" }
      : stepStanding(next);
  const toPlans = handlers.onNavigate && (
    <button type="button" className="btn btn-primary" onClick={() => handlers.onNavigate!("plans")}>
      Open Plans to review and approve <ArrowRight size={14} aria-hidden />
    </button>
  );

  return (
    <div className="card ha-card ha-next-step" aria-labelledby="ha-next-title" aria-live="polite">
      <div className="ha-readiness-row">
        <h3 id="ha-next-title">{answer.complete ? "Home automation is set up" : `Next: ${next.title}`}</h3>
        <StatusPill kind={standing.kind}>{standing.label}</StatusPill>
      </div>
      <div className="muted">{next.reason}</div>

      {isPackageStep(next) && (
        <PackageStep answer={answer} status={status} draft={draft} busy={busy} title={title} onPropose={onPropose} handlers={handlers} />
      )}

      {next.action === "await_approval" && (
        <>
          <p className="ha-setup-p">
            Plan <span className="ha-mono">{next.planId}</span> is waiting for your approval on the Plans screen. Review
            what it changes there and approve or reject it; this page notices on its own and moves on once Wildstone
            sees the result.
          </p>
          {toPlans && <div className="row">{toPlans}</div>}
        </>
      )}
      {next.action === "await_apply" && (
        <p className="ha-setup-p">
          Plan <span className="ha-mono">{next.planId}</span> is approved and being applied
          {next.node ? ` on ${next.node}` : ""}. Nothing more to do until it finishes.
        </p>
      )}
      {next.action === "await_observation" && (
        <p className="ha-setup-p">
          The plan is applied. Wildstone is waiting to see {next.title} running and healthy
          {next.node ? ` on ${next.node}` : ""} before going on; a restarted machine is picked up the same way.
        </p>
      )}

      {next.action === "resolve_blockers" && (
        <>
          {next.blockers.map((b, i) => (
            <div key={`${b.code}-${i}`}>
              <BlockerCard b={b} handlers={handlers} />
              {b.actions
                .filter((a) => a.kind === "provide_option" || a.kind === "choose_mode")
                .map((a, j) => (
                  <ActionField key={j} a={a} draft={draft} title={title} onOption={onOption} onMode={onMode} />
                ))}
            </div>
          ))}
          {next.planId && toPlans && <div className="row">{toPlans}</div>}
        </>
      )}

      {next.action === "external_step" && (
        <p className="ha-setup-p">
          Every package is installed. Finish the steps below in Home Assistant; the setup is done only once each one is
          seen or confirmed.
        </p>
      )}

      {answer.complete && (
        <div className="callout callout-ok" role="status">
          Every part you chose is installed and healthy, and every step in Home Assistant is done.
        </div>
      )}
    </div>
  );
}

function PackageStep({
  answer,
  status,
  draft,
  busy,
  title,
  onPropose,
  handlers,
}: {
  answer: HomeAutomationPlanAnswer;
  status: HomeAutomationStatus;
  draft: Draft;
  busy: boolean;
  title: (id?: HomeAutomationComponentId) => string;
  onPropose: () => void;
  handlers: ActionHandlers;
}) {
  const next = answer.next;
  const radio = next.radioId
    ? selectedNode(status)?.radioInventory?.radios.find((r) => r.id === next.radioId)
    : undefined;
  // A hand-typed path cannot go through the guided setup: the server
  // only assigns radios it can see. Proposing anyway could hand the
  // package a different radio than the one the person typed.
  const manual = next.component ? draft.manualRadios?.[next.component] : undefined;
  const params = Object.entries(next.params ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const preview = answer.preview;
  return (
    <>
      <dl className="ha-facts">
        {next.package && (
          <>
            <dt>Package</dt>
            <dd className="ha-mono">
              {next.package}
              {next.version ? ` ${next.version}` : ""}
            </dd>
          </>
        )}
        {next.node && (
          <>
            <dt>Machine</dt>
            <dd>{next.node}</dd>
          </>
        )}
        {next.radioId && (
          <>
            <dt>Radio</dt>
            <dd>{radio ? radioName(radio) : <span className="ha-mono">{next.radioId}</span>}</dd>
          </>
        )}
      </dl>
      {params.length > 0 && (
        <table className="ha-table">
          <caption className="ha-caption">Settings the plan uses</caption>
          <tbody>
            {params.map(([k, v]) => (
              <tr key={k}>
                <th scope="row" className="ha-mono">
                  {k}
                </th>
                <td className="ha-mono">{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {preview && (
        <details className="ha-preview">
          <summary>What the plan would change</summary>
          {preview.plan?.summary && <div className="muted">{preview.plan.summary}</div>}
          <PlanSteps plan={preview.plan} />
          {(preview.providedSecrets?.length ?? 0) > 0 && (
            <div className="muted ha-setup-p">Wildstone creates: {preview.providedSecrets.join(", ")}.</div>
          )}
        </details>
      )}
      {(preview?.blockers ?? []).map((b, i) => (
        <div key={i} className="callout callout-warn" role="note">
          {b}
        </div>
      ))}
      {manual && (
        <div className="callout callout-warn" role="note">
          You typed the device path <span className="ha-mono">{manual}</span> for {title(next.component)}. The guided setup
          only uses radios Wildstone can see, so set {title(next.component)} up from its package form with that path, or
          clear it and choose a listed radio.
          <div className="row ha-actions">
            {next.package && (
              <button type="button" className="btn btn-secondary btn-sm" onClick={() => handlers.onOpenPackage(next.package!)}>
                Open the package form
              </button>
            )}
            {handlers.onChooseRadio && (
              <button type="button" className="btn btn-secondary btn-sm" onClick={() => handlers.onChooseRadio!(next.component)}>
                Choose a listed radio
              </button>
            )}
          </div>
        </div>
      )}
      <div className="row">
        <button type="button" className="btn btn-primary" disabled={busy || !!manual} onClick={onPropose}>
          {busy ? "Proposing…" : next.action === "reconfigure_package" ? "Propose this change" : "Propose this plan"}
        </button>
      </div>
      <div className="muted ha-hint-inline">
        Proposing only files the plan. Nothing changes on {next.node ?? "the machine"} until you approve it on the Plans
        screen.
      </div>
    </>
  );
}

// The answer to a choose_mode or provide_option remedy, typed into the
// draft. A value is kept as typed and sent on the next check; the
// server decides whether it does.
function ActionField({
  a,
  draft,
  title,
  onOption,
  onMode,
}: {
  a: HomeAutomationAction;
  draft: Draft;
  title: (id?: HomeAutomationComponentId) => string;
  onOption: (comp: HomeAutomationComponentId, name: string, value: string) => void;
  onMode: (m: BluetoothMode) => void;
}) {
  if (a.kind === "choose_mode") {
    if (a.component !== "bluetooth") return null;
    const name = `ha-mode-${a.component}`;
    return (
      <fieldset className="ha-fieldset ha-nested ha-action-field">
        <legend className="ha-legend-sub">{a.label}</legend>
        <div className="ha-options">
          {(["host-bluez", "proxy"] as BluetoothMode[]).map((m) => (
            <label key={m} className={draft.bluetooth === m ? "ha-option on" : "ha-option"}>
              <input type="radio" name={name} value={m} checked={draft.bluetooth === m} onChange={() => onMode(m)} />
              <span className="ha-option-body">
                <span className="ha-option-title">{BLUETOOTH_COPY[m].title}</span>
                <span className="ha-option-text">{BLUETOOTH_COPY[m].body}</span>
              </span>
            </label>
          ))}
        </div>
      </fieldset>
    );
  }
  if (!a.component) return null;
  const comp = a.component;
  return (
    <div className="ha-action-field">
      {Object.entries(a.params ?? {}).map(([name, example]) => (
        <OptionInput
          key={name}
          comp={comp}
          name={name}
          example={example}
          help={a.instructions?.join(" ")}
          label={`${title(comp)}: ${name}`}
          value={draft.options?.[comp]?.[name] ?? ""}
          onSave={(v) => onOption(comp, name, v)}
        />
      ))}
    </div>
  );
}

// Typed locally and saved on purpose, so the server is not asked about
// every keystroke.
function OptionInput({
  comp,
  name,
  example,
  help,
  label,
  value,
  onSave,
}: {
  comp: string;
  name: string;
  example?: string;
  help?: string;
  label: string;
  value: string;
  onSave: (v: string) => void;
}) {
  const [text, setText] = useState(value);
  useEffect(() => setText(value), [value]);
  const id = `ha-opt-${comp}-${name}`;
  return (
    <form
      className="ha-manual-panel"
      onSubmit={(e) => {
        e.preventDefault();
        onSave(text.trim());
      }}
    >
      <label htmlFor={id} className="ha-legend-sub">
        {label}
      </label>
      {help && (
        <div id={`${id}-help`} className="muted ha-hint-inline">
          {help}
        </div>
      )}
      <div className="row">
        <input
          id={id}
          className="input"
          type="text"
          spellCheck={false}
          autoComplete="off"
          placeholder={example}
          value={text}
          aria-describedby={help ? `${id}-help` : undefined}
          onChange={(e) => setText(e.target.value)}
        />
        <button type="submit" className="btn btn-secondary btn-sm" disabled={text.trim() === value}>
          Use this value
        </button>
      </div>
    </form>
  );
}

function Sequence({ answer }: { answer: HomeAutomationPlanAnswer }) {
  if (answer.sequence.length === 0) return null;
  return (
    <>
      <h3 className="ha-legend ha-sub-head">In order</h3>
      <ol className="ha-list ha-sequence">
        {answer.sequence.map((s, i) => {
          const st = stepStanding(s);
          return (
            <li
              key={s.id}
              id={s.component ? `ha-comp-${s.component}` : undefined}
              className={s.status === "current" ? "ha-seq-item current" : "ha-seq-item"}
              aria-current={s.status === "current" ? "step" : undefined}
            >
              <div className="ha-readiness-row">
                <span className="ha-option-title">
                  <span className="ha-seq-n" aria-hidden>
                    {i + 1}.
                  </span>{" "}
                  {s.title}
                  {s.status === "current" && <span className="muted"> (now)</span>}
                </span>
                <StatusPill kind={st.kind} title={s.reason}>
                  {st.label}
                </StatusPill>
              </div>
              <div className="muted">
                {s.reason}
                {s.planId && (
                  <>
                    {" "}
                    Plan <span className="ha-mono">{s.planId}</span>.
                  </>
                )}
              </div>
            </li>
          );
        })}
      </ol>
    </>
  );
}

function Checklist({
  steps,
  confirmed,
  title,
  loading,
  onConfirm,
  handlers,
}: {
  steps: HomeAutomationChecklistStep[];
  confirmed: Set<string>;
  title: (id?: HomeAutomationComponentId) => string;
  loading: boolean;
  onConfirm: (key: string, on: boolean) => void;
  handlers: ActionHandlers;
}) {
  if (steps.length === 0) return null;
  return (
    <>
      <h3 className="ha-legend ha-sub-head" id="ha-outside">
        In Home Assistant
      </h3>
      <div className="muted ha-hint">
        Wildstone cannot see inside Home Assistant. Tick each step once you have done it; the tick is your word, kept in
        this browser, and you can untick it.
      </div>
      <ul className="ha-list ha-readiness-list">
        {steps.map((es) => {
          const k = stepKey(es);
          const mine = confirmed.has(k);
          const st = externalStanding(es, mine, title(es.component));
          const cid = `ha-ext-${es.component}-${es.id}`;
          return (
            <li key={k} id={cid}>
              <div className="ha-readiness-row">
                <span className="ha-option-title">
                  {es.title} <span className="muted">· {title(es.component)}</span>
                </span>
                <StatusPill kind={st.kind}>{st.label}</StatusPill>
              </div>
              <ol className="ha-instructions">
                {es.instructions.map((line, j) => (
                  <li key={j}>{line}</li>
                ))}
              </ol>
              {(es.evidence ?? []).length > 0 && (
                <ul className="ha-evidence" aria-label="What Wildstone sees">
                  {es.evidence!.map((e, j) => (
                    <li key={j}>{evidenceLine(e)}</li>
                  ))}
                </ul>
              )}
              {es.status === "unverified" && (
                <label className={mine ? "ha-confirm on" : "ha-confirm"}>
                  <input
                    type="checkbox"
                    checked={mine}
                    // Not yet: the part it configures is not installed.
                    // Untick is always allowed.
                    disabled={!es.due && !mine}
                    onChange={(e) => onConfirm(k, e.target.checked)}
                  />
                  <span>I have done this in Home Assistant</span>
                </label>
              )}
              {es.status === "pending" && es.due && (
                <div className="row ha-actions">
                  {es.action && <ExternalAction a={es.action} handlers={handlers} />}
                  <button type="button" className="btn btn-ghost btn-sm" disabled={loading} onClick={handlers.onRefresh}>
                    Check again
                  </button>
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </>
  );
}

// An observable outside step's own remedy (the publisher credential):
// open where it is done. Nothing here does it in place.
function ExternalAction({ a, handlers }: { a: HomeAutomationAction; handlers: ActionHandlers }) {
  if (!a.package && !a.capability) return null;
  return (
    <button
      type="button"
      className="btn btn-secondary btn-sm"
      onClick={() => (a.package ? handlers.onOpenPackage(a.package) : handlers.onOpenServices())}
    >
      {a.label}
    </button>
  );
}
