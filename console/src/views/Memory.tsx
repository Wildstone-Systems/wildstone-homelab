import { useState } from "react";
import { api, type MemoryRecord } from "../api";
import { ErrorLine, useAction, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";

// What the Butler remembers (ADR-W030 A3, P8).
//
// A household that cannot see what its assistant believes cannot correct
// it, and an assistant nobody corrects gets steadily more confident and
// more wrong. So this screen is deliberately plain: every record, where
// it came from, and two buttons.
//
// The distinction it works hardest to keep visible is stated versus
// inferred. An inference shown with the confidence of a fact is how a
// household ends up arguing with its own house, so a guess says it is a
// guess.

const CONFIDENCE_NOTE: Record<string, string> = {
  stated: "you said this",
  inferred: "worked out from what it saw — worth checking",
  uncertain: "a guess it is not sure about",
};

export function Memory() {
  const memory = useLoad(() => api.butlerMemory());
  const digest = useLoad(() => api.memoryDigest());
  const records = memory.data?.records ?? [];
  const entries = digest.data?.entries ?? [];
  const immediate = entries.filter((e) => e.immediate);

  return (
    <>
      <div className="sub">
        Everything the Butler has learned that you can see: your own, and what the household
        shares. Correcting a record supersedes it rather than editing it, so what it used to
        think stays answerable — and forgetting one also stops the conversation it came from
        teaching it again.
      </div>
      <ErrorLine error={memory.error} />

      {immediate.length > 0 && (
        <section className="work-section">
          <h3>Removed just now</h3>
          <div className="msg-note">
            Safety-relevant records were removed. The household hears about these immediately
            rather than in the next digest.
          </div>
          <ul className="work-evidence">
            {immediate.map((e) => (
              <li key={e.seq}>
                {e.summary}
                <span className="timeline-src">
                  {e.actor} · {when(e.at)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <section className="work-section">
        <h3>What it remembers</h3>
        {records.length === 0 && !memory.loading && (
          <div className="muted">Nothing yet. Records appear here as conversations produce them.</div>
        )}
        <ul className="work-evidence">
          {records.map((r) => (
            <Record key={r.seq} record={r} onChanged={memory.reload} />
          ))}
        </ul>
      </section>

      <section className="work-section">
        <h3>Shared changes</h3>
        {entries.length === 0 && !digest.loading && (
          <div className="muted">No shared memory has changed since the last digest.</div>
        )}
        <ul className="work-evidence">
          {entries.map((e) => (
            <li key={e.seq}>
              <StatusPill kind={e.change === "forgotten" ? "warn" : "mute"}>{e.change}</StatusPill>
              {e.summary}
              <span className="timeline-src">
                {e.actor} · {when(e.at)}
              </span>
            </li>
          ))}
        </ul>
      </section>
    </>
  );
}

function Record({ record, onChanged }: { record: MemoryRecord; onChanged: () => void }) {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(record.content);
  const [note, setNote] = useState<string | null>(null);
  const action = useAction();
  return (
    <li>
      <StatusPill kind={record.audience === "shared" ? "ok" : "mute"}>{record.audience}</StatusPill>
      <span>{record.content}</span>
      <span className="timeline-src">{record.kind}</span>
      <span className="timeline-src">
        {CONFIDENCE_NOTE[record.confidence] ?? record.confidence}
      </span>
      <span className="timeline-src">
        from {record.sourceKind} {record.sourceId}
      </span>
      {!editing && (
        <span className="work-form">
          <button className="btn btn-ghost" onClick={() => setEditing(true)}>
            Correct
          </button>
          <button
            className="btn btn-ghost"
            onClick={() =>
              action.run(async () => {
                const res = await api.forgetButlerMemory(record.seq);
                setNote(res.note);
                onChanged();
              }, "Forgotten.")
            }
          >
            Forget
          </button>
        </span>
      )}
      {editing && (
        <span className="work-form">
          <input value={text} onChange={(e) => setText(e.target.value)} />
          <button
            className="btn"
            disabled={!text || action.state.phase === "busy"}
            onClick={() =>
              action.run(async () => {
                await api.correctButlerMemory(record.seq, text);
                setEditing(false);
                onChanged();
              }, "Corrected.")
            }
          >
            Save
          </button>
          <button className="btn btn-ghost" onClick={() => setEditing(false)}>
            Cancel
          </button>
        </span>
      )}
      {note && <span className="timeline-src">{note}</span>}
      {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
    </li>
  );
}
