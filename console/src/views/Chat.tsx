import { useState } from "react";
import { api, ChatBinding, ChatClaim, ButlerSurface } from "../api";
import { Badge, ErrorLine, useLoad, when } from "../lib";

// Chat (ADR-W026 A4): who may talk to a Butler, and as whom.
//
// The screen is an access list plus one button. Issuing a code is the
// only way a binding is ever created, and the code is shown exactly
// once — the server keeps only its hash, so there is no "show it again".
export function Chat() {
  const [showRevoked, setShowRevoked] = useState(false);
  const bindings = useLoad<ChatBinding[]>(() => api.chatBindings(showRevoked), [showRevoked]);
  const [surface, setSurface] = useState<ButlerSurface>("family");
  const [identity, setIdentity] = useState("");
  const [transport, setTransport] = useState("");
  const [issued, setIssued] = useState<ChatClaim | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const issue = () => {
    setBusy(true);
    setError(null);
    api
      .issueChatClaim(surface, identity.trim(), transport.trim())
      .then((claim) => {
        setIssued(claim);
        setIdentity("");
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  const revoke = (b: ChatBinding) => {
    setError(null);
    api
      .revokeChatBinding(b.id)
      .then(() => bindings.reload())
      .catch((e: Error) => setError(e.message));
  };

  return (
    <>
      <h2>Chat</h2>
      <div className="sub">
        A chat account reaches a Butler only through a binding you created here. Bindings are
        per-surface: an account bound to the household Butler cannot reach the operator one, and
        neither can approve a plan — approval happens in this Console.
      </div>
      <ErrorLine error={error} />

      <div className="card">
        <h3>Invite an account</h3>
        <div className="row">
          <select value={surface} onChange={(e) => setSurface(e.target.value as ButlerSurface)}>
            <option value="family">Household butler</option>
            <option value="admin">Operator butler</option>
          </select>
          <input
            type="text"
            placeholder={surface === "admin" ? "principal, e.g. mika" : "household member, e.g. sam"}
            value={identity}
            onChange={(e) => setIdentity(e.target.value)}
          />
          <input
            type="text"
            placeholder="transport (optional), e.g. telegram"
            value={transport}
            onChange={(e) => setTransport(e.target.value)}
          />
          <button
            className="action primary"
            disabled={busy || identity.trim() === ""}
            onClick={issue}
          >
            {busy ? "Issuing…" : "Issue code"}
          </button>
        </div>
        {issued && <IssuedCode claim={issued} />}
      </div>

      <h3 style={{ fontSize: 13, margin: "20px 0 8px" }}>Bindings</h3>
      <div className="row">
        <button className="action" onClick={() => setShowRevoked(!showRevoked)}>
          {showRevoked ? "Live only" : "Show revoked"}
        </button>
      </div>
      <ErrorLine error={bindings.error} />
      <table>
        <thead>
          <tr>
            <th>transport</th>
            <th>account</th>
            <th>surface</th>
            <th>as</th>
            <th>bound</th>
            <th>last used</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(bindings.data ?? []).map((b) => (
            <tr key={b.id}>
              <td>{b.transport}</td>
              <td>
                {b.account}
                {b.displayName && <span className="muted"> · {b.displayName}</span>}
              </td>
              <td>
                <Badge value={b.surface} />
              </td>
              <td>{b.identity}</td>
              <td>{when(b.createdAt)}</td>
              <td>{b.lastUsedAt ? when(b.lastUsedAt) : <span className="muted">never</span>}</td>
              <td>
                {b.revokedAt ? (
                  <span className="muted">revoked {when(b.revokedAt)}</span>
                ) : (
                  <button className="action" onClick={() => revoke(b)}>
                    Revoke
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {!bindings.loading && (bindings.data ?? []).length === 0 && (
        <div className="muted">
          No accounts are bound. Issue a code above, then have the person send it to the bot as a
          direct message.
        </div>
      )}
    </>
  );
}

// The code is shown once and never again — the same contract a minted
// token has, and the screen says so rather than letting someone assume
// they can come back for it.
export function IssuedCode({ claim }: { claim: ChatClaim }) {
  return (
    <div style={{ marginTop: 10 }}>
      <pre>{claim.code}</pre>
      <div className="muted">{claim.note}</div>
      <div className="muted">
        This is the only time the code is shown. If it is lost, issue another one.
      </div>
    </div>
  );
}
