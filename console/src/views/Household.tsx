import { useState } from "react";
import { api, HouseholdGroup, HouseholdMember, PlanRecord } from "../api";
import { ErrorLine, useLoad } from "../lib";

// Household (ADR-W020 A3, ADR-W023 A7): the people this home lab knows.
//
// A member is an `LdapUser` in the Statefile, not a row in LLDAP's own
// UI. That is the whole reason this screen exists: the household's
// account list belongs in state, in plans, in approval and in audit,
// because a Statefile restore that brought back every group with nobody
// in them would read as a working system and be an empty one.
//
// Two things this screen deliberately does not do. It does not create an
// account — it proposes a plan, and the account is created by the
// identity capability's provisioning job once a human approves it. And
// it does not remove anybody: declaring a person is one-way everywhere
// else in the system, and a button here would promise something the
// control plane does not do.
export function Household() {
  const members = useLoad<HouseholdMember[]>(() => api.householdMembers());
  const groups = useLoad<HouseholdGroup[]>(() => api.householdGroups());

  return (
    <>
      <div className="sub">
        Everyone with an account here. Adding someone proposes a plan; their account is created
        when you approve it, without a password — they set their own through the sign-in page's
        reset flow.
      </div>
      <ErrorLine error={members.error ?? groups.error} />

      <AddMember groups={groups.data ?? []} members={members.data ?? []} onDone={members.reload} />

      <div className="sect" style={{ marginTop: "var(--space-8)" }}>
        <span className="eyebrow">Members</span>
        <span className="fade" />
      </div>
      <table>
        <thead>
          <tr>
            <th>member</th>
            <th>name</th>
            <th>email</th>
            <th>groups</th>
            <th>declared by</th>
          </tr>
        </thead>
        <tbody>
          {(members.data ?? []).map((m) => (
            <tr key={m.username}>
              <td>{m.username}</td>
              <td>{m.displayName || <span className="muted">—</span>}</td>
              <td>{m.email || <span className="muted">—</span>}</td>
              <td>
                {m.groups.length > 0 ? (
                  m.groups.join(", ")
                ) : (
                  <span className="muted">no groups — they can sign in and reach nothing</span>
                )}
              </td>
              <td className="muted">{m.owner === "system" ? "you" : m.owner}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {!members.loading && (members.data ?? []).length === 0 && (
        <div className="muted">
          Nobody has an account yet. Add the first person above; until then there is nobody to
          invite to the household butler either.
        </div>
      )}
    </>
  );
}

// AddMember is the form ADR-W023 A7 describes: the admin adds people,
// and what they are really doing is proposing a resource.
//
// Groups are offered rather than typed for the same reason the chat
// invite offers transports: a membership in a group no capability
// declares cannot be provisioned, and it would fail in a container log
// nobody reads rather than here.
export function AddMember({
  groups,
  members,
  onDone,
}: {
  groups: HouseholdGroup[];
  members: HouseholdMember[];
  onDone: () => void;
}) {
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [email, setEmail] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [node, setNode] = useState("");
  const [proposed, setProposed] = useState<PlanRecord | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The node is derived by the API and only asked for when a deployment
  // genuinely runs more than one directory.
  const nodes = [...new Set([...groups, ...members].map((g) => g.node).filter(Boolean))];
  const taken = new Set(members.map((m) => m.username));

  const toggle = (name: string) =>
    setChosen((prev) => (prev.includes(name) ? prev.filter((g) => g !== name) : [...prev, name]));

  const add = () => {
    setBusy(true);
    setError(null);
    api
      .addHouseholdMember({
        username: username.trim(),
        displayName: displayName.trim(),
        email: email.trim(),
        groups: chosen,
        ...(node ? { node } : {}),
      })
      .then((plan) => {
        setProposed(plan);
        setUsername("");
        setDisplayName("");
        setEmail("");
        setChosen([]);
        onDone();
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  const duplicate = taken.has(username.trim());
  const ready = username.trim() !== "" && email.trim() !== "" && !duplicate;

  return (
    <div className="card">
      <h3>Add someone</h3>
      <div className="row">
        <input
          type="text"
          placeholder="username, e.g. sam"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <input
          type="text"
          placeholder="full name, e.g. Sam Reed"
          value={displayName}
          onChange={(e) => setDisplayName(e.target.value)}
        />
        <input
          type="text"
          placeholder="email, e.g. sam@example.com"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
        />
        {nodes.length > 1 && (
          <select value={node} onChange={(e) => setNode(e.target.value)}>
            <option value="">directory node…</option>
            {nodes.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        )}
        <button className="btn btn-primary" disabled={busy || !ready} onClick={add}>
          {busy ? "Proposing…" : "Propose"}
        </button>
      </div>

      {groups.length > 0 ? (
        <div className="row" style={{ flexWrap: "wrap", marginTop: 8 }}>
          {groups.map((g) => (
            <label key={g.name} style={{ marginRight: 12 }}>
              <input
                type="checkbox"
                checked={chosen.includes(g.name)}
                onChange={() => toggle(g.name)}
              />{" "}
              {g.name}
            </label>
          ))}
        </div>
      ) : (
        <div className="muted" style={{ marginTop: 8 }}>
          No app declares a group yet, so there is nothing to give access to. Install an app from
          Services first, then add its group here.
        </div>
      )}

      {duplicate && <div className="muted">{username.trim()} already has an account.</div>}
      <ErrorLine error={error} />
      {proposed && (
        <div className="muted" style={{ marginTop: 8 }}>
          Proposed plan {proposed.id}. Nobody has an account until you approve it on the Plans
          screen.
        </div>
      )}
      <div className="muted" style={{ marginTop: 8 }}>
        The email is how the sign-in page reaches them for a password reset, and LLDAP requires
        one. No password is set here — a credential never lives in declared state.
      </div>
    </div>
  );
}
