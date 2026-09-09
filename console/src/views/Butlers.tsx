import { useState } from "react";
import { api, type ButlerRow, type ModelRoute } from "../api";
import { ErrorLine, useLoad } from "../lib";
import { StatusPill } from "../ui/status";

// The household's Butlers (ADR-W031, work package B6).
//
// There is not one Butler. There are as many as the household made,
// each with its own model, its own directive and its own scope — a
// health Butler on the local model, a nutrition Butler on a cloud one.
//
// This screen works hard on exactly one thing: making it impossible to
// look at a Butler without seeing where its words go. That is A2 and A4
// rendered. Somebody deciding to tell a Butler something sensitive is
// relying entirely on which model sits behind it, and a screen that
// puts that a click away has failed at the only moment it mattered.
//
// Switching a model applies at once (A8): the person here owns every
// Butler, so a proposal they would then approve themselves is ceremony
// with nobody able to refuse it. What they get instead is the
// consequence — and going local-to-cloud, a sentence saying what
// happens to a conversation already open.

export function Butlers() {
  return (
    <>
      <div className="sub">
        Each Butler has one model, and its own directive and reach. A Butler on a cloud model sends
        what it is told outside the house — that is what the badge means, and it is why the health
        Butler and the nutrition Butler are separate.
      </div>
      <Roster />
    </>
  );
}

function Roster() {
  const butlers = useLoad(() => api.butlers());
  const routes = useLoad(() => api.modelRoutes());
  const [creating, setCreating] = useState(false);
  const rows = butlers.data?.butlers ?? [];
  const available = (routes.data?.routes ?? [])
    .map((r) => r.route)
    .filter((r) => r.enabled);

  // Grouped by surface, because the difference is worth seeing without
  // a word of explanation: an operator Butler can propose changes to the
  // home lab and a household one cannot (A3).
  const bySurface = [
    { key: "family" as const, title: "Household", blurb: "Reads the home. Cannot touch the home lab." },
    { key: "admin" as const, title: "Operator", blurb: "Can propose changes to the home lab." },
  ];

  return (
    <section className="work-section">
      <div className="row" style={{ justifyContent: "space-between", alignItems: "center" }}>
        <h3 style={{ margin: 0 }}>Butlers</h3>
        <button className="btn btn-secondary" onClick={() => setCreating((v) => !v)}>
          {creating ? "Cancel" : "New Butler"}
        </button>
      </div>
      <ErrorLine error={butlers.error} />
      <ErrorLine error={routes.error} />

      {creating && (
        <NewButler
          routes={available}
          onDone={() => {
            setCreating(false);
            butlers.reload();
          }}
        />
      )}

      {rows.length === 0 && !butlers.loading && !creating && (
        <div className="muted">
          No Butlers yet. Make one per job — a health Butler, a nutrition Butler — and give each the
          model its material should go to.
        </div>
      )}

      {bySurface.map((group) => {
        const mine = rows.filter((r) => r.butler.surface === group.key);
        if (mine.length === 0) return null;
        return (
          <div key={group.key}>
            <h4 style={{ marginBottom: 2 }}>{group.title}</h4>
            <div className="muted" style={{ marginBottom: 8 }}>
              {group.blurb}
            </div>
            <ul className="work-evidence">
              {mine.map((row) => (
                <ButlerCard
                  key={row.butler.name}
                  row={row}
                  routes={available}
                  onChanged={() => butlers.reload()}
                />
              ))}
            </ul>
          </div>
        );
      })}
    </section>
  );
}

