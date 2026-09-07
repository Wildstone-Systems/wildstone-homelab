import { useState } from "react";
import { ChatsCircle, Prohibit } from "@phosphor-icons/react";
import { api, ChatBinding, ChatClaim, ButlerSurface } from "../api";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";

// Chat (ADR-W026 A4, ADR-W027): who may talk to a Butler, and as whom.
//
// The screen is an access list plus one button. Issuing a code is the
// only way a binding is ever created, and the code is shown exactly
// once — the server keeps only its hash, so there is no "show it again".
export function Chat() {
  const [showRevoked, setShowRevoked] = useState(false);
  const bindings = useLoad<ChatBinding[]>(() => api.chatBindings(showRevoked), [showRevoked]);
  if (bindings.error) return <ErrorLine error={bindings.error} />;
  return (
    <ChatView
      bindings={bindings.data ?? []}
      showRevoked={showRevoked}
      onToggleRevoked={() => setShowRevoked((v) => !v)}
      loading={bindings.loading}
      reload={bindings.reload}
    />
  );
}

export function ChatView({
  bindings,
  showRevoked,
  onToggleRevoked,
  loading,
  reload,
}: {
  bindings: ChatBinding[];
  showRevoked: boolean;
  onToggleRevoked: () => void;
  loading?: boolean;
  reload: () => void;
}) {
  const [surface, setSurface] = useState<ButlerSurface>("family");
  const [identity, setIdentity] = useState("");
  const [transport, setTransport] = useState("");
  const [issued, setIssued] = useState<ChatClaim | null>(null);
  const issue = useAction();

  return (
    <>
      <div className="sub">
        A chat account reaches a Butler only through a binding you created here. Bindings are
        per-surface: an account bound to the household Butler cannot reach the operator one, and
        neither can approve a plan — approval happens in this Console.
      </div>

      <div className="card">
        <h3>Invite an account</h3>
        <div className="row">
          <select
            value={surface}
            aria-label="surface"
            onChange={(e) => setSurface(e.target.value as ButlerSurface)}
          >
            <option value="family">Household butler</option>
            <option value="admin">Operator butler</option>
          </select>
          <input
            type="text"
            aria-label="identity"
            placeholder={surface === "admin" ? "principal, e.g. mika" : "household member, e.g. sam"}
            value={identity}
            onChange={(e) => setIdentity(e.target.value)}
          />
          <input
            type="text"
            aria-label="transport"
            placeholder="transport (optional), e.g. telegram"
            value={transport}
            onChange={(e) => setTransport(e.target.value)}
          />
          <button
            className="btn btn-primary"
            disabled={issue.state.phase === "busy" || identity.trim() === ""}
            onClick={() =>
              issue.run(async () => {
                setIssued(await api.issueChatClaim(surface, identity.trim(), transport.trim()));
                setIdentity("");
              }, "issued")
            }
          >
            {issue.state.phase === "busy" ? "Issuing…" : "Issue code"}
          </button>
        </div>
        <div className="muted">
          {surface === "admin"
            ? "An operator binding lets that account ask the Butler about the home lab and receive its proposals. It still cannot approve one."
            : "A household binding reaches the family Butler only — no containers, storage, accounts or access rules."}
        </div>
        {issue.state.phase === "error" && <ErrorLine error={issue.state.message} />}
        {issued && <IssuedCode claim={issued} />}
      </div>

      <div className="sect" style={{ marginTop: "var(--space-8)" }}>
        <span className="eyebrow">Bindings</span>
        <span className="muted">{bindings.length}</span>
        <span className="fade" />
        <button className="btn btn-ghost" onClick={onToggleRevoked}>
          {showRevoked ? "Live only" : "Show revoked"}
        </button>
      </div>

      <table>
        <thead>
          <tr>
            <th>account</th>
            <th>transport</th>
            <th>butler</th>
            <th>speaks as</th>
            <th>bound</th>
            <th>last used</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {bindings.map((b) => (
            <BindingRow key={b.id} binding={b} reload={reload} />
          ))}
        </tbody>
      </table>
      {!loading && bindings.length === 0 && (
        <div className="muted">
          No accounts are bound. Issue a code above, then have the person send it to the bot as a
          direct message.
        </div>
      )}
    </>
  );
}

function BindingRow({ binding, reload }: { binding: ChatBinding; reload: () => void }) {
  const revoke = useAction();
  const done = revoke.state.phase === "done";
  const dead = binding.revokedAt !== undefined || done;
  return (
    <tr style={dead ? { opacity: 0.55 } : undefined}>
      <td>
        <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
          <ChatsCircle size={13} />
          {binding.account}
        </span>
        {binding.displayName && <div className="muted">{binding.displayName}</div>}
      </td>
      <td>{binding.transport}</td>
      <td>
        {/* Which butler, in the words the rest of the console uses —
            "family" is the wire value, "Household" is what the surface
            switch on the Butler screen calls it. */}
        <StatusPill kind="mute">
          {binding.surface === "family" ? "Household" : "Operator"}
        </StatusPill>
      </td>
      <td>{binding.identity}</td>
      <td className="muted" title={when(binding.createdAt)}>
        {ago(binding.createdAt)}
      </td>
      <td className="muted" title={binding.lastUsedAt ? when(binding.lastUsedAt) : undefined}>
        {binding.lastUsedAt ? ago(binding.lastUsedAt) : "never"}
      </td>
      <td>
        {binding.revokedAt ? (
          <StatusPill kind="bad" icon={<Prohibit size={12} />}>
            revoked {ago(binding.revokedAt)}
          </StatusPill>
        ) : done ? (
          <StatusPill kind="bad" icon={<Prohibit size={12} />}>
            revoked — that account can no longer reach a Butler
          </StatusPill>
        ) : (
          <button
            className="btn btn-danger"
            disabled={revoke.state.phase === "busy"}
            onClick={() => revoke.run(() => api.revokeChatBinding(binding.id).then(reload), "revoked")}
          >
            {revoke.state.phase === "busy" ? "Revoking…" : "Revoke"}
          </button>
        )}
        {revoke.state.phase === "error" && <ErrorLine error={revoke.state.message} />}
      </td>
    </tr>
  );
}

// The code is shown once and never again — the same contract a minted
// token has, and the same one the Identity screen's code has. It is set
// at display size for the reason both of those are: it is going to be
// read aloud or typed by hand.
export function IssuedCode({ claim }: { claim: ChatClaim }) {
  return (
    <div style={{ marginTop: "var(--space-4)" }}>
      <div className="identity-code tnum">{claim.code}</div>
      <div className="muted">{claim.note}</div>
      <div className="callout callout-warn">
        This is the only time the code is shown. The server keeps only its hash, so there is no
        way to read it again — if it is lost, issue another one.
      </div>
    </div>
  );
}
