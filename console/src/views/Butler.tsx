import { ReactNode, useEffect, useRef, useState } from "react";
import {
  ArrowsClockwise,
  ArrowUp,
  CaretDown,
  Check,
  Copy,
  Eye,
  NotePencil,
  Prohibit,
  SidebarSimple,
  Sparkle,
  Wrench,
  X,
} from "@phosphor-icons/react";
import {
  api,
  ApiError,
  isPendingStepUp,
  modelLabel,
  pendingStepUp,
  AskResponse,
  ButlerRow,
  ButlerSession,
  ButlerStatus,
  ButlerSurface,
  ButlerTurn,
  PlanRecord,
  RuntimeCredential,
  RuntimeHealth,
  RuntimeRecord,
} from "../api";
import { ago, ErrorLine, useLoad, when } from "../lib";
import { Confirm } from "../ui/Confirm";
import { Dot, Kind, StatusPill } from "../ui/status";
import { PendingPlan } from "./Plans";
import { ButlerPicker, openingFor, preferred, SURFACE_LABEL } from "./butler/picker";

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
export function Butler({
  onEyebrow,
  onNavigate,
}: {
  onEyebrow?: (label: string | null) => void;
  // Both places that link out of this screen go to Butlers: the
  // picker's footer and the read-only backend popover. A Butler's
  // model, reach and tools are edited there and nowhere else.
  onNavigate?: (view: "butlers") => void;
}) {
  // The selected Butler is a NAME, and everything else on the screen is
  // derived from it. It replaced a surface toggle, which was a control
  // for choosing a correspondent that could not name one: a household
  // with three household Butlers had a button meaning "one of these".
  const roster = useLoad(() => api.butlers());
  const rows = roster.data?.butlers ?? [];
  const [name, setName] = useState<string | null>(null);
  const current = rows.find((r) => r.butler.name === name && !r.butler.retiredAt) ?? preferred(rows);
  const butlerName = current?.butler.name;
  const surface: ButlerSurface = current?.butler.surface ?? "admin";

  const status = useLoad<ButlerStatus | null>(
    () => (butlerName ? api.oneButlerStatus(butlerName) : Promise.resolve(null)),
    [butlerName],
  );
  const sessions = useLoad<ButlerSession[]>(
    () => (butlerName ? api.butlerSessionsFor(butlerName) : Promise.resolve([])),
    [butlerName],
  );
  // The runtime behind the Butler (ADR-W039 §3.3), shown beside the
  // backend in the same popover. Its two actions are the Console
  // holder's, so who is looking decides whether they are offered.
  const runtime = useLoad<RuntimeRecord | null>(
    () => (butlerName ? api.butlerRuntime(butlerName) : Promise.resolve(null)),
    [butlerName],
  );
  const me = useLoad(() => api.whoami());
  // The list is a sidebar on a desk and a sheet over the thread on a
  // phone, so it starts open on one and closed on the other.
  const [railOpen, setRailOpen] = useState(() => !narrow());
  // A step-up interrupted a runtime action: the popover it lives in
  // opens again, so the confirmation the card reopens is on screen.
  const [statusOpen, setStatusOpen] = useState(
    () => pendingStepUp()?.path.includes("/runtime") ?? false,
  );
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

  // The top bar names the SURFACE, and now takes it from the selected
  // Butler rather than from a toggle. It is the one thing here that must
  // never lag the selection: a bar reading OPERATOR over a household
  // Butler is the mistake on this screen a person would act on.
  useEffect(() => {
    onEyebrow?.(current ? SURFACE_LABEL[current.butler.surface] : null);
    return () => onEyebrow?.(null);
  }, [current?.butler.surface, current, onEyebrow]);

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
    // A Butler whose model is not answering refuses here as well as in
    // the disabled button, because a keyboard shortcut does not read a
    // disabled attribute. The blocked strip is the explanation; this is
    // the refusal.
    if (!current || !current.reachable) return;
    setBusy(true);
    setAskError(null);
    setPending(message);
    // Keyed on openID rather than on the loaded thread, and the
    // difference is a race with real consequences. openID is set the
    // moment a session is known; thread is filled in by a fetch that
    // happens afterwards and can fail. Branching on thread meant that
    // between adopting a preserved session and its fetch landing, a
    // retry went back to askOneButler — opening a FRESH task and
    // running again the tool calls the preservation exists to protect.
    // openID is what "which conversation is this" actually means here,
    // and it is already correct by the time send can be called again.
    const call: Promise<AskResponse> =
      openID === null
        ? // The Butler by NAME. Asking a surface was the ambiguity this
          // screen removed, and asking through the surface endpoint
          // would also refuse a household Butler outright.
          api.askOneButler(current.butler.name, message)
        : api.continueButlerSession(openID, message);
    call
      // Re-opening the session is what loads the new turns AND the
      // plans they proposed; the effect above does both, so there is
      // one path that assembles a thread rather than two.
      .then((res) => {
        setOpenID(res.sessionId);
        setReloads((n) => n + 1);
      })
      .then(() => sessions.reload())
      .catch((e: Error) => {
        // A turn can fail after tools have already run, and the server
        // preserves that work and names the session holding it.
        // Adopting it is what makes the next message continue the task
        // instead of opening a new one and repeating calls that already
        // changed the house.
        if (e instanceof ApiError && e.sessionId) {
          setOpenID(e.sessionId);
          setReloads((n) => n + 1);
          void sessions.reload();
        }
        // A proposal the queue refused leaves no trace to find later.
        // The tool trace on the preserved turn shows that propose_change
        // RAN, which reads as success; the refusal happened after it,
        // during submission, and is only reported here. Saying it with
        // the error is what stops the operator assuming the change is
        // waiting on the Plans screen.
        //
        // It says to start a NEW conversation rather than just to ask
        // again, and the difference is not politeness. This thread has
        // just adopted the preserved session, so asking again resumes
        // that task — and the server recorded the accepted
        // propose_change as a verified step, which a resumed task
        // replays from its ledger instead of re-running. Retrying here
        // would look like it worked and queue nothing (#234).
        const refused =
          e instanceof ApiError && e.proposalErrors?.length ? e.proposalErrors : null;
        setAskError(
          refused
            ? `${e.message} — and a change it tried to propose was refused before reaching the approval queue (${refused.join("; ")}). Nothing is waiting there. Start a new conversation and ask again; continuing this one resumes the same task, which will not attempt that change a second time.`
            : e.message,
        );
      })
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
            sessions={sessions}
            openID={thread?.id ?? openID}
            onPick={(id) => {
              setAskError(null);
              setOpenID(id);
              setStatusOpen(false);
              if (narrow()) setRailOpen(false);
            }}
            rows={rows}
            current={current}
            onPickButler={(next) => {
              if (next === current?.butler.name) return;
              setName(next);
              // A thread belongs to the Butler that answered it, and a
              // draft belongs to the one you were writing to. Carrying
              // either across would put words meant for one Butler in
              // front of another's model.
              startNew();
            }}
            onManage={() => onNavigate?.("butlers")}
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
          onManage={() => onNavigate?.("butlers")}
          runtime={
            butlerName ? (
              <RuntimeCard
                name={butlerName}
                runtime={runtime.data}
                loading={runtime.loading}
                error={runtime.error}
                canManage={me.data?.role === "admin"}
                onChanged={() => void runtime.reload()}
              />
            ) : null
          }
        />

        {current && !current.reachable && (
          <BlockedStrip row={current} onManage={() => onNavigate?.("butlers")} />
        )}

        {/* useLoad keeps the last good data, so a status reload that
            fails after one has succeeded leaves `configured` true. The
            error belongs here, above every branch, or the screen goes on
            presenting a backend it can no longer reach. */}
        {status.error && (
          <div className="thread-error">
            <ErrorLine error={status.error} />
          </div>
        )}

        {!configured ? (
          // A surface with no backend behind it says what to run. The
          // sidebar stays: the OTHER surface may well be configured,
          // and a screen you cannot switch away from is a dead end.
          <div className="butler-setup">
            <StatusCard status={status.data} loading={status.loading} />
          </div>
        ) : thread === null && pending === null ? (
          <EmptyState row={current} busy={busy} onSend={send} error={askError} />
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
  rows,
  current,
  sessions,
  openID,
  onPick,
  onPickButler,
  onManage,
  onNew,
  onClose,
}: {
  rows: ButlerRow[];
  current?: ButlerRow;
  sessions: { data: ButlerSession[] | null; error: string | null; loading: boolean };
  openID: string | null;
  onPick: (id: string) => void;
  onPickButler: (name: string) => void;
  onManage: () => void;
  onNew: () => void;
  onClose: () => void;
}) {
  const groups = bucket(sessions.data ?? []);
  return (
    <aside className="convos" aria-label="Conversations">
      <div className="convos-head">
        <div className="row" style={{ margin: 0 }}>
          <ButlerPicker rows={rows} current={current} onPick={onPickButler} onManage={onManage} />
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
            No conversations with {current?.butler.name ?? "this Butler"} yet.
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
  onManage,
  runtime,
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
  onManage: () => void;
  // The runtime card, when the screen has a Butler to show one for.
  runtime?: ReactNode;
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
          {runtime}
          {/* Read-only, and ButlerSettings is gone from this screen.
              It writes the DEPLOYMENT's provider, model and endpoint,
              which is the pre-W031 shape: one model for everything.
              Editing it from a conversation with ONE Butler would have
              changed what answers for all of them, from a popover
              labelled with one Butler's name. Model, reach and tools
              are properties of a Butler and are set on its own screen. */}
          <div className="muted popover-foot">
            The model, reach and tools of a Butler are set on its own screen — one Butler, one
            model.{" "}
            <button className="btn btn-ghost btn-small" onClick={onManage}>
              Open {status?.butler ?? "it"} in Butlers →
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// A Butler that cannot answer says so, above the composer.
//
// It is a strip rather than a toast because it is not an event: it is
// the state the screen is in until somebody changes the model, and a
// message that faded would leave a composer that silently refuses. The
// send button goes dead alongside it, so the explanation and the
// refusal are never separated.
export function BlockedStrip({ row, onManage }: { row: ButlerRow; onManage: () => void }) {
  const b = row.butler;
  const why = !b.modelEnabled
    ? `${modelLabel(b)} is turned off`
    : `${modelLabel(b)} is not answering`;
  return (
    <div className="blocked-strip">
      <Dot kind="bad" />
      <span>
        {b.name} cannot answer: {why}.
      </span>
      <button className="btn btn-ghost btn-small" onClick={onManage}>
        Change its model →
      </button>
    </div>
  );
}

// The opening: the composer IS the screen, and the asks sit under it as
// chips. They belong here rather than above every composer — a suggested
// question is a way to start, not a control you need mid-conversation.
export function EmptyState({
  row,
  busy,
  onSend,
  error,
}: {
  row?: ButlerRow;
  busy: boolean;
  onSend: (text: string) => void;
  error?: string | null;
}) {
  const [text, setText] = useState("");
  // Every string on this screen is the selected Butler's, derived from
  // its own record. There is a fallback for the moment before the
  // roster has loaded, and it says nothing it cannot know.
  const opening = row
    ? openingFor(row)
    : { title: "Which Butler?", blurb: "Pick one to start a conversation.", placeholder: "Ask…" };
  const blocked = !!row && !row.reachable;
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
        <h2>{opening.title}</h2>
        <p className="blurb">{opening.blurb}</p>
        <div className="composer-box">
          <textarea
            rows={2}
            placeholder={opening.placeholder}
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
            disabled={busy || blocked || text.trim() === ""}
            onClick={submit}
            aria-label="Send"
          >
            {busy ? <Wrench size={15} /> : <ArrowUp size={15} />}
          </button>
        </div>
        {/* Suggested asks are the SURFACE's, because that is what they
            actually describe: what a household Butler can be asked about
            differs from what an operator one can, and neither depends on
            which Butler on that surface you picked. A per-Butler table
            would be inventing questions for a Butler somebody just
            made. */}
        <div className="asks">
          {(row ? SURFACES[row.butler.surface].asks : []).map((a) => (
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
          <TurnRow>
            <div className="msg msg-butler thinking">
              <Sparkle size={14} />
              Thinking…
            </div>
          </TurnRow>
        </>
      )}
      <div ref={end} />
    </div>
  );
}

// The butler's turn is a row — mark, then text — where the operator's
// own words are a bubble. Two shapes rather than two columns of cards is
// what makes the thread read as a conversation.
function TurnRow({ children }: { children: React.ReactNode }) {
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
  const made = proposals.filter((p) => (turn.planIds ?? []).includes(p.id));
  if (turn.role === "user") {
    // A user turn normally carries nothing but the question, and the
    // early return below is that case.
    //
    // A turn that FAILED after running tools is the exception. Its calls
    // are recorded on the question, because the Butler never answered
    // and inventing an assistant turn would put words in its mouth — so
    // this is the only place they can be shown. Hiding them is how
    // somebody does by hand the thing that already happened.
    const ran = parseCalls(turn.transcriptJson).length > 0 || (turn.planIds ?? []).length > 0;
    if (!ran) {
      return <div className="msg msg-user">{turn.text}</div>;
    }
    return (
      <TurnRow>
        <div className="msg msg-user">{turn.text}</div>
        <div className="msg-note">
          This turn did not finish. What it had already done is below, and the
          next message continues it rather than starting again.
        </div>
        <ToolTrace turn={turn} />
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
      </TurnRow>
    );
  }
  return (
    <TurnRow>
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
    </TurnRow>
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

// The Agent's runtime (ADR-W039 §3.3): what reasons for it, as whom,
// and how it is doing. Every Agent is on the native runtime today, so
// the type is shown and set nowhere on this screen; the two acts an
// Owner has here are on the credential. Rotating mints a successor and
// shows its secret ONCE — Wildstone keeps a hash and cannot show it
// again — and revoking stops every credential the runtime ever held
// from the next request (§3.7), leaving the Agent itself as it was.
// Both are consequential (ADR-W033 A4.1): a stale sign-in goes round
// the step-up and comes back to this card with its confirmation open.
export const RUNTIME_HEALTH: Record<RuntimeHealth, { kind: Kind; label: string }> = {
  connected: { kind: "ok", label: "connected" },
  degraded: { kind: "warn", label: "degraded" },
  offline: { kind: "bad", label: "offline" },
  revoked: { kind: "bad", label: "revoked" },
  unregistered: { kind: "mute", label: "unregistered" },
};

export function RuntimeCard({
  name,
  runtime,
  loading,
  error,
  canManage,
  onChanged,
}: {
  name: string;
  runtime: RuntimeRecord | null;
  loading: boolean;
  error: string | null;
  // Rotating and revoking need manage, which only the Console holder's
  // session carries. Anyone else sees the record and no buttons.
  canManage: boolean;
  onChanged: () => void;
}) {
  const credentialPath = `/api/v1/butlers/${encodeURIComponent(name)}/runtime/credential`;
  const revokePath = `/api/v1/butlers/${encodeURIComponent(name)}/runtime/revoke`;
  const [confirm, setConfirm] = useState<"rotate" | "revoke" | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [issued, setIssued] = useState<RuntimeCredential | null>(null);

  // Back from a step-up: the confirmation the person was at reopens,
  // and nothing is done until they press it again.
  useEffect(() => {
    if (isPendingStepUp("POST", credentialPath)) setConfirm("rotate");
    else if (isPendingStepUp("POST", revokePath)) setConfirm("revoke");
  }, [credentialPath, revokePath]);

  async function run(kind: "rotate" | "revoke") {
    setBusy(true);
    setActionError(null);
    try {
      if (kind === "rotate") {
        setIssued(await api.rotateRuntimeCredential(name));
      } else {
        await api.revokeRuntime(name);
        setIssued(null);
      }
      onChanged();
    } catch (e) {
      setActionError((e as Error).message);
    } finally {
      // Closed on failure too: a step-up refusal opens the shell's own
      // dialog, and two stacked confirmations would say the same thing.
      setConfirm(null);
      setBusy(false);
    }
  }

  if (loading && !runtime) return <div className="card runtime-card muted">Checking the runtime…</div>;
  if (!runtime) {
    return error ? (
      <div className="card runtime-card">
        <ErrorLine error={error} />
      </div>
    ) : null;
  }
  const health = RUNTIME_HEALTH[runtime.health] ?? { kind: "mute" as Kind, label: runtime.health };
  const credential = runtime.credentialRef || "none yet";
  return (
    <div className="card runtime-card">
      <h3>
        Runtime <StatusPill kind={health.kind}>{health.label}</StatusPill>
      </h3>
      <div className="chips" style={{ marginBottom: 8 }}>
        <span className="chip">type: {runtime.type}</span>
        {runtime.endpoint && <span className="chip">endpoint: {runtime.endpoint}</span>}
        <span className="chip" title="The Keystone principal the runtime authenticates as">
          credential: {credential}
        </span>
        <span className="chip">delivery: {runtime.negotiated?.delivery ?? "push"}</span>
        {runtime.negotiated?.processingClass && (
          <span className="chip">class: {runtime.negotiated.processingClass}</span>
        )}
        <span className="chip">profile: {runtime.executionProfileRef || "platform-only"}</span>
        <span className="chip">
          heartbeat:{" "}
          {runtime.lastHeartbeat
            ? ago(runtime.lastHeartbeat)
            : runtime.type === "native"
              ? "in-process"
              : "never"}
        </span>
      </div>
      {/* Revoked first, whatever the type: it is the one state where
          the runtime's usual description would be a lie. */}
      <div className="muted">
        {runtime.health === "revoked"
          ? "Nothing can act for this Butler until a new credential is rotated" +
            (runtime.type === "native" ? "." : " and the runtime presents it.")
          : runtime.type === "native"
            ? "Reasons inside Keystone, as this credential. Its model, reach and tools are set on the Butlers screen."
            : "An external runtime. It is connected once it has presented this credential."}
      </div>
      {issued && <IssuedCredential credential={issued} onDismiss={() => setIssued(null)} />}
      {actionError && <ErrorLine error={actionError} />}
      {canManage && (
        <div className="row" style={{ marginBottom: 0 }}>
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            disabled={busy}
            onClick={() => setConfirm("rotate")}
          >
            <ArrowsClockwise size={14} /> Rotate credential
          </button>
          <button
            type="button"
            className="btn btn-danger btn-sm"
            disabled={busy || runtime.health === "revoked"}
            onClick={() => setConfirm("revoke")}
          >
            <Prohibit size={14} /> Revoke runtime
          </button>
        </div>
      )}
      {confirm === "rotate" && (
        <Confirm
          kind="warn"
          icon={<ArrowsClockwise size={18} weight="bold" />}
          title="Rotate the runtime credential?"
          body={`A new credential is minted for ${name} and shown once. ${credential} stops working immediately: whatever runs as it must be given the new secret.`}
          confirmLabel="Rotate"
          cancelLabel={`Keep ${credential}`}
          busy={busy}
          onConfirm={() => void run("rotate")}
          onCancel={() => setConfirm(null)}
        />
      )}
      {confirm === "revoke" && (
        <Confirm
          kind="bad"
          icon={<Prohibit size={18} weight="bold" />}
          title="Revoke the runtime?"
          body={`Every credential ${name}'s runtime has ever held stops working from the next request, and the runtime is marked revoked. ${name} keeps its people, tools, connections and routines; nothing can act for it until you rotate a new credential.`}
          confirmLabel="Revoke"
          cancelLabel="Keep it running"
          busy={busy}
          onConfirm={() => void run("revoke")}
          onCancel={() => setConfirm(null)}
        />
      )}
    </div>
  );
}

// The one moment a runtime secret is on screen. Wildstone stores a
// hash of it and nothing else, so there is no "show again": the person
// copies it now or rotates again. A secret that was placed on a node
// instead never appears here at all — the card says where it went.
export function IssuedCredential({
  credential,
  onDismiss,
}: {
  credential: RuntimeCredential;
  onDismiss: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    // Guarded: the clipboard is absent off HTTPS and in the render
    // check, and a copy button that throws is worse than one that
    // leaves the secret selectable.
    if (typeof navigator === "undefined" || !navigator.clipboard) return;
    navigator.clipboard
      .writeText(credential.secret ?? "")
      .then(() => setCopied(true))
      .catch(() => {});
  };
  if (!credential.secret) {
    return (
      <div className="callout callout-warn" role="status">
        <div className="eyebrow">New credential: {credential.credentialRef}</div>
        <div>
          Placed on <strong>{credential.node}</strong> as <code>{credential.secretName}</code>. It was
          never returned here; containers that read it have been re-created.
        </div>
        <div className="row" style={{ margin: "8px 0 0" }}>
          <button type="button" className="btn btn-ghost btn-sm" onClick={onDismiss}>
            Done
          </button>
        </div>
      </div>
    );
  }
  return (
    <div className="callout callout-warn" role="status">
      <div className="eyebrow">New credential: {credential.credentialRef}</div>
      <pre className="runtime-secret-value">{credential.secret}</pre>
      <div className="row" style={{ margin: "8px 0 0" }}>
        <button type="button" className="btn btn-secondary btn-sm" onClick={copy}>
          {copied ? <Check size={14} /> : <Copy size={14} />} {copied ? "Copied" : "Copy"}
        </button>
        <button type="button" className="btn btn-ghost btn-sm" onClick={onDismiss}>
          Done
        </button>
      </div>
      <div className="danger-line">
        This is the only time it will be shown. Paste it into the runtime's configuration now; if it
        is lost, rotate again to get another.
      </div>
    </div>
  );
}
