import { useState } from "react";
import { api, ChatPlatform, ConnectionCreated, InviteKey } from "../api";
import { ErrorLine, useLoad } from "../lib";

// Connect someone to an Agent over chat (ADR-W033 B7, amended B7.1).
//
// Three answers: which Agent, which Person, and the platform's
// credential. Keystone does the rest — mints the bot's own credential,
// stores the platform's, installs the bot as its own instance and has
// the Agent tell the Person where to collect their key. The key is never
// shown here: the Person fetches it on their own signed-in page, so the
// Owner setting this up never holds it.
export function ConnectChat() {
  const platforms = useLoad(() => api.chatPlatforms());
  const butlers = useLoad(() => api.butlers());
  const members = useLoad(() => api.butlerMembers());
  const nodes = useLoad(() => api.nodes());

  const list = platforms.data?.platforms ?? [];
  const [platformName, setPlatformName] = useState("");
  const platform: ChatPlatform | undefined = list.find((p) => p.name === platformName) ?? list[0];

  const agents = (butlers.data?.butlers ?? []).map((r) => r.butler).filter((b) => b.enabled !== false);
  const [butler, setButler] = useState("");
  const agent = agents.find((a) => a.name === butler) ?? agents[0];
  const ownerAgent = agent?.surface === "admin";

  // An Owner Agent's Connections admit Owners only (B9), so only Owners
  // are offered for one.
  const people = (members.data?.members ?? [])
    .filter((m) => m.active)
    .map((m) => m.member)
    .filter((m) => !ownerAgent || m.role === "owner");
  const [person, setPerson] = useState("");
  const who = people.find((p) => p.member === person) ?? people[0];

  const nodeList = nodes.data?.nodes ?? [];
  const [node, setNode] = useState("");

  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<ConnectionCreated | null>(null);

  const ready = platform && agent && who && platform.fields.every((f) => (values[f.key] ?? "").trim() !== "");
  const submit = () => {
    if (!platform || !agent || !who) return;
    setBusy(true);
    setError(null);
    api
      .createConnection({
        platform: platform.name,
        butler: agent.name,
        person: who.member,
        credentials: values,
        node: nodeList.length > 1 ? node || nodeList[0]?.id : undefined,
      })
      .then((r) => {
        setDone(r);
        setValues({});
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <div className="sub">
        A connection lets one person talk to one of your agents from a chat app. Pick the app, the agent
        and the person, and paste the bot's credential. The agent then tells them where to collect their
        one-time key.
      </div>
      <ErrorLine error={platforms.error ?? butlers.error ?? members.error ?? nodes.error} />

      <div className="card">
        <h3>New connection</h3>
        <div className="seg" role="tablist" aria-label="Chat app">
          {list.map((p) => (
            <button
              key={p.name}
              role="tab"
              aria-selected={p.name === platform?.name}
              className={p.name === platform?.name ? "on" : ""}
              onClick={() => {
                setPlatformName(p.name);
                setValues({});
                setDone(null);
              }}
            >
              {p.label}
            </button>
          ))}
        </div>

        <div className="field" style={{ marginTop: "var(--space-4)" }}>
          <label htmlFor="connect-agent">Agent</label>
          <select id="connect-agent" value={agent?.name ?? ""} onChange={(e) => setButler(e.target.value)}>
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

        {platform?.fields.map((f) => (
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
        ))}

        {nodeList.length > 1 && (
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
            {busy ? "Connecting…" : "Connect"}
          </button>
        </div>
        <ErrorLine error={error} />
        {done && <div className="muted">{done.note}</div>}
      </div>

      <MyInvites />
    </>
  );
}

// MyInvites is the signed-in Owner's own invites to Owner Agents: the
// Console is where those are collected, as the dashboard is for Member
// Agents.
function MyInvites() {
  const invites = useLoad(() => api.myInvites());
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
    <div className="card">
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
            {k && <div className="muted" style={{ flexBasis: "100%" }}>{k.note}</div>}
          </div>
        );
      })}
      <ErrorLine error={error ?? invites.error} />
    </div>
  );
}
