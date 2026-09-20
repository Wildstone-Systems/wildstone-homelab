import { useState } from "react";
import { api, type ButlerRow, type MemoryRecord } from "../../api";
import { ErrorLine, useLoad, when } from "../../lib";
import { Say } from "./ButlerDrawer";

// What this Butler remembers, and where each note came from.
//
// Every record carries its provenance because a memory with no source
// cannot be corrected at its source or invalidated when that source is
// deleted (ADR-W030 A3) — and because a person reading "you train on
// Tuesdays" is owed the answer to "how does it know that".
//
// Correcting rewrites the note and re-stamps the provenance; forgetting
// removes it. Neither is a delete of the underlying material, which is
// offboarding's job and a different act with different consequences.
export function TabMemory({ row, say }: { row: ButlerRow; say: Say }) {
  const b = row.butler;
  // The active Person's Personal/Household material plus this Agent's
  // local memory, subject to both audience and Agent restrictions.
  const memory = useLoad(() => api.butlerMemory({ butler: b.name }), [b.name]);
  const restrictions = useLoad(() => api.agentMemoryRestrictions(b.name), [b.name]);
  const [correcting, setCorrecting] = useState<number | null>(null);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const notes = (memory.data?.records ?? []).filter((m) => !m.forgottenAt);

  // Returns whether it worked, so a caller can close its editor only
  // when there is something to close it over. Clearing the textarea on
  // a failed save would throw away the correction somebody just typed,
  // leaving them with the old note and no copy of the new one.
  async function run(fn: () => Promise<unknown>, done: string, kind: "ok" | "warn" = "ok") {
    setBusy(true);
    setError(null);
    try {
      await fn();
      say(done, kind);
      memory.reload();
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="tab-stack">
      <div className="muted">
        {notes.length === 0
          ? `${b.name} has not been told anything worth remembering yet.`
          : `What ${b.name} remembers, and where each note came from. Correcting rewrites the note; forgetting removes it.`}
      </div>
      <div className="card">
        <h3>Memory access</h3>
        <p className="muted">These limits apply in addition to each person's ownership and the record's audience.</p>
        {["personal", "household"].map((scope) => (
          <label key={scope} className="checkrow">
            <input type="checkbox" disabled={busy || !!b.retiredAt || !restrictions.data}
              checked={!(restrictions.data?.restricted ?? []).includes(scope)}
              onChange={(event) => {
                const restricted = !event.target.checked;
                void run(() => api.setAgentMemoryRestriction(b.name, scope, restricted), "Memory access updated").then(() => restrictions.reload());
              }} />
            Allow {scope === "personal" ? "Personal" : "Household"} memory
          </label>
        ))}
        <ErrorLine error={restrictions.error} />
      </div>
      <ErrorLine error={memory.error} />
      <ErrorLine error={error} />

      <div className="memory-list">
        {notes.map((m) => (
          <div key={m.seq} className="card memory-card">
            <div className="row memory-head">
              <span className="chip chip-static chip-subject">{m.subject || m.kind}</span>
              <span className="muted">{m.scope === "agent_local" ? "Agent-local" : m.scope === "personal" ? "Personal" : "Household"}</span>
              <span className="muted tnum">{provenanceOf(m)}</span>
              <div className="row memory-actions">
                <button
                  className="btn btn-secondary btn-small"
                  disabled={busy}
                  onClick={() => {
                    setCorrecting(m.seq);
                    setDraft(m.content);
                  }}
                >
                  Correct
                </button>
                <button
                  className="btn btn-danger btn-small"
                  disabled={busy}
                  onClick={() =>
                    void run(
                      () => api.forgetButlerMemory(m.seq),
                      `Forgotten. The note is gone from what ${b.name} knows.`,
                      "warn",
                    )
                  }
                >
                  Forget
                </button>
              </div>
            </div>
            <div>{m.content}</div>
            {correcting === m.seq && (
              <div className="memory-correct">
                <textarea
                  aria-label="Correction"
                  rows={3}
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                />
                <div className="row">
                  <button
                    className="btn btn-primary btn-small"
                    disabled={busy || !draft.trim()}
                    onClick={() =>
                      void run(() => api.correctButlerMemory(m.seq, draft.trim()), "Corrected").then(
                        (ok) => ok && setCorrecting(null),
                      )
                    }
                  >
                    Save correction
                  </button>
                  <button
                    className="btn btn-ghost btn-small"
                    disabled={busy}
                    onClick={() => setCorrecting(null)}
                  >
                    Cancel
                  </button>
                </div>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

// Where a note came from, in a person's words.
//
// The wire carries sourceKind and author, which are the right things to
// store and the wrong things to print: "chat/tom" is a lookup key, and
// "learned from chat · 6d ago" is the same fact as a sentence.
export function provenanceOf(m: MemoryRecord): string {
  const at = when(m.createdAt);
  if (m.supersededBy) return `corrected · ${at}`;
  if (m.sourceKind === "correction") return `corrected by ${m.author} · ${at}`;
  // A group conversation says so, because "you said this in front of
  // people" is the difference ADR-W030 A3.1 records and the one a
  // person is most likely to want back.
  if (m.sourceKind === "group-conversation") return `told in a group · ${at}`;
  if (m.sourceKind === "conversation") return `told by ${m.author} · ${at}`;
  return `learned from ${m.sourceKind || "chat"} · ${at}`;
}
