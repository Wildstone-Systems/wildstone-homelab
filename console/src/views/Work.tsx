import { useState } from "react";
import {
  api,
  FEEDBACK_LABELS,
  type ButlerTask,
  type FeedbackLabel,
  type TaskDetail,
  type TimelineEntry,
} from "../api";
import { ErrorLine, useAction, useLoad, when } from "../lib";
import { Kind, StatusPill } from "../ui/status";

// Durable work, and how to explain a piece of it (ADR-W030 A11, P5).
//
// The Butler view shows a conversation: what was asked and what was
// answered, with one turn's tool calls expandable. That answers "what
// did it read to say this" and not the questions a failed or resumed
// task actually raises — what was it trying to do, what had it
// established before it stopped, what refused it, what is it waiting
// for, and what finally justified calling it done.
//
// This screen answers those, from the permission-filtered timeline. Two
// things it deliberately does NOT do:
//
//   - It never shows what a source SAID. The trace carries counts,
//     flags, tool names and versions; the reading itself belongs to its
//     source and is read from there, under the reader's own
//     permissions. So there is nothing here to redact, because there is
//     nothing here to leak.
//   - It never presents an incomplete story as a complete one. Entries
//     this reader may not see are counted and said out loud.

const STATE_TONE: Record<ButlerTask["state"], Kind> = {
  pending: "mute",
  running: "ok",
  waiting: "warn",
  completed: "ok",
  failed: "bad",
  cancelled: "mute",
  "paused-offboarded": "warn",
};

// What each state means in a sentence, because "waiting" and "paused-
// offboarded" are not self-explanatory to the person reading them at
// eight in the morning.
const STATE_MEANING: Record<ButlerTask["state"], string> = {
  pending: "queued, not started",
  running: "working on it now",
  waiting: "stopped on purpose, waiting for something",
  completed: "finished, with evidence",
  failed: "stopped and will not resume on its own",
  cancelled: "stopped because someone asked it to",
  "paused-offboarded": "paused because its owner was offboarded; needs review",
};

// The trace's event vocabulary, in words rather than dotted names.
const EVENT_LABEL: Record<string, string> = {
  "slice.started": "started a turn",
  "model.call": "asked the model",
  "model.answer": "the model replied",
  "tool.dispatched": "ran a tool",
  "tool.result": "got a result",
  "attempt.fenced": "abandoned a stalled attempt",
  denial: "was refused",
  "wait.opened": "started waiting",
  "wait.resolved": "stopped waiting",
  "evidence.recorded": "recorded evidence",
  delivery: "delivered an answer",
  outcome: "finished",
};

export function Work() {
  const tasks = useLoad(() => api.butlerTasks());
  const [selected, setSelected] = useState<string | null>(null);

  const list = tasks.data?.tasks ?? [];
  return (
    <>
      <div className="sub">
        Everything the Butler is doing or has done, and how each piece got where it is. A task
        outlives one answer: it survives a restart, so this is also what was interrupted.
      </div>
      <ErrorLine error={tasks.error} />
      {!tasks.loading && list.length === 0 && (
        <div className="muted">
          No durable work yet. Tasks appear here once the Butler starts something that outlives a
          single answer.
        </div>
      )}
      <div className="work-split">
        <ul className="work-list">
          {list.map((t) => (
            <li key={t.id}>
              <button
                className={`work-item ${selected === t.id ? "is-selected" : ""}`}
                onClick={() => setSelected(t.id)}
              >
                <StatusPill kind={STATE_TONE[t.state]}>{t.state}</StatusPill>
                <span className="work-objective">{t.objective}</span>
                <span className="work-meta">
                  {t.owner} · {when(t.updatedAt)}
                </span>
              </button>
            </li>
          ))}
        </ul>
        {selected && <TaskPanel id={selected} onGone={() => setSelected(null)} />}
      </div>
    </>
  );
}

