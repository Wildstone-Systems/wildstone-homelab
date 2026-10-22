import { useState } from "react";
import { IdentificationCard } from "@phosphor-icons/react";
import { api, IdentityNotification } from "../api";
import { ago, ErrorLine, useAction } from "../lib";
import { useConsoleState } from "../state";
import { StatusPill } from "../ui/status";

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
  const { snap } = useConsoleState();
  const nodes = snap?.nodes ?? [];
  const [node, setNode] = useState("");
  const [note, setNote] = useState<IdentityNotification | null>(null);
  const [readAt, setReadAt] = useState<Date | null>(null);
  const read = useAction();
  const chosen = node || nodes[0]?.id || "";

  return (
    <>
      <div className="sub">
        When someone sets up two-factor or resets a password, the code is delivered here rather
        than by email — read it and pass it on, in person or over a channel you already trust.
      </div>
      <div className="row">
        <label htmlFor="identity-node">node</label>
        <select id="identity-node" value={chosen} onChange={(e) => setNode(e.target.value)}>
          {nodes.map((n) => (
            <option key={n.id} value={n.id}>
              {n.id}
            </option>
          ))}
        </select>
        <button
          className="btn btn-primary"
          onClick={() =>
            read.run(async () => {
              setNote(await api.identityCode(chosen));
              setReadAt(new Date());
            }, "read")
          }
          disabled={read.state.phase === "busy" || !chosen}
        >
          {read.state.phase === "busy" ? "Reading…" : "Read the latest code"}
        </button>
      </div>
      {nodes.length === 0 && (
        <div className="muted">No nodes have joined yet — pair one on Nodes first.</div>
      )}
      {read.state.phase === "error" && <ErrorLine error={read.state.message} />}
      {note && <IdentityCode note={note} readAt={readAt} />}
    </>
  );
}

export function IdentityCode({
  note,
  readAt,
}: {
  note: IdentityNotification;
  readAt: Date | null;
}) {
  if (!note.code && !note.resetLink) {
    return (
      <div className="card">
        <div className="muted">
          {note.body
            ? "The latest notification holds no code or reset link. It is shown below as it was written."
            : "Nothing is waiting on this node. Ask the person to request a code or reset, then read again."}
        </div>
        {note.body && <pre>{note.body}</pre>}
      </div>
    );
  }
  return (
    <div className="card">
      <div className="sect">
        <span className="eyebrow">
          <IdentificationCard size={13} style={{ verticalAlign: "-2px", marginRight: 5 }} />
          {note.code ? "One-time code" : "Password reset link"}
        </span>
        <span className="fade" />
        {/* The code is whatever the last read found. It has no expiry on
            the wire, so this says how old the READ is rather than
            inventing a countdown: a code that looks fresh and is not is
            worse than one labelled honestly. */}
        {readAt && <StatusPill kind="mute">read {ago(readAt)}</StatusPill>}
      </div>
      {note.code && <div className="identity-code tnum">{note.code}</div>}
      {/* The body's copy of this link is redacted and cannot complete a
          reset; this one is whole (#463). Shown as text to copy, not a
          link to follow: opening it here would spend it. */}
      {note.resetLink && <pre className="identity-link">{note.resetLink}</pre>}
      <div className="muted">{note.note}</div>
      {note.body && (
        <details>
          <summary className="muted">the whole notification</summary>
          <pre>{note.body}</pre>
        </details>
      )}
    </div>
  );
}
