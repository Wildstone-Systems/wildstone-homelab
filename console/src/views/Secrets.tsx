import { useState } from "react";
import { Key } from "@phosphor-icons/react";
import { api, PackageView, SecretInfo } from "../api";
import { ErrorLine, useAction, useLoad, when } from "../lib";
import { useConsoleState } from "../state";
import { StatusPill } from "../ui/status";

// Secrets (ADR-W016 A2 / ADR-W012 / ADR-W027): name-level only — the API
// cannot read a value back, so neither can this screen.
//
// The addition here is "used by". A list of names says nothing about
// what rotating one will restart, and rotation re-creates dependents —
// so the column that answers "what does this belong to" is the one that
// makes the button safe to press.
export function Secrets() {
  const { snap } = useConsoleState();
  const nodes = snap?.nodes ?? [];
  const [node, setNode] = useState("");
  const chosen = node || nodes[0]?.id || "";

  return (
    <>
      <div className="sub">
        Admin-only; values are write-only by construction. A secret is per-node, so pick the node
        it lives on.
      </div>
      <div className="row">
        <label htmlFor="secret-node">node</label>
        <select id="secret-node" value={chosen} onChange={(e) => setNode(e.target.value)}>
          {nodes.map((n) => (
            <option key={n.id} value={n.id}>
              {n.id}
              {n.connected ? "" : " (disconnected)"}
            </option>
          ))}
        </select>
      </div>
      {nodes.length === 0 && (
        <div className="muted">No nodes have joined yet — pair one on Nodes first.</div>
      )}
      {chosen && <NodeSecrets key={chosen} node={chosen} />}
    </>
  );
}

// usedBy maps a secret name to the capabilities whose resources include
// it. Joined from /packages, which already lists every resource a
// capability owns — no endpoint was added for this.
export function usedBy(pkgs: PackageView[]): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const p of pkgs) {
    for (const r of p.resources ?? []) {
      if (r.kind !== "Secret") continue;
      out.set(r.name, [...(out.get(r.name) ?? []), p.capability]);
    }
  }
  return out;
}

// purposeOf maps a secret name to what its package says it is FOR.
//
// The table could say a name, a version and what rotating it would
// restart, and nothing about what any of it meant. "davis-admin-password"
// does not tell a household admin that it is the calendar service's admin
// login, that the username beside it is `admin`, or that the generated
// value cannot be read back — so the page it opens stayed shut and the
// answer lived in a YAML comment.
export function purposeOf(pkgs: PackageView[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const p of pkgs) {
    for (const r of p.resources ?? []) {
      if (r.kind !== "Secret" || !r.purpose) continue;
      out.set(r.name, r.purpose);
    }
  }
  return out;
}

function NodeSecrets({ node }: { node: string }) {
  const list = useLoad(() => api.nodeSecrets(node), [node]);
  const catalog = useLoad(() => api.packages(), []);
  if (list.error) return <ErrorLine error={list.error} />;
  return (
    <SecretsView
      node={node}
      secrets={list.data?.secrets ?? []}
      owners={usedBy(catalog.data ?? [])}
      purposes={purposeOf(catalog.data ?? [])}
      loading={list.loading}
      reload={list.reload}
    />
  );
}

