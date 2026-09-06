import { useState } from "react";
import { api, AskResponse, ButlerSession, ButlerStatus } from "../api";
import { Badge, ErrorLine, useLoad, when } from "../lib";

// Butler (ADR-W017 A2): one screen for the operator's butler. Asking is a
// read — the only thing a session can write is a plan in state `proposed`,
// which a human approves on the Plans screen. This surface never approves.
export function Butler() {
  const status = useLoad<ButlerStatus>(() => api.butlerStatus(), []);
  const sessions = useLoad<ButlerSession[]>(() => api.butlerSessions(), []);
  const [message, setMessage] = useState("");
  const [answer, setAnswer] = useState<AskResponse | null>(null);
  const [askError, setAskError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [open, setOpen] = useState<ButlerSession | null>(null);

  const configured = status.data?.configured === true;

  const ask = () => {
    const text = message.trim();
    if (!text) return;
    setBusy(true);
    setAskError(null);
    api
      .askButler(text)
      .then((res) => {
        setAnswer(res);
        setMessage("");
        sessions.reload();
      })
      .catch((e: Error) => setAskError(e.message))
      .finally(() => setBusy(false));
  };

  return (
    <>
      <h2>Butler</h2>
      <div className="sub">
        Ask in plain language. The Butler reads state and can propose changes — every proposal
        lands on Plans as <code>proposed</code> and waits for your approval.
      </div>
      <ErrorLine error={status.error} />

      <StatusCard status={status.data} loading={status.loading} />

      <div className="card">
        <h3>Ask</h3>
        <textarea
          rows={4}
          placeholder={
            configured
              ? "e.g. why is jellyfin unhealthy? or: expose grafana at grafana.home.arpa"
              : "Configure a provider to enable asking."
          }
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) ask();
          }}
          disabled={!configured || busy}
        />
        <div className="row">
          <button
            className="action primary"
            disabled={!configured || busy || message.trim() === ""}
            onClick={ask}
          >
            {busy ? "Thinking…" : "Ask"}
          </button>
          <span className="muted">⌘/Ctrl + Enter sends. One ask is one recorded session.</span>
        </div>
        <ErrorLine error={askError} />
        {answer && <Answer answer={answer} />}
      </div>

      <h3 style={{ fontSize: 13, margin: "20px 0 8px" }}>Recent sessions</h3>
      <ErrorLine error={sessions.error} />
      <table>
        <thead>
          <tr>
            <th>time</th>
            <th>for</th>
            <th>provider</th>
            <th>mode</th>
            <th>answer</th>
          </tr>
        </thead>
        <tbody>
          {(sessions.data ?? []).map((s) => (
            <tr
              key={s.id}
              className="clickable"
              onClick={() => {
                if (open?.id === s.id) {
                  setOpen(null);
                  return;
                }
                api.butlerSession(s.id).then(setOpen).catch(() => setOpen(s));
              }}
            >
              <td>{when(s.createdAt)}</td>
              <td>{s.actingFor}</td>
              <td>{s.provider}</td>
              <td>
                <Badge value={s.mode} />
              </td>
              <td>{s.finalText.length > 160 ? s.finalText.slice(0, 160) + "…" : s.finalText}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {!sessions.loading && (sessions.data ?? []).length === 0 && (
        <div className="muted">No sessions yet.</div>
      )}
      {open && (
        <div className="card" style={{ marginTop: 12 }}>
          <h3>
            {open.id} — {when(open.createdAt)}
          </h3>
          <div className="muted" style={{ marginBottom: 8 }}>
            {open.principal} acting for {open.actingFor} · {open.provider} · {open.mode}
          </div>
          <pre>{open.finalText}</pre>
          {open.transcriptJson && (
            <>
              <div className="muted">Transcript (tool calls, already redacted)</div>
              <pre>{pretty(open.transcriptJson)}</pre>
            </>
          )}
        </div>
      )}
    </>
  );
}

export function StatusCard({ status, loading }: { status: ButlerStatus | null; loading: boolean }) {
  if (loading && !status) return <div className="card muted">Checking the Butler…</div>;
  if (!status) return null;

  if (!status.configured) {
    return (
      <div className="card">
        <h3>
          Not configured <Badge value="off" />
        </h3>
        <div className="muted" style={{ marginBottom: 8 }}>
          {status.reason}
        </div>
        <div className="muted">Point Keystone at a local Ollama:</div>
        <pre>
          {`keystone \\
  -butler-provider ollama \\
  -butler-endpoint http://localhost:11434 \\
  -butler-model llama3.1`}
        </pre>
        <div className="muted">
          Hosted backends read their key from the environment only — <code>ANTHROPIC_API_KEY</code>{" "}
          or <code>OPENAI_API_KEY</code>. There is deliberately no key flag and no key field here.
        </div>
      </div>
    );
  }

  return (
    <div className="card">
      <h3>
        {status.provider} <Badge value={status.mode ?? ""} />
      </h3>
      <div className="chips" style={{ marginBottom: 8 }}>
        <span className="chip">model: {status.model || "provider default"}</span>
        {status.endpoint && <span className="chip">endpoint: {status.endpoint}</span>}
      </div>
      {status.mode === "qa-only" && (
        <div className="muted" style={{ marginBottom: 8 }}>
          The startup probe found this model unreliable at tool calls, so sessions run read-only:
          it can answer questions, but <code>propose_change</code> is withheld and refused.
        </div>
      )}
      <div className="muted">Tools offered this session</div>
      <div className="chips">
        {(status.tools ?? []).map((t) => (
          <span className="chip" key={t}>
            {t}
          </span>
        ))}
      </div>
    </div>
  );
}

export function Answer({ answer }: { answer: AskResponse }) {
  return (
    <div style={{ marginTop: 10 }}>
      <pre>{answer.finalText || "(no answer text)"}</pre>
      {answer.capped && (
        <div className="muted">
          The session hit its tool-call cap; the answer above may be incomplete.
        </div>
      )}
      {(answer.planIds ?? []).length > 0 && (
        <div className="row">
          <span className="muted">Proposed, awaiting your approval on Plans:</span>
          {answer.planIds!.map((id) => (
            <span className="chip" key={id}>
              {id}
            </span>
          ))}
        </div>
      )}
      {(answer.proposalErrors ?? []).length > 0 && (
        <div className="row">
          <span className="muted">Refused by policy:</span>
          {answer.proposalErrors!.map((e, i) => (
            <span className="chip bad" key={i}>
              {e}
            </span>
          ))}
        </div>
      )}
      <div className="muted">session {answer.sessionId}</div>
    </div>
  );
}

// pretty renders the stored transcript readably, falling back to the raw
// string if it is not the JSON we expect.
function pretty(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
