import { useEffect, useRef, useState } from "react";
import { ArrowUp, Eye, NotePencil, Sparkle, Wrench } from "@phosphor-icons/react";
import {
  api,
  AskResponse,
  ButlerSession,
  ButlerStatus,
  ButlerSurface,
  ButlerTurn,
  PlanRecord,
} from "../api";
import { ago, ErrorLine, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";
import { PendingPlan } from "./Plans";
import { ButlerSettings } from "./ButlerSettings";

// The two surfaces, as the operator picks between them (ADR-W026 A1).
// The copy differs because the ceilings differ, and a screen that
// described them identically would be teaching the wrong thing.
export const SURFACES: Record<
  ButlerSurface,
  { label: string; blurb: string; placeholder: string; asks: string[] }
> = {
  admin: {
    label: "Operator",
    blurb:
      "Reads system state and can propose changes — every proposal lands on Plans as `proposed` and waits for your approval.",
    placeholder: "e.g. why is jellyfin unhealthy? or: expose grafana at grafana.home.arpa",
    asks: ["What is unhealthy right now?", "What nodes do I have?", "Did the last backup pass?"],
  },
  family: {
    label: "Household",
    blurb:
      "Reads the household timeline and can perform household actions. It has no access to the home lab itself — no containers, storage, accounts or access rules — and cannot propose changes.",
    placeholder: "e.g. did anyone log breakfast this morning? or: remind everyone about the bins",
    asks: ["Did anyone log breakfast?", "What happened today?", "Remind everyone about the bins"],
  },
};

// Butler (ADR-W017 A2, ADR-W026 A1, ADR-W027): one screen, two surfaces,
// and a conversation rather than a form.
//
// Asking is a read on both surfaces — the only thing an operator session
// can write is a plan in state `proposed`, and a household session cannot
// write a plan at all. Neither surface approves: a proposal appears in
// the thread as the SAME card the Plans screen shows, with the same
// handlers, which is what keeps "the Butler proposes but never acts"
// visible. The approve button is yours, in both places.
export function Butler() {
  const [surface, setSurface] = useState<ButlerSurface>("admin");
  const status = useLoad<ButlerStatus>(() => api.butlerStatus(surface), [surface]);
  const sessions = useLoad<ButlerSession[]>(() => api.butlerSessions(20, surface), [surface]);
  const [showSettings, setShowSettings] = useState(false);

  // The open conversation. null is a new one, which is the state the
  // screen opens in — reading someone's last answer is not the same as
  // wanting to continue it.
  const [openID, setOpenID] = useState<string | null>(null);
  const [thread, setThread] = useState<ButlerSession | null>(null);
  const [pending, setPending] = useState<string | null>(null);
  // A follow-up lands in the SAME session, so openID does not change
  // and the effect below would not re-run. Bumping this is what makes
  // "reload the thread" mean it.
  const [reloads, setReloads] = useState(0);
  const [busy, setBusy] = useState(false);
  const [askError, setAskError] = useState<string | null>(null);
  const [proposals, setProposals] = useState<PlanRecord[]>([]);

  const configured = status.data?.configured === true;

  useEffect(() => {
    if (openID === null) {
      setThread(null);
      setProposals([]);
      return;
    }
    let stale = false;
    api
      .butlerSession(openID)
      .then(async (s) => {
        if (stale) return;
        setThread(s);
        // Fetch every plan this conversation proposed, so re-opening it
        // shows the proposals as the cards they are — and lets one that
        // is still waiting be approved from here. A plan that has since
        // been deleted just falls back to being named.
        const ids = (s.turns ?? []).flatMap((t) => t.planIds ?? []);
        const fetched = await Promise.all(ids.map((id) => api.plan(id).catch(() => null)));
        if (!stale) setProposals(fetched.filter((p): p is PlanRecord => p !== null));
      })
      .catch((e: Error) => !stale && setAskError(e.message));
    return () => {
      stale = true;
    };
  }, [openID, reloads]);

  const send = (text: string) => {
    const message = text.trim();
    if (!message) return;
    setBusy(true);
    setAskError(null);
    setPending(message);
    const call: Promise<AskResponse> =
      thread === null ? api.askButler(message, surface) : api.continueButlerSession(thread.id, message);
    call
      // Re-opening the session is what loads the new turns AND the
      // plans they proposed; the effect above does both, so there is
      // one path that assembles a thread rather than two.
      .then((res) => {
        setOpenID(res.sessionId);
        setReloads((n) => n + 1);
      })
      .then(() => sessions.reload())
      .catch((e: Error) => setAskError(e.message))
      .finally(() => {
        setBusy(false);
        setPending(null);
      });
  };

  // After approving or rejecting inside the thread, re-read that one
  // plan so the card in the conversation shows the same resolution the
  // Plans screen would.
  const refreshProposal = (id: string) => {
    api
      .plan(id)
      .then((fresh) => setProposals((ps) => ps.map((p) => (p.id === fresh.id ? fresh : p))))
      .catch(() => {});
  };

  const startNew = () => {
    setOpenID(null);
    setThread(null);
    setProposals([]);
    setAskError(null);
  };

  return (
    <>
      <div className="row" style={{ marginTop: 0 }}>
        {(Object.keys(SURFACES) as ButlerSurface[]).map((key) => (
          <button
            key={key}
            className={"btn " + (surface === key ? "btn-primary" : "btn-secondary")}
            onClick={() => {
              if (surface === key) return;
              setSurface(key);
              startNew();
            }}
          >
            {SURFACES[key].label}
          </button>
        ))}
        <span className="fade" />
        <button className="btn btn-secondary" onClick={() => setShowSettings((v) => !v)}>
          {showSettings ? "Hide settings" : "Settings"}
        </button>
      </div>

      {/* Behind a toggle on the same screen: asking a question should not
          mean scrolling past a configuration form, and changing the model
          should not mean finding another tab. The backend it configures is
          shared — both surfaces run on it — so this sits above the surface
          switch rather than inside either one. */}
      {showSettings && (
        <div className="card">
          <ButlerSettings
            onSaved={() => {
              status.reload();
              sessions.reload();
            }}
          />
        </div>
      )}

      <ErrorLine error={status.error} />

      {!configured ? (
        <StatusCard status={status.data} loading={status.loading} />
      ) : (
        <div className="butler">
          <div className="thread">
            <Thread
              surface={surface}
              session={thread}
              pending={pending}
              proposals={proposals}
              onProposalResolved={refreshProposal}
            />
            <ErrorLine error={askError} />
            <Composer
              surface={surface}
              busy={busy}
              onSend={send}
              showAsks={thread === null}
            />
          </div>

          <aside className="convos">
            <div className="sect">
              <span className="eyebrow">Conversations</span>
              <span className="fade" />
              <button className="btn btn-ghost" onClick={startNew} title="Start a new conversation">
                <NotePencil size={15} />
              </button>
            </div>
            <ErrorLine error={sessions.error} />
            {(sessions.data ?? []).map((s) => (
              <button
                key={s.id}
                className={"convo" + (thread?.id === s.id ? " on" : "")}
                onClick={() => {
                  setAskError(null);
                  setOpenID(s.id);
                }}
              >
                <span className="convo-title">{s.finalText || s.id}</span>
                <span className="muted" title={when(s.createdAt)}>
                  {ago(s.createdAt)} · {s.provider}
                </span>
              </button>
            ))}
            {!sessions.loading && (sessions.data ?? []).length === 0 && (
              <div className="muted">
                No {SURFACES[surface].label.toLowerCase()} conversations yet.
              </div>
            )}
            <StatusCard status={status.data} loading={status.loading} />
          </aside>
        </div>
      )}
    </>
  );
}

export function Thread({
  surface,
  session,
  pending,
  proposals,
  onProposalResolved,
}: {
  surface: ButlerSurface;
  session: ButlerSession | null;
  pending: string | null;
  proposals: PlanRecord[];
  onProposalResolved: (id: string) => void;
}) {
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => {
    end.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [session, pending]);

  const turns = session?.turns ?? [];

  return (
    <div className="messages">
      {turns.length === 0 && pending === null && (
        <div className="muted" style={{ maxWidth: "60ch" }}>
          {SURFACES[surface].blurb}
        </div>
      )}
      {turns.map((t) => (
        <Turn key={t.ordinal} turn={t} proposals={proposals} onResolved={onProposalResolved} />
      ))}
      {pending !== null && (
        <>
          <div className="msg msg-user">{pending}</div>
          <div className="msg msg-butler thinking">
            <Sparkle size={14} />
            Thinking…
          </div>
        </>
      )}
      <div ref={end} />
    </div>
  );
}

export function Turn({
  turn,
  proposals,
  onResolved,
}: {
  turn: ButlerTurn;
  proposals: PlanRecord[];
  onResolved: (id: string) => void;
}) {
  if (turn.role === "user") {
    return <div className="msg msg-user">{turn.text}</div>;
  }
  const made = proposals.filter((p) => (turn.planIds ?? []).includes(p.id));
  return (
    <>
      <div className="msg msg-butler">{turn.text || "(no answer text)"}</div>
      <ToolTrace turn={turn} />
      {turn.capped && (
        <div className="msg-note">
          The turn hit its tool-call cap; the answer above may be incomplete.
        </div>
      )}
      {/* The same card Plans shows, with the same handlers. One
          component, two placements — which is what keeps "the Butler
          proposes but never acts" visible: the approve button is yours,
          in both places. */}
      {made.map((p) => (
        <div className="proposal" key={p.id}>
          <PendingPlan plan={p} onResolved={() => onResolved(p.id)} />
        </div>
      ))}
      {made.length === 0 && (turn.planIds ?? []).length > 0 && (
        <div className="msg-note">
          Proposed {turn.planIds!.join(", ")} — open Plans to approve.
        </div>
      )}
    </>
  );
}

// The tool trace is a quiet line, not a panel: what the Butler read to
// answer is worth being able to check and not worth reading every time.
export function ToolTrace({ turn }: { turn: ButlerTurn }) {
  const [open, setOpen] = useState(false);
  const calls = parseCalls(turn.transcriptJson);
  if (calls.length === 0) return null;
  const names = [...new Set(calls.map((c) => c.name))];
  return (
    <div className="trace">
      <button className="btn btn-ghost" onClick={() => setOpen((v) => !v)}>
        <Eye size={13} />
        {calls.length} tool {calls.length === 1 ? "call" : "calls"} · {names.join(", ")}
      </button>
      {open && (
        <pre>
          {calls
            .map((c) => `${c.name}(${JSON.stringify(c.input ?? {})})\n  ${c.error ?? c.result ?? ""}`)
            .join("\n\n")}
        </pre>
      )}
    </div>
  );
}

interface StoredCall {
  name: string;
  input?: Record<string, unknown>;
  result?: string;
  error?: string;
  rejected?: boolean;
}

function parseCalls(raw?: string): StoredCall[] {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw) as { calls?: StoredCall[] };
    return parsed.calls ?? [];
  } catch {
    return [];
  }
}

