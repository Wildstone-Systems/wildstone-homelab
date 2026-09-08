import { useEffect, useRef, useState } from "react";
import {
  ArrowUp,
  CaretDown,
  Eye,
  NotePencil,
  SidebarSimple,
  Sparkle,
  Wrench,
  X,
} from "@phosphor-icons/react";
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
  { label: string; title: string; blurb: string; placeholder: string; asks: string[] }
> = {
  admin: {
    label: "Operator",
    title: "What do you need to know?",
    blurb:
      "Reads system state and can propose changes — every proposal lands on Plans as `proposed` and waits for your approval.",
    placeholder: "e.g. why is jellyfin unhealthy? or: expose grafana at grafana.home.arpa",
    asks: ["What is unhealthy right now?", "What nodes do I have?", "Did the last backup pass?"],
  },
  family: {
    label: "Household",
    title: "What's happening at home?",
    blurb:
      "Reads the household timeline and can perform household actions. It has no access to the home lab itself — no containers, storage, accounts or access rules — and cannot propose changes.",
    placeholder: "e.g. did anyone log breakfast this morning? or: remind everyone about the bins",
    asks: ["Did anyone log breakfast?", "What happened today?", "Remind everyone about the bins"],
  },
};

const NARROW = "(max-width: 720px)";

// True on a phone. Guarded because the render check has no window at
// all, and a screen that throws in Node is a screen nothing checks.
function narrow(): boolean {
  try {
    return window.matchMedia(NARROW).matches;
  } catch {
    return false;
  }
}

// Butler (ADR-W017 A2, ADR-W026 A1, ADR-W027): one screen, two surfaces,
// and a conversation rather than a form.
//
// The screen owns the pane: the header is fixed, the thread scrolls and
// the composer is pinned under it, which is what a chat window is. The
// conversation list is a collapsible sidebar on the left and a bottom
// sheet on a phone; the backend it runs on is a chip in the header, not
// a form the thread has to be scrolled past.
//
// Asking is a read on both surfaces — the only thing an operator session
// can write is a plan in state `proposed`, and a household session cannot
// write a plan at all. Neither surface approves: a proposal appears in
// the thread as the SAME card the Plans screen shows, with the same
// handlers, which is what keeps "the Butler proposes but never acts"
// visible. The approve button is yours, in both places.
export function Butler({ onEyebrow }: { onEyebrow?: (label: string | null) => void }) {
  const [surface, setSurface] = useState<ButlerSurface>("admin");
  const status = useLoad<ButlerStatus>(() => api.butlerStatus(surface), [surface]);
  const sessions = useLoad<ButlerSession[]>(() => api.butlerSessions(20, surface), [surface]);
  // The list is a sidebar on a desk and a sheet over the thread on a
  // phone, so it starts open on one and closed on the other.
  const [railOpen, setRailOpen] = useState(() => !narrow());
  const [statusOpen, setStatusOpen] = useState(false);
  const chip = useRef<HTMLButtonElement>(null);

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

  // The top bar names the SURFACE, not the view: a bar reading
  // "OPERATOR" over the household butler is the one mistake this screen
  // can make that a person would act on.
  useEffect(() => {
    onEyebrow?.(SURFACES[surface].label);
    return () => onEyebrow?.(null);
  }, [surface, onEyebrow]);

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
    setStatusOpen(false);
    if (narrow()) setRailOpen(false);
  };

  // Closing the popover puts focus back where it came from: a dialog
  // that drops focus on the body sends a keyboard back to the top of
  // the screen.
  const closeStatus = () => {
    setStatusOpen(false);
    chip.current?.focus();
  };

  return (
    <div className="butler">
      {railOpen && (
        <>
          {/* The sheet needs something to close it on a phone. It is a
              button rather than a div so a keyboard can reach it. */}
          <button
            type="button"
            className="convos-scrim"
            aria-label="Close conversations"
            onClick={() => setRailOpen(false)}
          />
          <ConversationRail
            surface={surface}
            sessions={sessions}
            openID={thread?.id ?? openID}
            onPick={(id) => {
              setAskError(null);
              setOpenID(id);
              setStatusOpen(false);
              if (narrow()) setRailOpen(false);
            }}
            onSurface={(s) => {
              if (s === surface) return;
              setSurface(s);
              startNew();
            }}
            onNew={startNew}
            onClose={() => setRailOpen(false)}
          />
        </>
      )}

      <section className="thread">
        <ThreadBar
          title={thread?.finalText || "New conversation"}
          status={status.data}
          configured={configured}
          open={statusOpen}
          chipRef={chip}
          onToggle={() => setStatusOpen((v) => !v)}
          onClose={closeStatus}
          onOpenRail={railOpen ? undefined : () => setRailOpen(true)}
          onNew={startNew}
          onSaved={() => {
            status.reload();
            sessions.reload();
          }}
        />

        {!configured ? (
          // A surface with no backend behind it says what to run. The
          // sidebar stays: the OTHER surface may well be configured,
          // and a screen you cannot switch away from is a dead end.
          <div className="butler-setup">
            <ErrorLine error={status.error} />
            <StatusCard status={status.data} loading={status.loading} />
          </div>
        ) : thread === null && pending === null ? (
          <EmptyState surface={surface} busy={busy} onSend={send} error={askError} />
        ) : (
          <>
            <Thread
              surface={surface}
              session={thread}
              pending={pending}
              proposals={proposals}
              onProposalResolved={refreshProposal}
            />
            {askError && (
              <div className="thread-error">
                <ErrorLine error={askError} />
              </div>
            )}
            <Composer surface={surface} busy={busy} onSend={send} />
          </>
        )}
      </section>
    </div>
  );
}

