import { useState } from "react";
import { LockSimple, Plus } from "@phosphor-icons/react";
import { api, type HouseholdRoutine } from "../api";
import type { AutomationSpec } from "../generated/resources";
import { ago, ErrorLine, useAction, useLoad } from "../lib";
import { useConsoleState } from "../state";
import { Kind, StatusPill } from "../ui/status";
import { RoutineDetail } from "./RoutineDetail";
import { RoutineDryRun } from "./RoutineDryRun";
import { RoutineEdit } from "./RoutineEdit";

// What the house does on its own (ADR-W024, ADR-W030 A6).
//
// The list works hard on two distinctions. Native versus Wildstone — one
// runs inside an application and Wildstone only knows about it, the
// other Wildstone runs — because offering to edit something you cannot
// is worse than not showing it. That distinction used to be a sentence
// per row and is now structural: two sections, and only one of them
// opens.
//
// And trial versus enabled: a trial routine does everything except act,
// which is the whole point of watching one for a week before letting it
// loose.
//
// Model routes used to live on this screen. They are a property of a
// Butler (ADR-W031 A1/A2) and `views/Butlers.tsx` renders them, so they
// are gone from here.

export const ROUTINE_TONE: Record<HouseholdRoutine["state"], Kind> = {
  draft: "mute",
  trial: "warn",
  enabled: "ok",
  disabled: "bad",
};

export const ROUTINE_MEANING: Record<HouseholdRoutine["state"], string> = {
  draft: "written, not running",
  trial: "running and watching — it does everything except act",
  enabled: "running, and it acts",
  disabled: "stopped",
};

// The screen is a small state machine rather than a route. The shell has
// no router and one screen should not introduce one.
export type Mode =
  | { kind: "list" }
  | { kind: "detail"; name: string }
  | { kind: "edit"; name: string }
  // A dry-run needs the draft to replay and the name the engine knows
  // the rule by. A null spec means "replay what is stored" — the detail
  // screen's button, which has changed nothing.
  | { kind: "dryrun"; name: string; spec: AutomationSpec | null; rule: string };

type StateFilter = "all" | HouseholdRoutine["state"];
type Audience = "admin" | "household";

const STATES: HouseholdRoutine["state"][] = ["draft", "trial", "enabled", "disabled"];

// openRoutine lets another screen land on one routine rather than on the
// list — the household stream's "Open routine", which would otherwise
// drop someone on a list and make them find it again.
export function Routines({ openRoutine }: { openRoutine?: string }) {
  const [mode, setMode] = useState<Mode>(
    openRoutine ? { kind: "detail", name: openRoutine } : { kind: "list" },
  );
  const routines = useLoad(() => api.householdRoutines());

  switch (mode.kind) {
    case "detail":
      return <RoutineDetail name={mode.name} onMode={setMode} onChanged={routines.reload} />;
    case "edit":
      return <RoutineEdit name={mode.name} onMode={setMode} onSaved={routines.reload} />;
    case "dryrun":
      return <RoutineDryRun name={mode.name} spec={mode.spec} rule={mode.rule} onMode={setMode} />;
    default:
      return <RoutineList load={routines} onMode={setMode} />;
  }
}

