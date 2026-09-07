import { useState } from "react";
import { api } from "../api";
import { ErrorLine, useLoad } from "../lib";

// Secrets (ADR-W016 A2 / ADR-W012): name-level only — the API cannot
// read a value back, so neither can this screen. Set values transit
// once; rotation re-creates dependents.
export function Secrets() {
  const [node, setNode] = useState("");
  const [active, setActive] = useState<string | null>(null);
  return (
    <>
      <div className="sub">
        Admin-only; values are write-only by construction. Pick the node to manage.
      </div>
      <div className="row">
        <input
          type="text"
          placeholder="node id (e.g. n1)"
          value={node}
          onChange={(e) => setNode(e.target.value)}
        />
        <button className="btn btn-secondary" disabled={!node} onClick={() => setActive(node)}>
          Load
        </button>
      </div>
      {active && <NodeSecrets node={active} />}
    </>
  );
}

function NodeSecrets({ node }: { node: string }) {
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const list = useLoad(() => api.nodeSecrets(node), [node]);
  if (list.error) return <ErrorLine error={list.error} />;
  const secrets = list.data?.secrets ?? [];

  const run = (p: Promise<unknown>) => {
    setBusy(true);
    setActionError(null);
    p.then(() => {
      setValue("");
      list.reload();
    })
      .catch((e: Error) => setActionError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <div className="card">
        <h3>Add or set on {node}</h3>
        <div className="row">
          <input
            type="text"
            placeholder="secret name"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <input
            type="password"
            placeholder="value (empty = generate on the node)"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
          <button
            className="btn btn-primary"
            disabled={busy || !name}
            onClick={() => run(api.setSecret(node, name, value || undefined))}
          >
            {value ? "Set" : "Generate"}
          </button>
        </div>
        <ErrorLine error={actionError} />
      </div>
      <table>
        <thead>
          <tr>
            <th>name</th>
            <th>version</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {secrets.map((s) => (
            <tr key={s.name}>
              <td>{s.name}</td>
              <td>v{s.version}</td>
              <td>
                <button
                  className="btn btn-danger"
                  disabled={busy}
                  onClick={() => run(api.rotateSecret(node, s.name))}
                  title="Regenerates the value and re-creates dependent containers"
                >
                  Rotate
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {secrets.length === 0 && <div className="muted">No secrets on this node yet.</div>}
    </>
  );
}