// The conversation list: which surface, a way to start a new one, and
// the history grouped by day. Its own scroller, so the thread beside it
// never moves when the list grows.
export function ConversationRail({
  surface,
  sessions,
  openID,
  onPick,
  onSurface,
  onNew,
  onClose,
}: {
  surface: ButlerSurface;
  sessions: { data: ButlerSession[] | null; error: string | null; loading: boolean };
  openID: string | null;
  onPick: (id: string) => void;
  onSurface: (s: ButlerSurface) => void;
  onNew: () => void;
  onClose: () => void;
}) {
  const groups = bucket(sessions.data ?? []);
  return (
    <aside className="convos" aria-label="Conversations">
      <div className="convos-head">
        <div className="row" style={{ margin: 0 }}>
          <div className="seg" role="tablist" aria-label="Butler surface">
            {(Object.keys(SURFACES) as ButlerSurface[]).map((key) => (
              <button
                key={key}
                role="tab"
                aria-selected={surface === key}
                className={surface === key ? "on" : ""}
                onClick={() => onSurface(key)}
              >
                {SURFACES[key].label}
              </button>
            ))}
          </div>
          <button
            className="btn btn-ghost"
            onClick={onClose}
            title="Hide conversations"
            aria-label="Hide conversations"
          >
            <SidebarSimple size={15} />
          </button>
        </div>
        <button className="btn btn-secondary btn-block" onClick={onNew} style={{ marginTop: 0 }}>
          <NotePencil size={15} />
          New conversation
        </button>
      </div>

      <div className="convos-list">
        <ErrorLine error={sessions.error} />
        {groups.map((g) => (
          <div className="convos-group" key={g.label}>
            <span className="eyebrow">{g.label}</span>
            {g.items.map((s) => (
              <button
                key={s.id}
                className={"convo" + (openID === s.id ? " on" : "")}
                onClick={() => onPick(s.id)}
              >
                <span className="convo-title">{s.finalText || s.id}</span>
                <span className="muted" title={when(s.createdAt)}>
                  {ago(s.createdAt)} · {s.provider}
                </span>
              </button>
            ))}
          </div>
        ))}
        {!sessions.loading && (sessions.data ?? []).length === 0 && (
          <div className="muted" style={{ padding: "var(--space-3)" }}>
            No {SURFACES[surface].label.toLowerCase()} conversations yet.
          </div>
        )}
      </div>
    </aside>
  );
}

export interface Bucket {
  label: string;
  items: ButlerSession[];
}

// Conversations grouped the way a person remembers them: today, then
// yesterday, then the week, then everything else. Grouping is a
// presentation concern, so it lives here rather than on the wire.
export function bucket(sessions: ButlerSession[], now: Date = new Date()): Bucket[] {
  const day = 86_400_000;
  const midnight = new Date(now).setHours(0, 0, 0, 0);
  const label = (iso?: string) => {
    const t = iso ? new Date(iso).getTime() : NaN;
    if (isNaN(t)) return "Earlier";
    if (t >= midnight) return "Today";
    if (t >= midnight - day) return "Yesterday";
    if (t >= midnight - 7 * day) return "This week";
    return "Earlier";
  };
  const order = ["Today", "Yesterday", "This week", "Earlier"];
  const map = new Map<string, ButlerSession[]>();
  for (const s of sessions) {
    const k = label(s.createdAt);
    map.set(k, [...(map.get(k) ?? []), s]);
  }
  return order.filter((k) => map.has(k)).map((k) => ({ label: k, items: map.get(k)! }));
}

