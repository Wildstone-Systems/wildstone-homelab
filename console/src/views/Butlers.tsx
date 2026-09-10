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
  const [open, setOpen] = useState(false);
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

      {!b.retiredAt && (
        <div className="row" style={{ gap: 8, marginTop: 6 }}>
          <button className="btn btn-secondary" onClick={() => setOpen((v) => !v)}>
            {open ? "Done" : "Edit"}
          </button>
        </div>
      )}

      {open && !b.retiredAt && (
        <ButlerDetail name={b.name} directive={b.directive ?? ""} onChanged={onChanged} />
      )}
    </li>
  );
}

// The rest of a Butler: what it is for, who may talk to it, where it
// answers, and how to retire it.
//
// It is behind an Edit button rather than always open because the
// roster's job is to let somebody see at a glance where each Butler's
// words go, and four editors per row buries that. Opening one is a
// deliberate act on ONE Butler, which is also the right shape for
// controls that change who can hear what.
function ButlerDetail({
  name,
  directive,
  onChanged,
}: {
  name: string;
  directive: string;
  onChanged: () => void;
}) {
  const scope = useLoad(() => api.butlerScope(name));
  const members = useLoad(() => api.butlerMembers());
  const [error, setError] = useState<string | null>(null);

  async function run(fn: () => Promise<unknown>) {
    setError(null);
    try {
      await fn();
      scope.reload();
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <div style={{ marginTop: 10, paddingLeft: 12, borderLeft: "2px solid var(--line, #333)" }}>
      <ErrorLine error={error} />
      <ErrorLine error={scope.error} />

      <Directive name={name} initial={directive} onSaved={onChanged} />

      <People
        people={scope.data?.people ?? []}
        members={(members.data?.members ?? []).filter((m) => m.active).map((m) => m.member.member)}
        onSet={(member, allow) => run(() => api.setButlerAccess(name, member, allow))}
      />

      <Addresses
        addresses={scope.data?.addresses ?? []}
        onSet={(a) => run(() => api.setButlerAddress(name, a))}
      />

      <Retire name={name} onRetired={onChanged} />
    </div>
  );
}

// A directive says what a Butler is for, in its owner's words. It
// shapes how the Butler talks and grants it nothing (A9) — worth saying
// on the screen, because a box of free text next to a list of tools
// invites the belief that writing "you may read the calendar" in it
// does something.
function Directive({
  name,
  initial,
  onSaved,
}: {
  name: string;
  initial: string;
  onSaved: () => void;
}) {
  const [text, setText] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const dirty = text !== initial;

  async function save() {
    setBusy(true);
    setError(null);
    try {
      await api.setButlerDirective(name, text);
      setSaved(true);
      onSaved();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: 8 }}>
      <label className="muted" htmlFor={`directive-${name}`}>
        What this Butler is for
      </label>
      <textarea
        id={`directive-${name}`}
        rows={3}
        style={{ width: "100%", marginTop: 4 }}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setSaved(false);
        }}
      />
      <div className="muted" style={{ fontSize: "0.9em" }}>
        This shapes how it talks. It grants nothing — tools, connections and memory are set
        separately.
      </div>
      <div className="row" style={{ gap: 8, marginTop: 4, alignItems: "center" }}>
        <button className="btn btn-secondary" disabled={!dirty || busy} onClick={save}>
          {busy ? "Saving…" : "Save"}
        </button>
        {saved && !dirty && <span className="muted">saved</span>}
      </div>
      <ErrorLine error={error} />
    </div>
  );
}

