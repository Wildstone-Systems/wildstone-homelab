import { useState } from "react";
import { ChatsCircle, Prohibit } from "@phosphor-icons/react";
import {
  api,
  ButlerRow,
  ChatBinding,
  ChatBot,
  ChatPlatform,
  ChatTransport,
  ConnectionCreated,
  InviteKey,
  NodeInfo,
} from "../api";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";

// Chat (ADR-W033 B7, amended B7.1): the one screen for letting people
// talk to Agents from a chat app, and seeing who can.
//
// It used to be two — "Connect chat" to set bots up and "Chat" for the
// access list — which were two halves of one job and made the Owner
// hold the other half in their head. Here, top to bottom: the bots each
// Agent has, connecting somebody (onto an Agent's existing bot, or a new
// one), the people bound, and the Owner's own waiting invites.
//
// Nothing here issues a key. The Agent tells the Person there is a
// connection waiting, and they collect their single-use key on their
// own signed-in page, so the Owner never holds it.
export function Chat() {
  const bots = useLoad(() => api.chatBots(), []);
  const transports = useLoad(() => api.chatTransports(), []);
  const [showRevoked, setShowRevoked] = useState(false);
  const bindings = useLoad<ChatBinding[]>(() => api.chatBindings(showRevoked), [showRevoked]);
  const reloadAll = () => {
    bots.reload();
    transports.reload();
    bindings.reload();
  };
  return (
    <>
      <div className="sub">
        People talk to your agents from Telegram, Discord, Slack or Matrix through a bot. Each bot
        answers for one agent, and one bot can serve many people. Nobody can approve a plan from chat;
        that happens here in the Console.
      </div>
      <ErrorLine error={bots.error ?? transports.error ?? bindings.error} />
      <BotList bots={bots.data?.bots ?? []} transports={transports.data ?? []} onChanged={reloadAll} />
      <ConnectForm bots={bots.data?.bots ?? []} onDone={reloadAll} />
      <ChatView
        bindings={bindings.data ?? []}
        showRevoked={showRevoked}
        onToggleRevoked={() => setShowRevoked((v) => !v)}
        loading={bindings.loading}
        reload={bindings.reload}
      />
      <MyInvites />
    </>
  );
}

// BotList is every bot and the Agent it answers for, with whether its
// adapter has checked in, and the way to remove one.
export function BotList({
  bots,
  transports,
  onChanged,
}: {
  bots: ChatBot[];
  transports: ChatTransport[];
  onChanged: () => void;
}) {
  return (
    <div className="card">
      <h3>Bots</h3>
      {bots.length === 0 ? (
        <div className="muted">No bots yet. Connect someone below and one is made for the agent.</div>
      ) : (
        bots.map((b) => (
          <BotRow key={b.capability} bot={b} status={transports.find((t) => t.capability === b.capability)} onChanged={onChanged} />
        ))
      )}
    </div>
  );
}

function BotRow({ bot, status, onChanged }: { bot: ChatBot; status?: ChatTransport; onChanged: () => void }) {
  const remove = useAction();
  const [confirming, setConfirming] = useState(false);
  return (
    <div className="row">
      <span style={{ flex: 1 }}>
        <strong>{bot.butler}</strong> on {bot.label}
        {status?.handle && <span className="muted"> · @{status.handle}</span>}
        <span className="muted"> · {bot.capability}</span>
      </span>
      <StatusPill kind={status?.online ? "ok" : "mute"}>{status?.online ? "online" : "not checked in"}</StatusPill>
      {confirming ? (
        <>
          <span className="muted">Everyone on this bot loses it.</span>
          <button
            className="btn btn-danger"
            disabled={remove.state.phase === "busy"}
            onClick={() => remove.run(() => api.removeChatBot(bot.capability).then(onChanged), "removed")}
          >
            {remove.state.phase === "busy" ? "Removing…" : "Remove bot"}
          </button>
          <button className="btn btn-ghost" onClick={() => setConfirming(false)}>
            Keep it
          </button>
        </>
      ) : (
        <button className="btn btn-ghost" onClick={() => setConfirming(true)}>
          Remove…
        </button>
      )}
      {remove.state.phase === "error" && <ErrorLine error={remove.state.message} />}
    </div>
  );
}