export function RoutineList({
  load,
  onMode,
}: {
  load: ReturnType<typeof useLoad<{ routines: HouseholdRoutine[] | null; specs?: Record<string, AutomationSpec> }>>;
  onMode: (m: Mode) => void;
}) {
  const [q, setQ] = useState("");
  const [state, setState] = useState<StateFilter>("all");
  const [audience, setAudience] = useState<Audience>("admin");
  const [writing, setWriting] = useState(false);
  const [stopReason,setStopReason] = useState("");
  const executionStop=useLoad(()=>api.householdExecutionStop());
  const stopAction=useAction();

  const rows = load.data?.routines ?? [];
  const specs = load.data?.specs ?? {};
  const isAdmin = audience === "admin";

  // Filtering client-side over the loaded array. It is a household's
  // worth of rows, and a ?state= parameter would be a second place the
  // filter lives.
  const shown = rows
    .filter((r) => (state === "all" ? true : r.state === state))
    .filter((r) => (q === "" ? true : r.name.toLowerCase().includes(q.toLowerCase())));

  const wildstone = shown.filter((r) => r.ownership === "wildstone");
  const native = shown.filter((r) => r.ownership === "native");

  return (
    <>
      <div className="sub">
        What the house does on its own. A routine in trial does everything except act, so you can
        watch it for a week before letting it loose.
      </div>

      {isAdmin && <div className="callout">
        <strong>Household execution {executionStop.data?.active ? "stopped" : "running"}</strong>
        {executionStop.data?.active ? <div>{executionStop.data.reason || "An Owner stopped household actions."}</div> : <div>Stop all household actions immediately during an incident.</div>}
        <div className="row">
          {!executionStop.data?.active && <input aria-label="Reason for household stop" value={stopReason} onChange={e=>setStopReason(e.target.value)} placeholder="Why stop household actions?" />}
          <button className={executionStop.data?.active ? "btn btn-secondary" : "btn btn-danger"} disabled={stopAction.state.phase==="busy" || (!executionStop.data?.active && !stopReason.trim())}
            onClick={()=>stopAction.run(async()=>{await api.setHouseholdExecutionStop(executionStop.data?.active ? "resume" : "stop",stopReason);executionStop.reload();},executionStop.data?.active ? "Household execution resumed." : "Household execution stopped.")}>
            {executionStop.data?.active ? "Resume household actions" : "Stop household actions"}
          </button>
        </div>
        <ErrorLine error={executionStop.error ?? (stopAction.state.phase==="error" ? stopAction.state.message : null)} />
      </div>}

      <div className="row">
        <div className="seg" role="tablist" aria-label="Who this is for">
          {(["admin", "household"] as Audience[]).map((a) => (
            <button
              key={a}
              role="tab"
              aria-selected={audience === a}
              className={audience === a ? "on" : ""}
              onClick={() => setAudience(a)}
            >
              {a === "admin" ? "Admin view" : "Household view"}
            </button>
          ))}
        </div>
        <input
          className="search"
          type="text"
          placeholder="Search routines…"
          aria-label="Search routines"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <div className="chips group-chips" style={{ marginBottom: 0 }}>
          {(["all", ...STATES] as StateFilter[]).map((s) => (
            <button
              key={s}
              className="chip"
              aria-pressed={state === s}
              onClick={() => setState(s)}
            >
              {s === "all" ? "All" : s}{" "}
              <span className="chip-count">
                {s === "all" ? rows.length : rows.filter((r) => r.state === s).length}
              </span>
            </button>
          ))}
        </div>
        {isAdmin && (
          <button
            className="btn btn-primary"
            style={{ marginLeft: "auto" }}
            onClick={() => setWriting((v) => !v)}
          >
            <Plus size={15} /> {writing ? "Cancel" : "New routine"}
          </button>
        )}
      </div>

      {writing && (
        <NewRoutine
          onDone={() => {
            setWriting(false);
            load.reload();
          }}
        />
      )}

      <ErrorLine error={load.error} />
      {rows.length === 0 && !load.loading && <div className="muted">No routines yet.</div>}
      {rows.length > 0 && shown.length === 0 && (
        <div className="muted">Nothing matches that.</div>
      )}

      {isAdmin && wildstone.length > 0 && (
        <div className="sect">
          <span className="eyebrow">Run by Wildstone</span>
          <span className="tab-count">{wildstone.length}</span>
          <span className="fade" />
        </div>
      )}
      <div className="card-grid">
        {wildstone.map((r) => (
          <WildstoneCard
            key={r.name}
            routine={r}
            spec={specs[r.name]}
            detailed={isAdmin}
            onOpen={() => onMode({ kind: "detail", name: r.name })}
          />
        ))}
      </div>

      {/* A household audience gets the Wildstone routines and nothing
          else: a native routine is not something anyone in this house
          can act on from here, and the operator detail is noise. */}
      {isAdmin && native.length > 0 && (
        <>
          <div className="sect" style={{ marginTop: "var(--space-6)" }}>
            <span className="eyebrow">Native to an app</span>
            <span className="tab-count">{native.length}</span>
            <span className="muted">Wildstone only knows about these</span>
            <span className="fade" />
          </div>
          <div className="card-grid">
            {native.map((r) => (
              <NativeCard key={r.name} routine={r} />
            ))}
          </div>
        </>
      )}

      <PendingRoutinePlans />
    </>
  );
}

