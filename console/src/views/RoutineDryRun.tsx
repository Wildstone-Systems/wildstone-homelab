import { ArrowLeft } from "@phosphor-icons/react";
import { api, type ReplayDecision, type ReplayResult } from "../api";
import type { AutomationSpec } from "../generated/resources";
import { ago, ErrorLine, useLoad, when } from "../lib";
import { StatusPill } from "../ui/status";
import type { Mode } from "./Routines";

// Replaying a change against the last 7 days (ADR-W024 A7/H3).
//
// A household is otherwise asked to accept a change to a routine with no
// way to see what the change would have done. The replay is
// side-effect-free and shares its implementation with `wild automation
// replay` and the Butler's dry_run_automation tool, so a rule the Butler
// tried is a rule an operator can try identically and neither gets a
// different verdict.
//
// Two replays run: the rule as it stands, and the draft. The diff is the
// screen.

// A day the two rules were asked about, and what each said.
export type Difference = {
  eventId: string;
  at: string;
  subject?: string;
  eventType: string;
  // Absent when a rule never saw this event at all — a changed trigger
  // means the two rules are not asked the same questions.
  current?: ReplayDecision;
  draft?: ReplayDecision;
  changed: boolean;
};

// diffReplays lines two replays up by event.
//
// Keyed on eventId, which is the only stable identifier across the two:
// `at` collides whenever two events share a second, and eventSeq is the
// household stream's rather than the replay's.
export function diffReplays(current: ReplayResult | null, draft: ReplayResult | null): Difference[] {
  const byId = new Map<string, Difference>();
  const put = (d: ReplayDecision, side: "current" | "draft") => {
    const existing = byId.get(d.eventId);
    const row: Difference = existing ?? {
      eventId: d.eventId,
      at: d.at,
      subject: d.subject,
      eventType: d.eventType,
      changed: false,
    };
    row[side] = d;
    byId.set(d.eventId, row);
  };
  for (const d of current?.decisions ?? []) put(d, "current");
  for (const d of draft?.decisions ?? []) put(d, "draft");

  const rows = [...byId.values()];
  for (const row of rows) {
    // An event only one rule ever saw is a difference by itself: the
    // change moved what the routine is asked about.
    row.changed = !row.current || !row.draft || row.current.fired !== row.draft.fired;
  }
  return rows.sort((a, b) => (a.at < b.at ? 1 : -1));
}

// summarise says what changed, in counted terms.
//
// Computed, never editorialised. "Nothing differs." rather than "0 of 5
// differ", which reads as a bug; and a window that held no matching
// events says so rather than reporting agreement between two rules that
// never ran.
export function summarise(rows: Difference[]): string {
  if (rows.length === 0) {
    return "Nothing in the last 7 days would have triggered either version, so there is nothing to compare.";
  }
  const changed = rows.filter((r) => r.changed);
  if (changed.length === 0) {
    return `Nothing differs. Both versions agree on all ${rows.length} of the last 7 days' triggers.`;
  }
  const lost = changed.filter((r) => r.current?.fired && !r.draft?.fired).length;
  const gained = changed.filter((r) => !r.current?.fired && r.draft?.fired).length;
  const parts = [`${changed.length} of ${rows.length} differ.`];
  if (lost > 0) parts.push(`${lost} ${lost === 1 ? "case it no longer catches" : "cases it no longer catches"}.`);
  if (gained > 0) parts.push(`${gained} ${gained === 1 ? "case it newly catches" : "cases it newly catches"}.`);
  return parts.join(" ");
}

export function RoutineDryRun({
  name,
  spec,
  rule,
  onMode,
}: {
  name: string;
  // The draft. Null means "replay what is stored" — the detail screen's
  // Dry-run button, which has changed nothing.
  spec: AutomationSpec | null;
  // The rule name the engine knows, for the current-revision replay.
  rule: string;
  onMode: (m: Mode) => void;
}) {
  // Both replays at once: they are independent reads and the screen
  // needs both before it can say anything.
  const replays = useLoad(
    () =>
      Promise.all([
        // A routine that has never been approved into the Statefile has
        // no current revision to compare against. That is not an error
        // state; it is a draft.
        api.replayAutomation({ name: rule }).catch(() => null),
        spec
          ? api.replayAutomation({ routine: name, spec })
          : api.replayAutomation({ name: rule }).catch(() => null),
      ]),
    [name, rule, JSON.stringify(spec)],
  );

  const [current, draft] = replays.data ?? [null, null];
  const rows = diffReplays(current, draft);
  const violations = draft?.violations ?? [];

  return (
    <>
      <div className="row">
        <button className="btn btn-ghost" onClick={() => onMode({ kind: "edit", name })}>
          <ArrowLeft size={15} /> Back to edit
        </button>
      </div>

      <div className="sub">
        Replayed against the last 7 days of the household timeline. Actions stubbed — nothing was
        sent.
      </div>

      <ErrorLine error={replays.error} />
      {replays.loading && <div className="muted">Replaying…</div>}

      {!replays.loading && !current && (
        <div className="callout">
          This routine has no running revision to compare against, so this is what the draft alone
          would have done.
        </div>
      )}

      {violations.length > 0 && (
        <div className="callout callout-warn">
          <strong>This draft would be refused as a proposal.</strong>
          <ul>
            {violations.map((v, i) => (
              <li key={i}>
                {v.rule ? `${v.rule}: ` : ""}
                {v.message}
              </li>
            ))}
          </ul>
        </div>
      )}

      {!replays.loading && (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">when</th>
                <th scope="col">subject</th>
                <th scope="col">now</th>
                <th scope="col">after</th>
                <th scope="col">why</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.eventId}>
                  <td className="tnum" title={when(r.at)}>
                    {ago(r.at)}
                  </td>
                  <td>{r.subject ?? <span className="muted">unmapped</span>}</td>
                  <td>
                    <Verdict decision={r.current} />
                  </td>
                  <td>
                    <Verdict decision={r.draft} />
                  </td>
                  <td className="muted">{(r.draft ?? r.current)?.reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {!replays.loading && (
        <div className={rows.some((r) => r.changed) ? "callout callout-warn" : "callout"}>
          {summarise(rows)}
        </div>
      )}

      <div className="row">
        <button className="btn btn-secondary" onClick={() => onMode({ kind: "edit", name })}>
          Back to edit
        </button>
        <button
          className="btn btn-primary"
          style={{ marginLeft: "auto" }}
          onClick={() => onMode({ kind: "detail", name })}
        >
          Done
        </button>
      </div>
    </>
  );
}

// An absent verdict and a suppressed one are different facts: one rule
// was never asked about this event.
function Verdict({ decision }: { decision?: ReplayDecision }) {
  if (!decision) return <span className="muted">—</span>;
  return (
    <StatusPill kind={decision.fired ? "ok" : "mute"}>
      {decision.fired ? "would act" : "quiet"}
    </StatusPill>
  );
}
