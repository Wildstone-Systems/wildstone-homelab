import { useEffect, useState } from "react";
import { api, ChatBinding, ChatClaim, ChatTransport, ButlerSurface } from "../api";
import { Badge, ErrorLine, useLoad, when } from "../lib";

// Chat (ADR-W026 A4): who may talk to a Butler, and as whom.
//
// The screen is an access list plus one button. Issuing a code is the
// only way a binding is ever created, and the code is shown exactly
// once — the server keeps only its hash, so there is no "show it again".
//
// Nothing on this form is typed. Both halves of an invite have a closed
// set of real answers — the people this household has declared
// (ADR-W020 A3) and the chat adapters this deployment has installed
// (ADR-W026 A3) — and a code naming anything else looks exactly like a
// working one until the person it was issued for says nothing happened.
// The API refuses those; this form simply never offers them.
export function Chat() {
  const [showRevoked, setShowRevoked] = useState(false);
  const bindings = useLoad<ChatBinding[]>(() => api.chatBindings(showRevoked), [showRevoked]);
  const transports = useLoad(() => api.chatTransports());
  const members = useLoad(() => api.householdMembers());
  const principals = useLoad(() => api.principals());
  const [surface, setSurface] = useState<ButlerSurface>("family");
  const [identity, setIdentity] = useState("");
  const [transport, setTransport] = useState("");
  const [issued, setIssued] = useState<ChatClaim | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Who this surface can bind: a household member on the family
  // surface, a live principal on the operator's.
  const identities =
    surface === "family"
      ? (members.data ?? []).map((m) => ({
          value: m.username,
          label: m.displayName ? `${m.username} · ${m.displayName}` : m.username,
        }))
      : (principals.data ?? [])
          .filter((p) => !p.disabled)
          .map((p) => ({ value: p.name, label: `${p.name} · ${p.role}` }));

  const usable = (transports.data ?? []).filter((t) => t.installed && t.surface === surface);

  // Keep the two selects on a real option as the surface changes, so
  // the form never submits a stale identity from the other surface.
  useEffect(() => {
    setIssued(null);
    setIdentity((prev) => (identities.some((i) => i.value === prev) ? prev : identities[0]?.value ?? ""));
    setTransport((prev) => (usable.some((t) => t.transport === prev) ? prev : usable[0]?.transport ?? ""));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [surface, members.data, principals.data, transports.data]);

  const chosen = usable.find((t) => t.transport === transport);

  const issue = () => {
    setBusy(true);
    setError(null);
    api
      .issueChatClaim(surface, identity, transport)
      .then((claim) => setIssued(claim))
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
      <ErrorLine error={error ?? transports.error ?? members.error ?? principals.error} />

      <div className="card">
        <h3>Invite an account</h3>
        <div className="row">
          <select value={surface} onChange={(e) => setSurface(e.target.value as ButlerSurface)}>
            <option value="family">Household butler</option>
            <option value="admin">Operator butler</option>
          </select>
          <select
            value={identity}
            disabled={identities.length === 0}
            onChange={(e) => setIdentity(e.target.value)}
          >
            {identities.length === 0 && <option value="">nobody to invite</option>}
            {identities.map((i) => (
              <option key={i.value} value={i.value}>
                {i.label}
              </option>
            ))}
          </select>
          <select
            value={transport}
            disabled={usable.length === 0}
            onChange={(e) => setTransport(e.target.value)}
          >
            {usable.length === 0 && <option value="">no adapter installed</option>}
            {usable.map((t) => (
              <option key={t.capability} value={t.transport}>
                {t.transport}
              </option>
            ))}
          </select>
          <button
            className="action primary"
            disabled={busy || identity === "" || transport === ""}
            onClick={issue}
          >
            {busy ? "Issuing…" : "Issue code"}
          </button>
        </div>
        <Prerequisites
          surface={surface}
          usable={usable}
          chosen={chosen}
          allTransports={transports.data ?? []}
          haveIdentities={identities.length > 0}
        />
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

// Prerequisites says what is missing rather than leaving an empty
// dropdown to be interpreted. An invite needs a person to invite and a
// bot to send the code to, and neither is this screen's job to create.
export function Prerequisites({
  surface,
  usable,
  chosen,
  allTransports,
  haveIdentities,
}: {
  surface: ButlerSurface;
  usable: ChatTransport[];
  chosen?: ChatTransport;
  allTransports: ChatTransport[];
  haveIdentities: boolean;
}) {
  const notes: string[] = [];
  if (!haveIdentities) {
    notes.push(
      surface === "family"
        ? "No household members yet. Add one on the Household screen — a member is a declared account, and the code binds a chat account to that person."
        : "No principals to bind. Mint one before inviting an operator account.",
    );
  }
  if (usable.length === 0) {
    const available = allTransports
      .filter((t) => t.surface === surface)
      .map((t) => t.capability)
      .join(", ");
    notes.push(
      `No ${surface} chat adapter is installed, so there is no bot for a code to be sent to.` +
        (available ? ` Install one from Services: ${available}.` : ""),
    );
  } else if (chosen && !chosen.credentialed) {
    notes.push(
      `The ${chosen.transport} adapter (${chosen.capability}) has no Keystone token yet, so it cannot redeem a code. Give it one with \`wild chat adapter ${chosen.capability}\`.`,
    );
  }
  if (notes.length === 0) return null;
  return (
    <div className="muted" style={{ marginTop: 8 }}>
      {notes.map((n) => (
        <div key={n}>{n}</div>
      ))}
    </div>
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