export function SecretsView({
  node,
  secrets,
  owners,
  purposes,
  loading,
  reload,
}: {
  node: string;
  secrets: SecretInfo[];
  owners: Map<string, string[]>;
  purposes?: Map<string, string>;
  loading?: boolean;
  reload: () => void;
}) {
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const set = useAction();

  return (
    <>
      <div className="card">
        <h3>Add or set on {node}</h3>
        <div className="row">
          <input
            type="text"
            placeholder="secret name"
            aria-label="secret name"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <input
            id="secret-value"
            type="password"
            placeholder="value (empty = generate on the node)"
            aria-label="secret value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
          <button
            className="btn btn-primary"
            disabled={set.state.phase === "busy" || !name}
            onClick={() =>
              set.run(
                () =>
                  api.setSecret(node, name, value || undefined).then(() => {
                    setValue("");
                    reload();
                  }),
                value ? `set ${name}` : `generated ${name} on ${node}`,
              )
            }
          >
            {set.state.phase === "busy" ? "Working…" : value ? "Set" : "Generate"}
          </button>
          {set.state.phase === "done" && <StatusPill kind="ok">{set.state.message}</StatusPill>}
        </div>
        <div className="muted">
          A value typed here transits once and is never readable again — not by this screen, not
          by the API. That is why a generated password cannot be shown to you: to use one, set it
          to a value you choose. Whatever reads it is re-created so the new value takes effect.
        </div>
        {set.state.phase === "error" && <ErrorLine error={set.state.message} />}
      </div>

      <table>
        <thead>
          <tr>
            <th>name</th>
            <th>version</th>
            <th>used by</th>
            <th>updated</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {secrets.map((s) => (
            <SecretRow
              key={s.name}
              node={node}
              secret={s}
              owners={owners.get(s.name) ?? []}
              purpose={purposes?.get(s.name)}
              onSet={() => {
                setName(s.name);
                document.getElementById("secret-value")?.focus();
              }}
              reload={reload}
            />
          ))}
        </tbody>
      </table>
      {!loading && secrets.length === 0 && (
        <div className="muted">No secrets on this node yet.</div>
      )}
    </>
  );
}

function SecretRow({
  node,
  secret,
  owners,
  purpose,
  onSet,
  reload,
}: {
  node: string;
  secret: SecretInfo;
  owners: string[];
  purpose?: string;
  onSet: () => void;
  reload: () => void;
}) {
  const rotate = useAction();
  const done = rotate.state.phase === "done" ? rotate.state : null;
  return (
    <tr>
      <td>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
          <Key size={13} />
          {secret.name}
        </span>
        {/* What the name means. Without it the whole table is
            identifiers, and the operator who needs one of these is
            exactly the one who does not already know. */}
        {purpose && <div className="muted secret-purpose">{purpose}</div>}
      </td>
      <td className="tnum">v{secret.version}</td>
      {/* What rotating this will restart. Without it the button is a
          dare: rotation re-creates every container that depends on the
          value, and the list of names never said which those were. */}
      <td>
        {owners.length === 0 ? (
          <span className="muted">nothing installed uses it</span>
        ) : (
          <span className="chips">
            {owners.map((o) => (
              <span className="chip" key={o}>
                {o}
              </span>
            ))}
          </span>
        )}
      </td>
      <td className="muted" title={when(secret.updatedUnixSec ? new Date(secret.updatedUnixSec * 1000).toISOString() : undefined)}>
        {secret.updatedUnixSec
          ? new Date(secret.updatedUnixSec * 1000).toLocaleDateString()
          : "—"}
      </td>
      {/* row-actions keeps both buttons on one line and lets the column
          be exactly as wide as they need. Without it the second button
          wrapped underneath the first, doubling the height of every row
          in the table. */}
      <td className="row-actions">
        {done ? (
          <StatusPill kind="ok">{done.message}</StatusPill>
        ) : (
          <div className="actions">
          {/* Setting a value you choose is the ONLY way into a service
              whose password was generated, because no value can be read
              back. It needed to be a button on the row: retyping the
              name into the form above is how an operator ends up in a
              terminal instead. */}
          <button className="btn btn-secondary" onClick={onSet} title={`Set ${secret.name} to a value you choose`}>
            Set…
          </button>
          <button
            className="btn btn-danger"
            disabled={rotate.state.phase === "busy"}
            onClick={() =>
              rotate.run(
                () => api.rotateSecret(node, secret.name).then(reload),
                `rotated to v${secret.version + 1}`,
              )
            }
            title={
              owners.length > 0
                ? `Regenerates the value and re-creates ${owners.join(", ")}`
                : "Regenerates the value"
            }
          >
            {rotate.state.phase === "busy" ? "Rotating…" : "Rotate"}
          </button>
          </div>
        )}
        {rotate.state.phase === "error" && <ErrorLine error={rotate.state.message} />}
      </td>
    </tr>
  );
}
