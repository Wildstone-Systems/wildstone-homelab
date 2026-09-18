import { useState } from "react";
import { api, type ButlerRow, type ButlerSession } from "../../api";
import { ErrorLine, useLoad, when } from "../../lib";

// What this Butler has been asked, and what it answered.
//
// The filter is the session's own Butler stamp, never the surface. A
// household has several household Butlers, so filtering by surface
// would list every one of their conversations under whichever name the
// drawer happens to be showing — a screen stating that a Butler held a
// conversation it never held, which is the one lie this design exists
// to make impossible.
//
// Sessions before the stamp existed carry no Butler and are therefore
// not shown here. That is the honest outcome: they genuinely did not
// run as this Butler, and inventing an owner for them from the surface
// would be the same lie arrived at by backfill.
export function TabSessions({ row }: { row: ButlerRow }) {
  const b = row.butler;
  const sessions = useLoad(() => api.butlerSessions(20, undefined, b.name), [b.name]);
  const [open, setOpen] = useState<string | null>(null);

  const rows = sessions.data ?? [];

  return (
    <div className="tab-stack">
      <ErrorLine error={sessions.error} />
      {rows.length === 0 && !sessions.loading && (
        <div className="card roster-empty">
          <div className="muted">Nobody has asked this Butler anything yet.</div>
        </div>
      )}
      <div className="session-list">
        {rows.map((s) => (
          <Session
            key={s.id}
            session={s}
            open={open === s.id}
            onToggle={() => setOpen(open === s.id ? null : s.id)}
          />
        ))}
      </div>
    </div>
  );
}

// One conversation, collapsed to its opening line.
//
// The turns are fetched only when it is opened: a list of twenty
// sessions is twenty transcripts, and the reader wants one.
export function Session({
  session,
  open,
  onToggle,
}: {
  session: ButlerSession;
  open: boolean;
  onToggle: () => void;
}) {
  const full = useLoad(() => (open ? api.butlerSession(session.id) : Promise.resolve(null)), [open]);
  const turns = full.data?.turns ?? [];
  const opening = turns.find((t) => t.role === "user")?.text ?? session.finalText;

  return (
    <div className="card session-card">
      <button type="button" className="session-head" aria-expanded={open} onClick={onToggle}>
        <span className="tnum session-id">
          <code>{session.id}</code>
        </span>
        <span className="ellip">{opening}</span>
        <span className="muted tnum">
          {session.actingFor} · {when(session.createdAt)}
        </span>
      </button>
      {open && (
        <div className="session-turns">
          <ErrorLine error={full.error} />
          {turns.map((t) => (
            <div key={t.ordinal} className="session-turn">
              <span className="muted">{t.role === "user" ? session.actingFor : session.butler}</span>
              <span>{t.text}</span>
            </div>
          ))}
          {full.loading && <div className="muted">Reading the conversation…</div>}
        </div>
      )}
    </div>
  );
}