function ButlerCard({
  row,
  routes,
  onChanged,
}: {
  row: ButlerRow;
  routes: ModelRoute[];
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);
  const b = row.butler;

  // The response is kept rather than discarded, because the crossing
  // warning only exists in it. useAction would have been the house
  // pattern here, but it reports a fixed message and this control's
  // whole job is to say what THIS switch did.
  async function switchModel(route: string) {
    if (route === b.route) return;
    setBusy(true);
    setError(null);
    try {
      const res = await api.setButlerRoute(b.name, route);
      // A statement of what just happened, not a dialog asking
      // permission for it. The person owns this Butler; what they are
      // owed is the consequence, in the moment they caused it.
      setWarning(res.warning ?? null);
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <li>
      <div className="row" style={{ justifyContent: "space-between", alignItems: "baseline" }}>
        <strong>{b.name}</strong>
        <div className="chips">
          <StatusPill kind={row.leavesTheHouse ? "warn" : "ok"}>
            {row.leavesTheHouse ? "leaves the house" : "stays in the house"}
          </StatusPill>
          {!row.reachable && <StatusPill kind="bad">model unavailable</StatusPill>}
          {row.reachable && !row.carriesActions && (
            <StatusPill kind="warn">answers questions only</StatusPill>
          )}
          {b.retiredAt && <StatusPill kind="mute">retired</StatusPill>}
        </div>
      </div>

      {b.directive && <div className="muted">{b.directive}</div>}

      <div className="row" style={{ gap: 8, alignItems: "center", marginTop: 6 }}>
        <label className="muted" htmlFor={`route-${b.name}`}>
          Model
        </label>
        <select
          id={`route-${b.name}`}
          value={b.route}
          disabled={!!b.retiredAt || busy}
          onChange={(e) => switchModel(e.target.value)}
        >
          {/* A Butler on a since-disabled route still has to render as
              being on THAT route. Listing only enabled ones would make
              the select fall back to its first option and show a model
              this Butler is not on — the one lie this screen must not
              tell. */}
          {!routes.some((r) => r.name === b.route) && (
            <option value={b.route}>{b.route} — turned off</option>
          )}
          {routes.map((r) => (
            <option key={r.name} value={r.name}>
              {r.name} — {r.kind === "cloud" ? "outside the house" : "on your hardware"}
            </option>
          ))}
        </select>
      </div>

      {warning && (
        <div className="muted" style={{ marginTop: 6 }}>
          <StatusPill kind="warn">heads up</StatusPill> {warning}
        </div>
      )}
      <ErrorLine error={error} />
    </li>
  );
}

// Creating a Butler grants a new scope rather than changing an existing
// one, which is why it is the one act here that is not a live control.
function NewButler({ routes, onDone }: { routes: ModelRoute[]; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [directive, setDirective] = useState("");
  const [surface, setSurface] = useState("family");
  const [route, setRoute] = useState(routes[0]?.name ?? "");

  const chosen = routes.find((r) => r.name === route);

  async function create() {
    setBusy(true);
    setError(null);
    try {
      await api.createButler({
        name: name.trim(),
        directive: directive.trim(),
        surface,
        profile: surface === "admin" ? "operator" : "household",
        route,
      });
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="work-section" style={{ marginTop: 8 }}>
      <label htmlFor="butler-name">Name</label>
      <input
        id="butler-name"
        value={name}
        placeholder="e.g. Alfred, or health"
        onChange={(e) => setName(e.target.value)}
      />

      <label htmlFor="butler-directive">What it is for</label>
      <input
        id="butler-directive"
        value={directive}
        placeholder="e.g. keeps track of training and sleep"
        onChange={(e) => setDirective(e.target.value)}
      />
      <div className="muted">
        This shapes how it talks. It grants nothing — tools, connections and memory are set
        separately.
      </div>

      <label htmlFor="butler-surface">Surface</label>
      <select id="butler-surface" value={surface} onChange={(e) => setSurface(e.target.value)}>
        <option value="family">Household — reads the home, cannot touch the home lab</option>
        <option value="admin">Operator — can propose home lab changes</option>
      </select>

      <label htmlFor="butler-route">Model</label>
      <select id="butler-route" value={route} onChange={(e) => setRoute(e.target.value)}>
        {routes.map((r) => (
          <option key={r.name} value={r.name}>
            {r.name} — {r.kind === "cloud" ? "outside the house" : "on your hardware"}
          </option>
        ))}
      </select>
      {chosen?.kind === "cloud" && (
        <div className="muted" style={{ marginTop: 6 }}>
          <StatusPill kind="warn">leaves the house</StatusPill> Anything told to this Butler goes to{" "}
          {chosen.name}, outside the house. For material that should not leave, pick a model running
          on your own hardware.
        </div>
      )}

      <ErrorLine error={error} />
      <div className="row" style={{ gap: 8, marginTop: 8 }}>
        <button className="btn" disabled={!name.trim() || !route || busy} onClick={create}>
          Create
        </button>
      </div>
    </div>
  );
}