// Who may talk to this Butler (A10). The Console holder decides, and
// this is where "Sam cannot reach the accounting Butler at all" is set
// — refused before a conversation starts rather than per tool.
function People({
  people,
  members,
  onSet,
}: {
  people: string[];
  members: string[];
  onSet: (member: string, allow: boolean) => void;
}) {
  const canAdd = members.filter((m) => !people.includes(m));
  const [pick, setPick] = useState("");

  return (
    <div style={{ marginTop: 10 }}>
      <div className="muted">Who may talk to it</div>
      {people.length === 0 && <div className="muted">Nobody yet — only you, from here.</div>}
      <ul className="work-evidence">
        {people.map((m) => (
          <li key={m} className="row" style={{ justifyContent: "space-between" }}>
            <span>{m}</span>
            <button className="btn btn-secondary" onClick={() => onSet(m, false)}>
              Remove
            </button>
          </li>
        ))}
      </ul>
      {canAdd.length > 0 && (
        <div className="row" style={{ gap: 8, marginTop: 4 }}>
          <select value={pick} onChange={(e) => setPick(e.target.value)}>
            <option value="">Add someone…</option>
            {canAdd.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          <button
            className="btn btn-secondary"
            disabled={!pick}
            onClick={() => {
              onSet(pick, true);
              setPick("");
            }}
          >
            Add
          </button>
        </div>
      )}
    </div>
  );
}

// Where this Butler answers.
//
// This is the control that decides which Butler replies to an incoming
// message (A7), so the screen says so plainly. On chat the address is
// the BOT — one bot per Butler, its own token — and two Butlers cannot
// share one: the store refuses, and the error it returns is shown
// rather than swallowed, because "that address already answers for
// somebody else" is the useful half of the failure.
function Addresses({
  addresses,
  onSet,
}: {
  addresses: { transport: string; externalId: string; label?: string }[];
  onSet: (a: { transport: string; externalId: string; label?: string; allow: boolean }) => void;
}) {
  const [transport, setTransport] = useState("telegram");
  const [externalId, setExternalId] = useState("");
  const [label, setLabel] = useState("");

  return (
    <div style={{ marginTop: 10 }}>
      <div className="muted">Where it answers</div>
      <div className="muted" style={{ fontSize: "0.9em" }}>
        A message arriving here is answered by this Butler, whatever it says. One address, one
        Butler.
      </div>
      {addresses.length === 0 && (
        <div className="muted">No address yet — it can only be reached from the Console.</div>
      )}
      <ul className="work-evidence">
        {addresses.map((a) => (
          <li
            key={`${a.transport}/${a.externalId}`}
            className="row"
            style={{ justifyContent: "space-between" }}
          >
            <span>
              {a.transport} / <code>{a.externalId}</code>
              {a.label ? ` — ${a.label}` : ""}
            </span>
            <button
              className="btn btn-secondary"
              onClick={() => onSet({ transport: a.transport, externalId: a.externalId, allow: false })}
            >
              Remove
            </button>
          </li>
        ))}
      </ul>
      <div className="row" style={{ gap: 8, marginTop: 4 }}>
        <input
          aria-label="transport"
          style={{ width: 110 }}
          value={transport}
          onChange={(e) => setTransport(e.target.value)}
        />
        <input
          aria-label="address"
          placeholder="the bot's capability"
          value={externalId}
          onChange={(e) => setExternalId(e.target.value)}
        />
        <input
          aria-label="label"
          placeholder="label (optional)"
          style={{ width: 140 }}
          value={label}
          onChange={(e) => setLabel(e.target.value)}
        />
        <button
          className="btn btn-secondary"
          disabled={!transport.trim() || !externalId.trim()}
          onClick={() => {
            onSet({
              transport: transport.trim(),
              externalId: externalId.trim(),
              label: label.trim() || undefined,
              allow: true,
            });
            setExternalId("");
            setLabel("");
          }}
        >
          Add
        </button>
      </div>
    </div>
  );
}

// Retiring is not deleting, and the button says which one it is.
//
// A Butler's conversations, its memory and its work stay recorded —
// removing the row would take the answer to "which model saw this" with
// it, which is the question the whole design exists to be able to
// answer. What retiring does is stop it answering.
function Retire({ name, onRetired }: { name: string; onRetired: () => void }) {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function retire() {
    setBusy(true);
    setError(null);
    try {
      await api.retireButler(name);
      onRetired();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: 12 }}>
      {!confirming ? (
        <button className="btn btn-secondary" onClick={() => setConfirming(true)}>
          Retire this Butler
        </button>
      ) : (
        <div>
          <div className="muted">
            {name} stops answering, on every address. Its conversations, memory and work stay
            recorded — retiring is not deleting, because "which model saw this" has to stay
            answerable afterwards.
          </div>
          <div className="row" style={{ gap: 8, marginTop: 6 }}>
            <button className="btn btn-secondary" disabled={busy} onClick={retire}>
              {busy ? "Retiring…" : `Retire ${name}`}
            </button>
            <button className="btn btn-secondary" disabled={busy} onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </div>
        </div>
      )}
      <ErrorLine error={error} />
    </div>
  );
}function NewButler({ routes, onDone }: { routes: ModelRoute[]; onDone: () => void }) {
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
