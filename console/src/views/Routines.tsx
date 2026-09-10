import { useState } from "react";
import { api, type HouseholdRoutine, type ModelRoute } from "../api";
import { ErrorLine, useAction, useLoad } from "../lib";
import { Kind, StatusPill } from "../ui/status";

// The household's routines, and where questions get sent (ADR-W030
// A6/A9, P13/P14).
//
// Two lists that belong together because they answer one question a
// household actually asks: what does this house do on its own, and who
// is it asking.
//
// The routine list works hard on two distinctions. Native versus
// Wildstone — one runs inside an application and Wildstone only knows
// about it, the other Wildstone runs — because offering to edit
// something you cannot is worse than not showing it. And trial versus
// enabled: a trial routine does everything except act, which is the
// whole point of watching one for a week before letting it loose.
//
// The route list makes one thing unmissable: whether a route leaves the
// house.

const ROUTINE_TONE: Record<HouseholdRoutine["state"], Kind> = {
  draft: "mute",
  trial: "warn",
  enabled: "ok",
  disabled: "bad",
};

const ROUTINE_MEANING: Record<HouseholdRoutine["state"], string> = {
  draft: "written, not running",
  trial: "running and watching — it does everything except act",
  enabled: "running, and it acts",
  disabled: "stopped",
};

export function Routines() {
  return (
    <>
      <div className="sub">
        What the house does on its own, and which model it asks. A routine in trial does everything
        except act, so you can watch it for a week before letting it loose.
      </div>
      <RoutineList />
      <RouteList />
    </>
  );
}

function RoutineList() {
  const routines = useLoad(() => api.householdRoutines());
  const action = useAction();
  const [writing, setWriting] = useState(false);
  const rows = routines.data?.routines ?? [];
  return (
    <section className="work-section">
      <div className="row" style={{ justifyContent: "space-between", alignItems: "center" }}>
        <h3 style={{ margin: 0 }}>Routines</h3>
        <button className="btn btn-secondary" onClick={() => setWriting((v) => !v)}>
          {writing ? "Cancel" : "New routine"}
        </button>
      </div>
      <ErrorLine error={routines.error} />
      {writing && (
        <NewRoutine
          onDone={() => {
            setWriting(false);
            routines.reload();
          }}
        />
      )}
      {rows.length === 0 && !routines.loading && !writing && (
        <div className="muted">
          No routines yet. A routine is a document the household writes — what to do, and when.
        </div>
      )}
      <ul className="work-evidence">
        {rows.map((r) => (
          <li key={r.name}>
            <StatusPill kind={ROUTINE_TONE[r.state]}>{r.state}</StatusPill>
            <strong>{r.name}</strong>
            <span className="timeline-src">{ROUTINE_MEANING[r.state]}</span>
            {r.ownership === "native" ? (
              <span className="timeline-src">
                runs inside {r.connection} — Wildstone only knows about it
              </span>
            ) : (
              <span className="timeline-src">run by Wildstone · revision {r.revision}</span>
            )}
            {r.disabledBy && <span className="timeline-src">stopped by {r.disabledBy}</span>}
            {r.ownership === "wildstone" && (
              <span className="work-form">
                {r.state !== "trial" && (
                  <button
                    className="btn btn-ghost"
                    onClick={() =>
                      action.run(async () => {
                        await api.setRoutineState(r.name, "trial");
                        routines.reload();
                      }, "Watching without acting.")
                    }
                  >
                    Watch only
                  </button>
                )}
                {r.state !== "enabled" && (
                  <button
                    className="btn btn-ghost"
                    onClick={() =>
                      action.run(async () => {
                        await api.setRoutineState(r.name, "enabled");
                        routines.reload();
                      }, "Running.")
                    }
                  >
                    Let it act
                  </button>
                )}
                {r.state !== "disabled" && (
                  <button
                    className="btn btn-ghost"
                    onClick={() =>
                      action.run(async () => {
                        await api.setRoutineState(r.name, "disabled");
                        routines.reload();
                      }, "Stopped. It stays here, stopped, so what it did is still readable.")
                    }
                  >
                    Stop it
                  </button>
                )}
                <RoutineEditor routine={r} onSaved={() => routines.reload()} />
              </span>
            )}
          </li>
        ))}
      </ul>
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
      {action.state.phase === "done" && <div className="msg-note">{action.state.message}</div>}
    </section>
  );
}