// ConnectForm connects a Person to an Agent. If the Agent already has a
// bot on the app, the Person is invited to it and no token is asked for:
// a second bot on the same token would be the same bot polled twice.
// "Add a separate bot" is there for an Agent that should have two.
export function ConnectForm({ bots, onDone }: { bots: ChatBot[]; onDone: () => void }) {
  const platforms = useLoad(() => api.chatPlatforms(), []);
  const butlers = useLoad(() => api.butlers(), []);
  const members = useLoad(() => api.butlerMembers(), []);
  const nodes = useLoad(() => api.nodes(), []);

  const list = platforms.data?.platforms ?? [];
  const [platformName, setPlatformName] = useState("");
  const platform: ChatPlatform | undefined = list.find((p) => p.name === platformName) ?? list[0];

  const agents = (butlers.data?.butlers ?? []).map((r: ButlerRow) => r.butler).filter((b) => b.enabled !== false);
  const [butler, setButler] = useState("");
  const agent = agents.find((a) => a.name === butler) ?? agents[0];
  const ownerAgent = agent?.surface === "admin";

  const existing = bots.filter((b) => b.butler === agent?.name && b.platform === platform?.name);
  const [newBot, setNewBot] = useState(false);
  const [bot, setBot] = useState("");
  const useExisting = existing.length > 0 && !newBot;

  // An Owner Agent's bots admit Owners only (B9).
  const people = (members.data?.members ?? [])
    .filter((m) => m.active)
    .map((m) => m.member)
    .filter((m) => !ownerAgent || m.role === "owner");
  const [person, setPerson] = useState("");
  const who = people.find((p) => p.member === person) ?? people[0];

  const nodeList: NodeInfo[] = nodes.data?.nodes ?? [];
  const [node, setNode] = useState("");

  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<ConnectionCreated | null>(null);

  const credentialsReady = useExisting || (platform?.fields.every((f) => (values[f.key] ?? "").trim() !== "") ?? false);
  const ready = platform && agent && who && credentialsReady;
  const reset = () => {
    setValues({});
    setDone(null);
    setError(null);
    setNewBot(false);
    setBot("");
  };
  const submit = () => {
    if (!platform || !agent || !who) return;
    setBusy(true);
    setError(null);
    api
      .createConnection({
        platform: platform.name,
        butler: agent.name,
        person: who.member,
        credentials: useExisting ? {} : values,
        bot: useExisting ? bot || existing[0]?.capability : undefined,
        node: !useExisting && nodeList.length > 1 ? node || nodeList[0]?.id : undefined,
      })
      .then((r) => {
        setDone(r);
        setValues({});
        setNewBot(false);
        onDone();
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <div className="card">
      <h3>Connect someone</h3>
      <ErrorLine error={platforms.error ?? butlers.error ?? members.error ?? nodes.error} />
      <div className="seg" role="tablist" aria-label="Chat app">
        {list.map((p) => (
          <button
            key={p.name}
            role="tab"
            aria-selected={p.name === platform?.name}
            className={p.name === platform?.name ? "on" : ""}
            onClick={() => {
              setPlatformName(p.name);
              reset();
            }}
          >
            {p.label}
          </button>
        ))}
      </div>

      <div className="field" style={{ marginTop: "var(--space-4)" }}>
        <label htmlFor="connect-agent">Agent</label>
        <select
          id="connect-agent"
          value={agent?.name ?? ""}
          onChange={(e) => {
            setButler(e.target.value);
            reset();
          }}
        >
          {agents.length === 0 && <option value="">no agents yet — make one on the Butlers screen</option>}
          {agents.map((a) => (
            <option key={a.name} value={a.name}>
              {a.name} ({a.surface === "admin" ? "owner agent" : "member agent"})
            </option>
          ))}
        </select>
      </div>

      <div className="field">
        <label htmlFor="connect-person">
          Person
          {ownerAgent && <span className="muted">An owner agent can only be connected to owners.</span>}
        </label>
        <select id="connect-person" value={who?.member ?? ""} onChange={(e) => setPerson(e.target.value)}>
          {people.length === 0 && <option value="">nobody who can use this agent</option>}
          {people.map((p) => (
            <option key={p.member} value={p.member}>
              {p.member}
            </option>
          ))}
        </select>
      </div>

      {useExisting ? (
        <div className="field">
          {existing.length > 1 ? (
            <>
              <label htmlFor="connect-bot">Bot</label>
              <select id="connect-bot" value={bot || existing[0].capability} onChange={(e) => setBot(e.target.value)}>
                {existing.map((b) => (
                  <option key={b.capability} value={b.capability}>
                    {b.capability}
                  </option>
                ))}
              </select>
            </>
          ) : (
            <div className="muted">
              {agent?.name} already has a {platform?.label} bot, so {who?.member ?? "they"} will be invited to it.
            </div>
          )}
          <button className="btn btn-ghost" onClick={() => setNewBot(true)}>
            Add a separate bot instead
          </button>
        </div>
      ) : (
        platform?.fields.map((f) => (
          <div className="field" key={platform.name + f.key}>
            <label htmlFor={`connect-${f.key}`}>
              {f.label}
              <span className="muted">{f.hint}</span>
            </label>
            <input
              id={`connect-${f.key}`}
              type={f.key === "homeserver" ? "url" : "password"}
              autoComplete="off"
              placeholder={f.prefix ? `${f.prefix}…` : undefined}
              value={values[f.key] ?? ""}
              onChange={(e) => setValues({ ...values, [f.key]: e.target.value })}
            />
          </div>
        ))
      )}

      {!useExisting && nodeList.length > 1 && (
        <div className="field">
          <label htmlFor="connect-node">Machine that runs the bot</label>
          <select id="connect-node" value={node || nodeList[0]?.id} onChange={(e) => setNode(e.target.value)}>
            {nodeList.map((n) => (
              <option key={n.id} value={n.id}>
                {n.id}
              </option>
            ))}
          </select>
        </div>
      )}

      <div className="row">
        <button className="btn btn-primary" disabled={!ready || busy} onClick={submit}>
          {busy ? "Connecting…" : useExisting ? "Invite" : "Connect"}
        </button>
      </div>
      <ErrorLine error={error} />
      {done && <div className="muted">{done.note}</div>}
      <div className="muted">
        The agent tells them there is a connection waiting, and they collect their one-time key on their
        own page: the household dashboard for a member agent, this Console for an owner agent.
      </div>
    </div>
  );
}

export interface Invitee {
  value: string;
  label: string;
}

// ChatView is the access list: every account bound to a bot, and the
// way to revoke one.
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
  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-8)" }}>
        <span className="eyebrow">People on chat</span>
        <span className="muted">{bindings.length}</span>
        <span className="fade" />
        <button className="btn btn-ghost" onClick={onToggleRevoked}>
          {showRevoked ? "Live only" : "Show revoked"}
        </button>
      </div>
      <div className="muted">
        Bindings are per agent: an account bound to one agent's bot reaches that agent and no other.
      </div>

      <table>
        <thead>
          <tr>
            <th>account</th>
            <th>app</th>
            <th>agent</th>
            <th>person</th>
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
          Nobody is connected yet. Connect someone above; once they send their key to the bot, they
          appear here.
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
        {/* The agent this account reaches, by name: a connection belongs
            to one agent, so Alfred's reach is not Robert's. */}
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
            revoked — that account can no longer reach an agent
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

// MyInvites is the signed-in Owner's own invites to owner agents: the
// Console is where those keys are collected, as the dashboard is for
// member agents.
export function MyInvites() {
  const invites = useLoad(() => api.myInvites(), []);
  const [keys, setKeys] = useState<Record<number, InviteKey>>({});
  const [error, setError] = useState<string | null>(null);
  const list = invites.data?.invites ?? [];
  if (list.length === 0) return null;
  const fetchKey = (seq: number) => {
    setError(null);
    api
      .myInviteKey(seq)
      .then((k) => setKeys({ ...keys, [seq]: k }))
      .catch((e: Error) => setError(e.message));
  };
  return (
    <div className="card" style={{ marginTop: "var(--space-8)" }}>
      <h3>Your invites</h3>
      {list.map((i) => {
        const k = keys[i.seq];
        return (
          <div key={i.seq} className="row">
            <span style={{ flex: 1 }}>
              {i.butler} on {i.label}
            </span>
            {k ? (
              k.carriesCode && k.link ? (
                <a className="btn btn-secondary" href={k.link} target="_blank" rel="noreferrer">
                  Open {i.label}
                </a>
              ) : (
                <code>{k.code}</code>
              )
            ) : (
              <button className="btn btn-secondary" onClick={() => fetchKey(i.seq)}>
                Get my key
              </button>
            )}
            {k && (
              <div className="muted" style={{ flexBasis: "100%" }}>
                {k.note}
              </div>
            )}
          </div>
        );
      })}
      <ErrorLine error={error ?? invites.error} />
    </div>
  );
}
