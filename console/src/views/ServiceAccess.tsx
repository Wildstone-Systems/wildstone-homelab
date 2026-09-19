import { useState } from "react";
import { api, type AccessDecision, type PersonGroup, type ServiceAccess } from "../api";
import { ErrorLine, useAction, useLoad } from "../lib";
import { StatusPill } from "../ui/status";

// Who may reach one service (ADR-W033 design §4, §18.2; slice 8).
//
// WHAT THIS SCREEN SHOWS FIRST is who it resolves to, not what the
// rules are. An Owner reading "Adults may use this" still has to work
// out who that is today, and getting it wrong is the whole cost of a
// permission model — so the answer is at the top and the rules that
// produced it are underneath.
//
// PRESENTATION, NEVER THE GATE. Nothing here decides a request. The
// door is Authelia's access rule, the OIDC client's policy, or
// Keystone's own check, and §18.2 requires a direct URL to be denied
// regardless of what any screen drew.
export function ServiceAccessPanel({ capability }: { capability: string }) {
  const access = useLoad(() => api.serviceAccess(capability), [capability]);
  const groups = useLoad(() => api.personGroups(), []);
  if (access.error) return <ErrorLine error={access.error} />;
  if (!access.data) return <div className="muted">Loading who may use this…</div>;
  return (
    <ServiceAccessView
      access={access.data}
      groups={groups.data ?? []}
      onChanged={() => access.reload()}
    />
  );
}

// Separated from its fetch so the render check can put it in a state:
// nobody decided, a group allowed, a person restricted, and dormant.
export function ServiceAccessView({
  access,
  groups,
  onChanged,
}: {
  access: ServiceAccess;
  groups: PersonGroup[];
  onChanged: () => void;
}) {
  const action = useAction();
  const [adding, setAdding] = useState(false);

  const decided = new Set([
    ...access.groups.map((g) => g.id),
    ...access.people.map((p) => p.id),
  ]);
  const undecided = groups.filter((g) => !decided.has(g.id));

  const busy = action.state.phase === "busy";
  const failure = action.state.phase === "error" ? action.state.message ?? null : null;

  const set = (subjectKind: "group" | "person", subject: string, effect: "" | "allow" | "restrict") =>
    action.run(
      async () => {
        await api.setServiceAccess(access.capability, { subjectKind, subject, effect });
        setAdding(false);
        onChanged();
      },
      effect === "" ? "rule removed" : effect === "allow" ? "allowed" : "refused",
    );

  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-6)" }}>
        <span className="eyebrow">Who may use this</span>
        <span className="muted">{access.reaches.length}</span>
        <span className="fade" />
      </div>

      {access.dormant && (
        <div className="callout callout-warn">
          This service is not installed, so none of these admit anybody. The decisions are kept and
          come back if you install it again.
        </div>
      )}

      {/* The answer, before the rules that produced it. */}
      {access.reaches.length === 0 ? (
        <div className="muted">
          Nobody. A service with no decision on record reaches nobody rather than everybody — which
          is the safe way round, and means this is waiting for you rather than broken.
        </div>
      ) : (
        <div className="chips">
          {access.reaches.map((who) => (
            <span className="chip" key={who}>
              {who}
            </span>
          ))}
        </div>
      )}

      <div className="sect" style={{ marginTop: "var(--space-5)" }}>
        <span className="eyebrow">Rules</span>
        <span className="fade" />
      </div>

      {access.groups.length === 0 && access.people.length === 0 ? (
        <div className="muted">Nothing decided yet.</div>
      ) : (
        <ul className="rules">
          {access.groups.map((d) => (
            <DecisionRow
              key={`g/${d.id}`}
              decision={d}
              kind="group"
              busy={busy}
              onSet={(effect) => set("group", d.id, effect)}
            />
          ))}
          {access.people.map((d) => (
            <DecisionRow
              key={`p/${d.id}`}
              decision={d}
              kind="person"
              busy={busy}
              onSet={(effect) => set("person", d.id, effect)}
            />
          ))}
        </ul>
      )}

      {/* Adding names a GROUP. A person is named by restricting or
          allowing them on an existing row, which is the shape §4 asks
          for: groups are the namespace, and a person-level row is the
          exception to one. */}
      {undecided.length > 0 &&
        (adding ? (
          <div className="chips" style={{ marginTop: "var(--space-3)" }}>
            {undecided.map((g) => (
              <button
                key={g.id}
                className="chip chip-btn"
                disabled={busy}
                onClick={() => set("group", g.id, "allow")}
              >
                {g.name}
                {g.members.length > 0 ? ` (${g.members.length})` : " (nobody in it yet)"}
              </button>
            ))}
            <button className="chip chip-btn" onClick={() => setAdding(false)}>
              Cancel
            </button>
          </div>
        ) : (
          <button
            className="btn btn-ghost"
            style={{ marginTop: "var(--space-3)" }}
            onClick={() => setAdding(true)}
          >
            Let a group use this…
          </button>
        ))}

      {failure && <ErrorLine error={failure} />}

      {access.declared && access.declared.length > 0 && (
        <div className="muted" style={{ marginTop: "var(--space-4)" }}>
          This service says it is for {access.declared.join(", ")}. That is where these rules came
          from; changing them here is yours to do, and installing it again will not undo it.
        </div>
      )}
    </>
  );
}

// One rule, and the two things an Owner can do to it.
//
// "Remove" and "Restrict" are genuinely different and the row says so:
// removing returns the subject to whatever their groups say, and
// restricting refuses them even when a group they are in is allowed.
// Offering only one of the two would make a household unable to express
// the other.
function DecisionRow({
  decision,
  kind,
  busy,
  onSet,
}: {
  decision: AccessDecision;
  kind: "group" | "person";
  busy: boolean;
  onSet: (effect: "" | "allow" | "restrict") => void;
}) {
  const allowed = decision.effect === "allow";
  return (
    <li className="rule">
      <StatusPill kind={allowed ? "ok" : "bad"}>{allowed ? "may use" : "refused"}</StatusPill>
      <span>{decision.name || decision.id}</span>
      <span className="timeline-src">{kind}</span>
      <span className="fade" />
      <button
        className="btn btn-ghost"
        disabled={busy}
        onClick={() => onSet(allowed ? "restrict" : "allow")}
      >
        {allowed ? "Refuse" : "Allow"}
      </button>
      <button className="btn btn-ghost" disabled={busy} onClick={() => onSet("")}>
        Remove
      </button>
    </li>
  );
}