// The header: what this conversation is, and what is answering it. The
// backend lives behind a chip because it is worth being able to check
// and not worth reading every time — and because a settings form above
// the thread pushed the conversation down the screen every time it was
// opened.
export function ThreadBar({
  title,
  status,
  configured,
  open,
  chipRef,
  onToggle,
  onClose,
  onOpenRail,
  onNew,
  onSaved,
}: {
  title: string;
  status: ButlerStatus | null;
  configured: boolean;
  open: boolean;
  chipRef?: React.RefObject<HTMLButtonElement>;
  onToggle: () => void;
  onClose: () => void;
  onOpenRail?: () => void;
  onNew: () => void;
  onSaved: () => void;
}) {
  const backend = configured
    ? [status?.provider, status?.model].filter(Boolean).join(" · ") || "configured"
    : "Not configured";
  return (
    <div className="thread-bar">
      {onOpenRail && (
        <button
          className="btn btn-secondary icon"
          onClick={onOpenRail}
          title="Show conversations"
          aria-label="Show conversations"
        >
          <SidebarSimple size={16} />
        </button>
      )}
      <span className="thread-title">{title}</span>
      <button
        className="btn btn-ghost icon"
        onClick={onNew}
        title="Start a new conversation"
        aria-label="Start a new conversation"
      >
        <NotePencil size={16} />
      </button>
      <button
        ref={chipRef}
        className="chip chip-button"
        onClick={onToggle}
        aria-expanded={open}
        title="Which backend is answering"
      >
        <Sparkle size={13} color="var(--mark)" />
        {backend}
        <CaretDown size={10} className="caret" />
      </button>
      {open && (
        <div className="popover" role="dialog" aria-label="Butler backend">
          <button
            className="btn btn-ghost popover-close"
            onClick={onClose}
            title="Close"
            aria-label="Close"
          >
            <X size={15} />
          </button>
          {/* The unconfigured card is already the pane behind this
              popover; showing it twice would say the same thing twice. */}
          {configured && <StatusCard status={status} loading={false} />}
          <div className="popover-settings">
            <ButlerSettings onSaved={onSaved} />
          </div>
        </div>
      )}
    </div>
  );
}

// The opening: the composer IS the screen, and the asks sit under it as
// chips. They belong here rather than above every composer — a suggested
// question is a way to start, not a control you need mid-conversation.
export function EmptyState({
  surface,
  busy,
  onSend,
  error,
}: {
  surface: ButlerSurface;
  busy: boolean;
  onSend: (text: string) => void;
  error?: string | null;
}) {
  const [text, setText] = useState("");
  const submit = () => {
    onSend(text);
    setText("");
  };
  return (
    <div className="opening">
      <div className="opening-inner">
        <span className="turn-mark" aria-hidden>
          <Sparkle size={21} weight="fill" />
        </span>
        <h2>{SURFACES[surface].title}</h2>
        <p className="blurb">{SURFACES[surface].blurb}</p>
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
        <div className="asks">
          {SURFACES[surface].asks.map((a) => (
            <button key={a} className="btn btn-secondary" disabled={busy} onClick={() => onSend(a)}>
              {a}
            </button>
          ))}
        </div>
        <ErrorLine error={error ?? null} />
        <div className="muted">⌘/Ctrl + Enter sends</div>
      </div>
    </div>
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
          <ButlerRow>
            <div className="msg msg-butler thinking">
              <Sparkle size={14} />
              Thinking…
            </div>
          </ButlerRow>
        </>
      )}
      <div ref={end} />
    </div>
  );
}

// The butler's turn is a row — mark, then text — where the operator's
// own words are a bubble. Two shapes rather than two columns of cards is
// what makes the thread read as a conversation.
function ButlerRow({ children }: { children: React.ReactNode }) {
  return (
    <div className="turn-butler">
      <span className="turn-mark" aria-hidden>
        <Sparkle size={14} weight="fill" />
      </span>
      <div className="turn-body">{children}</div>
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
    <ButlerRow>
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
    </ButlerRow>
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

export function Composer({
  surface,
  busy,
  onSend,
}: {
  surface: ButlerSurface;
  busy: boolean;
  onSend: (text: string) => void;
}) {
  const [text, setText] = useState("");
  const submit = () => {
    onSend(text);
    setText("");
  };
  return (
    <div className="composer">
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
    <div className="card">
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
