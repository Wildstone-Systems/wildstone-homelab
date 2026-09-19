import { useState } from "react";
import { CaretRight, Check } from "@phosphor-icons/react";
import { api, HouseholdGroup, HouseholdMember, PackageView, PlanRecord } from "../api";
import { ErrorLine, useLoad } from "../lib";
import { Drawer } from "../ui/Drawer";

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
  // Where the identity provider answers, read from the catalog rather
  // than rebuilt here. Keystone already resolves a Route's browsable
  // origin for the Services screen, and the tailnet posture makes that
  // a port on the node's name rather than the host in the spec — so a
  // URL assembled in the Console would be wrong on exactly the
  // deployments this household runs.
  const pkgs = useLoad<PackageView[]>(() => api.packages());
  const authOrigin = (pkgs.data ?? [])
    .find((p) => p.capability === "identity" && p.enabled)
    ?.addresses?.find((a) => a.route === "auth")?.url;
  const [open, setOpen] = useState<string | null>(null);
  const current = (members.data ?? []).find((m) => m.username === open);

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
            <th />
          </tr>
        </thead>
        <tbody>
          {(members.data ?? []).map((m) => (
            <tr
              key={m.username}
              className="row-clickable"
              onClick={() => setOpen(m.username)}
              tabIndex={0}
              role="button"
              aria-label={`Edit ${m.username}`}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  setOpen(m.username);
                }
              }}
            >
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
              <td className="muted">
                <CaretRight size={13} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {current && (
        <EditMember
          authOrigin={authOrigin}
          member={current}
          groups={groups.data ?? []}
          onClose={() => setOpen(null)}
          onDone={() => {
            setOpen(null);
            members.reload();
          }}
        />
      )}

      {!members.loading && (members.data ?? []).length === 0 && (
        <div className="muted">
          Nobody has an account yet. Add the first person above; until then there is nobody to
          invite to the household butler either.
        </div>
      )}
    </>
  );
}

// Editing one person, in a drawer beside the list they were picked
// from — the same shape the Butlers roster uses, and for the same
// reason: the list is what is being compared, and answering "which of
// these has no groups" by navigating away loses the comparison.
//
// It proposes. Nothing here writes to the directory, exactly as adding
// does not, because the account list belongs in state, in plans, in
// approval and in audit.
//
// Groups can only be ADDED, and the drawer says so rather than offering
// a control that would lie. The identity capability's provisioning job
// is additive in both directions — it creates a user the directory
// lacks and adds a membership it is missing, and never removes either.
// A tickbox that cleared a group would produce a plan that applies
// cleanly, a row that reads as changed, and a person who can still open
// the app. Removing one is a job for the directory itself, and saying
// that is more use than a switch that does nothing.
export function EditMember({
  member,
  groups,
  authOrigin,
  onClose,
  onDone,
}: {
  member: HouseholdMember;
  groups: HouseholdGroup[];
  // Where the identity provider answers, or undefined when identity is
  // not installed or serves no route yet.
  authOrigin?: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [displayName, setDisplayName] = useState(member.displayName ?? "");
  const [email, setEmail] = useState(member.email ?? "");
  const [adding, setAdding] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [plan, setPlan] = useState<PlanRecord | null>(null);

  const has = (g: string) => member.groups.includes(g);
  const available = groups.filter((g) => g.node === member.node);
  const dirty =
    adding.length > 0 ||
    displayName !== (member.displayName ?? "") ||
    email !== (member.email ?? "");

  async function save() {
    setBusy(true);
    setError(null);
    try {
      setPlan(
        await api.updateHouseholdMember(member.username, {
          displayName,
          email,
          groups: [...member.groups, ...adding],
        }),
      );
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Drawer open onClose={onClose} title={member.username}>
      <div className="drawer-head">
        <h3>{member.username}</h3>
      </div>

      {plan ? (
        // Said plainly, because the row behind this drawer has not
        // changed and will not until somebody approves.
        <div className="tab-stack">
          <div className="note">
            Proposed as <code>{plan.id}</code>. Nothing has changed yet — approve it on Plans and
            the provisioning job applies it on its next run.
          </div>
          <button className="btn btn-secondary" onClick={onDone}>
            Done
          </button>
        </div>
      ) : (
        <div className="tab-stack">
          <div>
            <label htmlFor="member-name">Name</label>
            <input
              id="member-name"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
            />
          </div>
          <div>
            <label htmlFor="member-email">Email</label>
            <input id="member-email" value={email} onChange={(e) => setEmail(e.target.value)} />
          </div>

          <div>
            <div className="eyebrow">PASSWORD</div>
            {/*
              A link, not a button, and deliberately so. Wildstone never
              holds this password: LLDAP stores it through an OPAQUE
              registration that neither a shell script nor Keystone can
              perform, so the directory's own client is the only thing
              positioned to change it (ADR-W020 A4). The one documented
              exception is the first Owner at install time, which exists
              because nothing can be signed into before there is one.

              So the reset is started by the person themselves, and this
              screen's job is to tell an Owner where to send them and
              where the code will come out — which is the part that was
              missing, not the mechanism. Authelia writes the code to a
              file rather than emailing it (there is no relay and there
              will not be one, ADR-W020 A6), and an Owner reads it back
              on Identity.
            */}
            {authOrigin ? (
              <div className="note">
                {member.username} sets their own password:{" "}
                <a href={`${authOrigin}/reset-password/step1`} target="_blank" rel="noreferrer">
                  the sign-in page&rsquo;s &ldquo;Forgot password?&rdquo;
                </a>
                . There is no mail relay here, so the one-time code is written to a file instead of
                sent — read it on <strong>Identity</strong> and hand it over in person, or on a
                channel you already trust.
              </div>
            ) : (
              <div className="muted">
                No sign-in service is installed yet, so there is nowhere to reset a password.
                Install identity first.
              </div>
            )}
          </div>

          <div>
            <div className="eyebrow">GROUPS</div>
            <div className="check-list">
              {available.map((g) => {
                const already = has(g.name);
                const picked = adding.includes(g.name);
                return (
                  <button
                    key={g.name}
                    type="button"
                    role="checkbox"
                    aria-checked={already || picked}
                    disabled={already || busy}
                    className={`check-row${already || picked ? " on" : ""}`}
                    onClick={() =>
                      setAdding((v) =>
                        v.includes(g.name) ? v.filter((x) => x !== g.name) : [...v, g.name],
                      )
                    }
                  >
                    <span className="checkbox" aria-hidden>
                      {(already || picked) && <Check size={11} weight="bold" />}
                    </span>
                    <span>{g.name}</span>
                    <span className="muted check-note">
                      {already ? "already in it" : picked ? "will be added" : ""}
                    </span>
                  </button>
                );
              })}
            </div>
            {available.length === 0 && (
              <div className="muted">
                No groups are declared on {member.node}. A group comes with the app that needs it —
                install one first.
              </div>
            )}
            <div className="note">
              A group can be added here and not taken away: the provisioning job adds a membership
              it is missing and never removes one, so a plan that dropped a group would apply and
              leave them in it. Remove one in the directory itself.
            </div>
          </div>

          <ErrorLine error={error} />
          <div className="row">
            <button className="btn btn-primary" disabled={!dirty || busy} onClick={save}>
              {busy ? "Proposing…" : "Propose change"}
            </button>
            <button className="btn btn-ghost" disabled={busy} onClick={onClose}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </Drawer>
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