const READINESS_MEANING: Record<ModelRoute["readiness"], string> = {
  unprobed: "not checked yet, so it answers questions and uses no tools that do things",
  ready: "checked and trusted with tools",
  "qa-only": "checked and found wanting, so it answers questions only",
  unavailable: "not answering",
};

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
// activation is supposed to require.
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

// Editing an existing routine's document.
//
// Only Wildstone-owned routines are editable, and the caller already
// checks that — a native routine runs inside another application and
// Wildstone only knows about it, so offering to edit one would be
// offering something that cannot work.
//
// The server's note is shown rather than swallowed. Editing bumps the
// revision and any review link issued for the previous one stops
// approving anything, which is a consequence the person editing should
// see at the moment they cause it.
function RoutineEditor({
  routine,
  onSaved,
}: {
  routine: HouseholdRoutine;
  onSaved: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [document, setDocument] = useState(routine.document);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<string | null>(null);

  async function save() {
    setBusy(true);
    setError(null);
    try {
      const res = await api.editHouseholdRoutine(routine.name, document, note.trim());
      setResult(res.note ?? `saved as revision ${res.revision}`);
      onSaved();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!open) {
    return (
      <button className="btn btn-ghost" onClick={() => setOpen(true)}>
        Edit
      </button>
    );
  }
  return (
    <div style={{ display: "block", width: "100%", marginTop: 6 }}>
      <textarea
        aria-label={`${routine.name} document`}
        rows={14}
        spellCheck={false}
        style={{ width: "100%", fontFamily: "ui-monospace, monospace" }}
        value={document}
        onChange={(e) => setDocument(e.target.value)}
      />
      <div className="row" style={{ gap: 8, marginTop: 4, alignItems: "center" }}>
        <input
          aria-label="what changed"
          placeholder="what changed (optional)"
          value={note}
          onChange={(e) => setNote(e.target.value)}
        />
        <button
          className="btn btn-secondary"
          disabled={busy || document === routine.document}
          onClick={save}
        >
          {busy ? "Saving…" : "Save revision"}
        </button>
        <button className="btn btn-ghost" onClick={() => setOpen(false)}>
          Close
        </button>
      </div>
      {result && <div className="msg-note">{result}</div>}
      <ErrorLine error={error} />
    </div>
  );
}

function RouteList() {
  const routes = useLoad(() => api.modelRoutes());
  const action = useAction();
  const rows = routes.data?.routes ?? [];
  return (
    <section className="work-section">
      <h3>Which model answers</h3>
      <ErrorLine error={routes.error} />
      {rows.length === 0 && !routes.loading && (
        <div className="muted">
          No routes configured. The Butler is running on whatever it was started with.
        </div>
      )}
      <ul className="work-evidence">
        {rows.map(({ route, carriesActions, leavesTheHouse }) => (
          <li key={route.name}>
            <StatusPill kind={route.readiness === "ready" ? "ok" : route.readiness === "unavailable" ? "bad" : "warn"}>
              {route.readiness}
            </StatusPill>
            <strong>{route.name}</strong>
            {/* The one thing that has to be unmissable. */}
            <StatusPill kind={leavesTheHouse ? "warn" : "ok"}>
              {leavesTheHouse ? "leaves the house" : "stays in the house"}
            </StatusPill>
            <span className="timeline-src">
              {route.provider}
              {route.model && ` · ${route.model}`}
            </span>
            <span className="timeline-src">{READINESS_MEANING[route.readiness]}</span>
            {!carriesActions && route.readiness !== "unavailable" && (
              <span className="timeline-src">answers only</span>
            )}
            <button
              className="btn btn-ghost"
              onClick={() =>
                action.run(async () => {
                  await api.setModelRouteEnabled(route.name, !route.enabled);
                  routes.reload();
                }, route.enabled ? "Turned off." : "Turned on.")
              }
            >
              {route.enabled ? "Turn off" : "Turn on"}
            </button>
          </li>
        ))}
      </ul>
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
    </section>
  );
}