// Writing a routine.
//
// A routine IS its document — nothing in Wildstone knows what breakfast
// is, and the file is the whole of the behaviour. So this is a text
// editor with a name on it, not a form with a field per concept: a form
// would have to enumerate the steps a routine may take, and it would be
// wrong the first time somebody wanted one it did not list.
//
// It is created as a draft and says so. The server refuses any other
// state on this route, because the endpoint needs only `propose` — a
// credential that could create an enabled routine could skip the review
// activation is supposed to require (ADR-W030 A6/A7.1).
//
// Editing an existing routine is the spec editor on RoutineEdit, which
// round-trips the parsed grammar. This one writes the document, because
// a routine that does not exist yet has no spec to serve.
function NewRoutine({ onDone }: { onDone: () => void }) {
  const [name, setName] = useState("");
  const [document, setDocument] = useState(TEMPLATE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function create() {
    setBusy(true);
    setError(null);
    try {
      await api.createHouseholdRoutine({ name: name.trim(), document });
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="work-form" style={{ display: "block", marginTop: 8 }}>
      <div className="row" style={{ gap: 8, alignItems: "center" }}>
        <input
          aria-label="routine name"
          placeholder="what to call it"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <button className="btn btn-secondary" disabled={!name.trim() || busy} onClick={create}>
          {busy ? "Saving…" : "Create as draft"}
        </button>
      </div>
      <textarea
        aria-label="routine document"
        rows={14}
        spellCheck={false}
        style={{ width: "100%", marginTop: 6, fontFamily: "ui-monospace, monospace" }}
        value={document}
        onChange={(e) => setDocument(e.target.value)}
      />
      <div className="muted" style={{ fontSize: "0.9em" }}>
        It starts as a draft and does nothing. Put it in trial to watch what it would do, then let
        it act.
      </div>
      <ErrorLine error={error} />
    </div>
  );
}

const TEMPLATE = `name: my-routine
trigger: person.left_home

# Two reports of the same thing within this window are one event.
dedupeWindow: 10m

steps:
  - kind: notify
    message: someone left
`;

function WildstoneCard({
  routine: r,
  spec,
  detailed,
  onOpen,
}: {
  routine: HouseholdRoutine;
  spec?: AutomationSpec;
  detailed: boolean;
  onOpen: () => void;
}) {
  return (
    <div
      className="card"
      role="button"
      tabIndex={0}
      onClick={onOpen}
      // A role="button" with a click handler and no key handler is a
      // control a keyboard cannot reach.
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <div className="card-head">
        <StatusPill kind={ROUTINE_TONE[r.state]}>{r.state}</StatusPill>
        <span className="card-name">{r.name}</span>
        {detailed && <span className="muted tnum">rev {r.revision}</span>}
      </div>
      <div className="muted">{ROUTINE_MEANING[r.state]}</div>
      {/* Operator detail. An adult can still open a routine and stop it
          — the ADR-W030 A8 amendment gives any trusted adult that
          authority — but the tier and the revision are not their
          question. */}
      {detailed && spec && (
        <div className="chips">
          <span className="chip mono">{spec.trigger.eventType}</span>
          <span className="chip mono">tier: {spec.action.tier}</span>
        </div>
      )}
      {detailed && (
        <div className="card-meta">
          <span>{r.author}</span>
          <span>changed {ago(r.updatedAt)}</span>
          {r.disabledBy && <span>stopped by {r.disabledBy}</span>}
        </div>
      )}
    </div>
  );
}

// Same card without the affordance: there is nothing Wildstone can show
// about a spec it does not hold, so opening one would open an empty
// screen.
function NativeCard({ routine: r }: { routine: HouseholdRoutine }) {
  return (
    <div className="card">
      <div className="card-head">
        <StatusPill kind={ROUTINE_TONE[r.state]}>{r.state}</StatusPill>
        <span className="card-name">{r.name}</span>
        <LockSimple size={14} className="card-stale" />
      </div>
      <div className="muted">{ROUTINE_MEANING[r.state]}</div>
      <div className="card-meta">
        <span>runs inside {r.connection} — Wildstone only knows about it</span>
      </div>
    </div>
  );
}

// A routine plan waiting on a human, read off the shell's existing poll
// rather than a second load of the same list.
//
// It points at Plans rather than approving here: a household-routine
// plan is reviewed where every other plan is reviewed, and a second
// approval surface is a second place the rules can differ.
function PendingRoutinePlans() {
  const { snap } = useConsoleState();
  const waiting = (snap?.pending ?? []).filter((p) =>
    (p.plan?.steps ?? []).some((s) => s.kind === "Automation"),
  );
  if (waiting.length === 0) return null;
  return (
    <div className="callout callout-warn" style={{ marginTop: "var(--space-6)" }}>
      {waiting.length === 1
        ? "1 routine plan waits for a human."
        : `${waiting.length} routine plans wait for a human.`}{" "}
      Review them on Plans.
    </div>
  );
}