function TaskPanel({ id, onGone }: { id: string; onGone: () => void }) {
  const detail = useLoad(() => api.butlerTask(id), [id]);
  if (detail.error) {
    return (
      <div className="work-detail">
        <ErrorLine error={detail.error} />
        <button className="btn btn-ghost" onClick={onGone}>
          Close
        </button>
      </div>
    );
  }
  if (!detail.data) return <div className="work-detail">Loading…</div>;
  const d = detail.data;
  return (
    <div className="work-detail">
      <h2>{d.task.objective}</h2>
      <p className="sub">
        <StatusPill kind={STATE_TONE[d.task.state]}>{d.task.state}</StatusPill>{" "}
        {STATE_MEANING[d.task.state]} · asked by {d.task.author}
        {d.task.owner !== d.task.author && ` · now owned by ${d.task.owner}`}
      </p>
      {d.task.completionCriteria && (
        <p className="sub">Done when: {d.task.completionCriteria}</p>
      )}
      {d.task.uncertainty && (
        <div className="msg-note">
          One outcome is unknown, so it will not retry that step on its own: {d.task.uncertainty}
        </div>
      )}
      {d.debugActive && (
        <div className="msg-note">
          Extra debug capture is on for this task. It expires by itself; nothing has to remember to
          turn it off.
        </div>
      )}

      <Waiting detail={d} />
      <Timeline entries={d.timeline} withheld={d.withheld} />
      <Evidence detail={d} />
      <Labels id={id} detail={d} onLabelled={detail.reload} />
      <ExportEvaluation id={id} state={d.task.state} />
    </div>
  );
}

// What it is waiting for, said plainly. An open wait is the difference
// between "stalled" and "stopped", and the Console has to be able to
// tell a person which one they are looking at.
function Waiting({ detail }: { detail: TaskDetail }) {
  const open = detail.waits.filter((w) => !w.resolvedAt);
  if (open.length === 0) return null;
  return (
    <section className="work-section">
      <h3>Waiting</h3>
      {open.map((w) => (
        <div key={w.seq} className="msg-note">
          {WAIT_REASON[w.reason] ?? w.reason}
          {w.detail && ` — ${w.detail}`}
          {w.resumeAt && ` · will pick up again ${when(w.resumeAt)}`}
        </div>
      ))}
    </section>
  );
}

const WAIT_REASON: Record<string, string> = {
  reply: "waiting for you to say continue",
  approval: "waiting for a plan to be approved",
  schedule: "waiting for a scheduled time",
  "model-unavailable": "could not reach the model; nothing is lost",
  "budget-exhausted": "used up this task's budget and needs review before it continues",
  clarification: "waiting for an answer to a question it asked",
  "source-unavailable": "a source it needs is unavailable",
  "uncertain-outcome": "an action's outcome is unknown, so it will not retry blindly",
};

function Timeline({ entries, withheld }: { entries: TimelineEntry[]; withheld: number }) {
  return (
    <section className="work-section">
      <h3>What happened</h3>
      <ol className="timeline">
        {entries.map((e) => (
          <li key={e.traceSeq}>
            <span className="timeline-when">{when(e.at)}</span>
            <span className="timeline-what">{EVENT_LABEL[e.type] ?? e.type}</span>
            {e.sources && e.sources.length > 0 && (
              <span className="timeline-src">{e.sources.join(", ")}</span>
            )}
            {e.modelEndpoint && <span className="timeline-src">{e.modelEndpoint}</span>}
            {e.detail && Object.keys(e.detail).length > 0 && (
              <span className="timeline-detail">{summarize(e.detail)}</span>
            )}
          </li>
        ))}
      </ol>
      {withheld > 0 && (
        <div className="msg-note">
          {withheld} {withheld === 1 ? "entry is" : "entries are"} not shown to you. This is a
          partial view, and saying so is the point.
        </div>
      )}
    </section>
  );
}

// The detail payload is structural — counts and flags — so rendering it
// is a matter of reading it out, not of deciding what is safe to show.
function summarize(detail: Record<string, unknown>): string {
  return Object.entries(detail)
    .map(([k, v]) => `${k}: ${String(v)}`)
    .join(" · ");
}