function Composer({
  surface,
  busy,
  onSend,
  showAsks,
}: {
  surface: ButlerSurface;
  busy: boolean;
  onSend: (text: string) => void;
  showAsks: boolean;
}) {
  const [text, setText] = useState("");
  const submit = () => {
    onSend(text);
    setText("");
  };
  return (
    <div className="composer">
      {showAsks && (
        <div className="asks">
          {SURFACES[surface].asks.map((a) => (
            <button key={a} className="btn btn-secondary" disabled={busy} onClick={() => onSend(a)}>
              {a}
            </button>
          ))}
        </div>
      )}
      <div className="composer-box">
        <textarea
          rows={2}
          placeholder={SURFACES[surface].placeholder}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit();
          }}
          disabled={busy}
          aria-label="Ask the Butler"
        />
        <button
          className="btn btn-primary"
          disabled={busy || text.trim() === ""}
          onClick={submit}
          aria-label="Send"
        >
          {busy ? <Wrench size={15} /> : <ArrowUp size={15} />}
        </button>
      </div>
      <div className="muted">⌘/Ctrl + Enter sends. A follow-up stays in the same conversation.</div>
    </div>
  );
}

export function StatusCard({ status, loading }: { status: ButlerStatus | null; loading: boolean }) {
  if (loading && !status) return <div className="card muted">Checking the Butler…</div>;
  if (!status) return null;

  if (!status.configured) {
    return (
      <div className="card">
        <h3>
          Not configured <StatusPill kind="mute">off</StatusPill>
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
    <div className="card" style={{ marginTop: "var(--space-6)" }}>
      <h3>
        {status.provider} <StatusPill kind={status.mode === "qa-only" ? "warn" : "ok"}>{status.mode ?? ""}</StatusPill>
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
