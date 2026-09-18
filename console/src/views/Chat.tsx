import { useEffect, useState } from "react";
import { ChatsCircle, Prohibit } from "@phosphor-icons/react";
import { api, ChatBinding, ChatClaim, ChatTransport } from "../api";
import { ago, ErrorLine, until, useAction, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";

// Chat (ADR-W026 A4, ADR-W027): who may talk to a Butler, and as whom.
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
  const transports = useLoad(() => api.chatTransports(), []);
  const members = useLoad(() => api.householdMembers(), []);

  return (
    <ChatView
      bindings={bindings.data ?? []}
      transports={transports.data ?? []}
      members={(members.data ?? []).map((m) => ({
        value: m.username,
        label: m.displayName ? `${m.username} · ${m.displayName}` : m.username,
      }))}
      showRevoked={showRevoked}
      onToggleRevoked={() => setShowRevoked((v) => !v)}
      loading={bindings.loading}
      reload={bindings.reload}
      error={bindings.error ?? transports.error ?? members.error}
    />
  );
}

export interface Invitee {
  value: string;
  label: string;
}

export function ChatView({
  bindings,
  transports,
  members,
  showRevoked,
  onToggleRevoked,
  loading,
  reload,
  error,
}: {
  bindings: ChatBinding[];
  transports: ChatTransport[];
  members: Invitee[];
  showRevoked: boolean;
  onToggleRevoked: () => void;
  loading?: boolean;
  reload: () => void;
  error?: string | null;
}) {
  const [butler, setButler] = useState("");
  const [identity, setIdentity] = useState("");
  const [transport, setTransport] = useState("");
  const [issued, setIssued] = useState<ChatClaim | null>(null);
  const issue = useAction();

  // An invite is to ONE agent's bot (ADR-W033 B7), so the agents worth
  // offering are the ones a credentialed bot already answers for — an
  // agent with no bot has no connection for an account to be admitted
  // to, and a code issued for it could never be redeemed.
  const agents = [...new Set(transports.filter((t) => t.butler).map((t) => t.butler as string))].sort();
  const who = butler || agents[0] || "";
  // Always people: a Connection admits a Person (ADR-W033 A2/B9), and a
  // chat account bound to a machine credential had nobody behind it.
  const identities = members;
  const usable = transports.filter((t) => t.installed && t.butler === who);

  // Keep the selects on a real option as the agent changes, so the form
  // never submits a transport that agent has no bot on.
  useEffect(() => {
    setIssued(null);
    setIdentity((prev) => (identities.some((i) => i.value === prev) ? prev : identities[0]?.value ?? ""));
    setTransport((prev) => (usable.some((t) => t.transport === prev) ? prev : usable[0]?.transport ?? ""));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [who, members, transports]);

  const chosen = usable.find((t) => t.transport === transport);

  return (
    <>
      <div className="sub">
        A chat account reaches a Butler only through a binding you created here. Bindings are per
        agent: an account bound to one agent's bot reaches that agent and no other, and no binding
        can approve a plan — approval happens in this Console.
      </div>
      <ErrorLine error={error ?? null} />

      <div className="card">
        <h3>Invite an account</h3>
        <div className="row">
          <select
            value={who}
            aria-label="butler"
            disabled={agents.length === 0}
            onChange={(e) => setButler(e.target.value)}
          >
            {agents.length === 0 && <option value="">no agent has a bot yet</option>}
            {agents.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
          <select
            value={identity}
            aria-label="identity"
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
            aria-label="transport"
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
            className="btn btn-primary"
            disabled={
              issue.state.phase === "busy" || who === "" || identity === "" || transport === ""
            }
            onClick={() =>
              issue.run(async () => {
                setIssued(await api.issueChatClaim(who, identity, transport));
              }, "issued")
            }
          >
            {issue.state.phase === "busy" ? "Issuing…" : "Issue code"}
          </button>
        </div>
        <div className="muted">
          The binding reaches {who || "that agent"} and nothing else. What it can do is whatever that
          agent can do, and no binding can approve a plan. An owner agent admits owners only, so an
          invite for anybody else is refused.
        </div>
        <Prerequisites
          butler={who}
          usable={usable}
          chosen={chosen}
          allTransports={transports}
          haveIdentities={identities.length > 0}
        />
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

// Prerequisites says what is missing rather than leaving an empty
// dropdown to be interpreted. An invite needs a person to invite and a
// bot to send the code to, and neither is this screen's job to create.
export function Prerequisites({
  butler,
  usable,
  chosen,
  allTransports,
  haveIdentities,
}: {
  butler: string;
  usable: ChatTransport[];
  chosen?: ChatTransport;
  allTransports: ChatTransport[];
  haveIdentities: boolean;
}) {
  const notes: string[] = [];
  if (!haveIdentities) {
    notes.push(
      "No household members yet. Add one on the Household screen — a member is a declared account, and the code binds a chat account to that person.",
    );
  }
  if (butler === "") {
    const spare = allTransports
      .filter((t) => !t.butler)
      .map((t) => t.capability)
      .join(", ");
    notes.push(
      "No agent has a bot yet, so there is nowhere for a code to be sent." +
        (spare
          ? ` Give one of these to an agent on the Connect chat screen: ${spare}.`
          : " Set one up on the Connect chat screen."),
    );
  } else if (usable.length === 0) {
    notes.push(
      `${butler} has no installed chat adapter, so there is no bot for a code to be sent to. Finish its install on the Connect chat screen.`,
    );
  } else if (chosen && !chosen.credentialed) {
    notes.push(
      `The ${chosen.transport} adapter (${chosen.capability}) has no Keystone token yet, so it cannot redeem a code. Give it one with \`wild chat adapter ${chosen.capability}\`.`,
    );
  }
  if (notes.length === 0) return null;
  return (
    <div className="callout callout-warn">
      {notes.map((n) => (
        <div key={n}>{n}</div>
      ))}
    </div>
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
        {/* The agent this account reaches, by name. A class — "Household"
            or "Operator" — would put every owner agent's bindings under
            one label, and the whole point of a connection belonging to
            one agent is that Alfred's reach is not Robert's. */}
        <StatusPill kind="mute">{binding.butler}</StatusPill>
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
// token has, and the same one the Identity screen's code has.
//
// Which of the two things gets the display treatment depends on what
// there is to hand over. On a transport that carries a payload the LINK
// is the invite — tapping it opens the bot with the code already in it
// — so it is shown in the code's place rather than beside it: two
// things to hand over invites handing over the wrong one. A bare code
// is read aloud or typed by hand, so that one gets the size and the
// letter spacing; a URL gets neither, because neither helps.
export function IssuedCode({ claim }: { claim: ChatClaim }) {
  return (
    <div style={{ marginTop: "var(--space-4)" }}>
      <div className="sect">
        <span className="eyebrow">{claim.link ? "Invite link" : "One-time code"}</span>
        <span className="fade" />
        {claim.expiresAt && (
          <StatusPill kind="warn" title={when(claim.expiresAt)}>
            expires {until(claim.expiresAt)}
          </StatusPill>
        )}
      </div>
      {claim.link ? (
        <pre className="invite-link">{claim.link}</pre>
      ) : (
        <div className="identity-code tnum">{claim.code}</div>
      )}
      <div className="muted">{claim.note}</div>
      <div className="callout callout-warn">
        This is the only time it is shown. The server keeps only the code's hash, so there is no
        way to read it again — if it is lost, issue another one.
      </div>
    </div>
  );
}
