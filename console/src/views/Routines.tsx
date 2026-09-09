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
  const rows = routines.data?.routines ?? [];
  return (
    <section className="work-section">
      <h3>Routines</h3>
      <ErrorLine error={routines.error} />
      {rows.length === 0 && !routines.loading && (
        <div className="muted">No routines yet.</div>
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
                      }, "Stopped.")
                    }
                  >
                    Stop it
                  </button>
                )}
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