function Evidence({ detail }: { detail: TaskDetail }) {
  if (detail.evidence.length === 0) return null;
  return (
    <section className="work-section">
      <h3>Why it counts as done</h3>
      <ul className="work-evidence">
        {detail.evidence.map((e) => (
          <li key={e.seq}>
            {e.description}
            {e.sourceId && <span className="timeline-src">{e.sourceId}</span>}
          </li>
        ))}
      </ul>
    </section>
  );
}

// Labels are the improvement loop's input. Butler may recommend a change
// and cannot label its own work; every row here is a person's judgement,
// with the person named.
function Labels({
  id,
  detail,
  onLabelled,
}: {
  id: string;
  detail: TaskDetail;
  onLabelled: () => void;
}) {
  const [label, setLabel] = useState<FeedbackLabel>("wrong-answer");
  const [note, setNote] = useState("");
  const { state, run } = useAction();
  return (
    <section className="work-section">
      <h3>What was wrong with this</h3>
      {detail.feedback.length > 0 && (
        <ul className="work-evidence">
          {detail.feedback.map((f) => (
            <li key={f.seq}>
              <strong>{f.label}</strong> — {f.author}
              {f.note && `: ${f.note}`}
            </li>
          ))}
        </ul>
      )}
      <div className="work-form">
        <select value={label} onChange={(e) => setLabel(e.target.value as FeedbackLabel)}>
          {FEEDBACK_LABELS.map((l) => (
            <option key={l} value={l}>
              {l}
            </option>
          ))}
        </select>
        <input
          value={note}
          placeholder="What happened, in your words"
          onChange={(e) => setNote(e.target.value)}
        />
        <button
          className="btn"
          disabled={state.phase === "busy"}
          onClick={() =>
            run(async () => {
              await api.labelButlerTask(id, label, note);
              setNote("");
              onLabelled();
            }, "Recorded.")
          }
        >
          Record
        </button>
      </div>
      {state.phase === "error" && <ErrorLine error={state.message} />}
      {state.phase === "done" && <div className="msg-note">{state.message}</div>}
    </section>
  );
}

// One reviewed failure becomes a repeatable check. The expectation is
// typed by a person: a fixture whose expectation was written by the
// model that failed it checks nothing.
function ExportEvaluation({ id, state }: { id: string; state: ButlerTask["state"] }) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [expected, setExpected] = useState("");
  const [summary, setSummary] = useState("");
  const action = useAction();
  if (state === "running" || state === "pending") return null;
  return (
    <section className="work-section">
      <h3>Turn this into a check</h3>
      {!open && (
        <button className="btn btn-ghost" onClick={() => setOpen(true)}>
          Make a regression check from this task
        </button>
      )}
      {open && (
        <div className="work-form work-form-column">
          <p className="sub">
            This records the shape of what happened — the objective, the tools it ran and the names
            of their arguments — pinned to the prompt, policy and tool manifest that were in force.
            Argument values, results and answer text are not included.
          </p>
          <input value={name} placeholder="short-name-for-this-check" onChange={(e) => setName(e.target.value)} />
          <input
            value={expected}
            placeholder="What SHOULD have happened"
            onChange={(e) => setExpected(e.target.value)}
          />
          <input
            value={summary}
            placeholder="What went wrong (optional)"
            onChange={(e) => setSummary(e.target.value)}
          />
          <button
            className="btn"
            disabled={action.state.phase === "busy" || !name || !expected}
            onClick={() =>
              action.run(
                () =>
                  api.exportButlerEvaluation(id, {
                    name,
                    expected,
                    summary,
                    evaluatorVersion: "",
                  }),
                "Saved as a regression check.",
              )
            }
          >
            Save check
          </button>
          {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
          {action.state.phase === "done" && <div className="msg-note">{action.state.message}</div>}
        </div>
      )}
    </section>
  );
}
