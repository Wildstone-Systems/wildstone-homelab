import { useState } from "react";
import { api, IdentityNotification } from "../api";
import { ErrorLine, useLoad } from "../lib";

// Identity (F78, ADR-W020 A6): read back the one-time code Authelia has
// just issued, so an admin can hand it to the person waiting for it.
//
// This exists because two-factor could not be ENROLLED without it. There
// is no mail relay and there will not be one, so Authelia delivers a
// code by writing a file — and that file lived inside a container,
// readable only with root and docker. The person the factor protects
// could not reach it, which is a lockout rather than an inconvenience.
//
// Nothing here generates or resends. The code is requested by the person
// signing in; this only lets somebody read what was already produced,
// which is exactly A6's out-of-band human delivery with a surface to
// deliver from.
export function IdentityView() {
  const nodes = useLoad(() => api.nodes());
  const [node, setNode] = useState("");
  const [note, setNote] = useState<IdentityNotification | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const list = nodes.data?.nodes ?? [];
  const chosen = node || list[0]?.id || "";

  async function read() {
    setBusy(true);
    setError(null);
    try {
      setNote(await api.identityCode(chosen));
    } catch (e) {
      setNote(null);
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <div className="sub">
        When someone sets up two-factor or resets a password, the code is
        delivered here rather than by email — read it and pass it on.
      </div>
      <ErrorLine error={nodes.error ?? error} />
      <div className="row">
        <label>node</label>
        <select value={chosen} onChange={(e) => setNode(e.target.value)}>
          {list.map((n) => (
            <option key={n.id} value={n.id}>
              {n.id}
            </option>
          ))}
        </select>
        <button onClick={read} disabled={busy || !chosen}>
          {busy ? "reading…" : "Read the latest code"}
        </button>
      </div>

      {note && (
        <div className="card">
          {note.code ? (
            <>
              <h3 style={{ letterSpacing: "0.2em", fontSize: "1.6em" }}>{note.code}</h3>
              <div className="muted">{note.note}</div>
            </>
          ) : (
            <div className="muted">
              No code is waiting. Ask the person to request one, then read again.
            </div>
          )}
          {note.body && (
            <details>
              <summary>the whole notification</summary>
              <pre style={{ whiteSpace: "pre-wrap" }}>{note.body}</pre>
            </details>
          )}
        </div>
      )}
    </>
  );
}
