import { ArrowLeft } from "@phosphor-icons/react";
import { api, type HouseholdEvent, type HouseholdRoutine, type RoutineDetail as Detail } from "../api";
import type { AutomationSpec } from "../generated/resources";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { Kind, StatusPill } from "../ui/status";
import { ROUTINE_MEANING, ROUTINE_TONE, type Mode } from "./Routines";

// One routine: what it does, what it has been doing, and who may change
// it (ADR-W024, ADR-W030 A6/A8).
//
// The document has been on the wire since A6 and nothing rendered it, so
// a household could turn a routine on and off without ever being shown
// what it was agreeing to. This is that screen.
//
// It replaces the list rather than opening a drawer: the runs table and
// the revision list do not fit a 440px panel, and the flow continues
// into a form.

export function RoutineDetail({
  name,
  onMode,
  onChanged,
}: {
  name: string;
  onMode: (m: Mode) => void;
  onChanged: () => void;
}) {
  const detail = useLoad(() => api.householdRoutine(name), [name]);
  const settings = useLoad(() => api.butlerSettings());
  const action = useAction();

  const d = detail.data;
  const r = d?.routine;

  // The state actions keep the labels and the messages the old screen
  // used. They are the household's vocabulary for this, not ours to
  // re-pick.
  const act = (state: string, message: string) =>
    action.run(async () => {
      await api.setRoutineState(name, state);
      detail.reload();
      onChanged();
    }, message);

  return (
    <>
      <div className="row">
        <button className="btn btn-ghost" onClick={() => onMode({ kind: "list" })}>
          <ArrowLeft size={15} /> Routines
        </button>
      </div>

      <ErrorLine error={detail.error} />
      {!r && detail.loading && <div className="muted">Loading…</div>}

      {r && (
        <>
          <div className="row">
            <span className="problem-title">{r.name}</span>
            <StatusPill kind={ROUTINE_TONE[r.state]}>{r.state}</StatusPill>
            <span className="muted tnum">rev {r.revision}</span>
          </div>
          <div className="sub">
            {r.ownership === "wildstone"
              ? `run by Wildstone · author ${r.author} · changed ${ago(r.updatedAt)} by ${r.updatedBy}`
              : `runs inside ${r.connection} — Wildstone only knows about it`}
          </div>
          <div className="muted" style={{ marginBottom: "var(--space-4)" }}>
            {ROUTINE_MEANING[r.state]}
          </div>

          {/* A native routine is read-only everywhere. There is nothing
              to show and nothing to press: Wildstone does not hold the
              spec and does not run it. */}
          {r.ownership === "native" ? (
            <div className="callout">
              This routine lives in {r.connection}. Wildstone records that it exists so the picture
              is complete, and changes it nowhere.
            </div>
          ) : (
            <>
              <div className="row">
                <button className="btn btn-primary" onClick={() => onMode({ kind: "edit", name })}>
                  Edit
                </button>
                <button
                  className="btn btn-secondary"
                  // Nothing has been changed here, so there is no draft:
                  // this replays the routine as it stands.
                  onClick={() => onMode({ kind: "dryrun", name, spec: null, rule: d.rule })}
                >
                  Dry-run
                </button>
                {r.state !== "trial" && (
                  <button
                    className="btn btn-secondary"
                    onClick={() => act("trial", "Watching without acting.")}
                  >
                    Watch only
                  </button>
                )}
                {r.state !== "enabled" && (
                  <button className="btn btn-secondary" onClick={() => act("enabled", "Running.")}>
                    Let it act
                  </button>
                )}
                {r.state !== "disabled" && (
                  <button className="btn btn-danger" onClick={() => act("disabled", "Stopped.")}>
                    Stop it
                  </button>
                )}
              </div>
              {action.state.phase === "error" && <ErrorLine error={action.state.message} />}
              {action.state.phase === "done" && (
                <div className="msg-note">{action.state.message}</div>
              )}

              <div className="routine-detail">
                <div>
                  <WhatItDoes spec={d.spec} specError={d.specError} document={r.document} />
                  <Runs runs={d.runs} routine={r} />
                </div>
                <div>
                  <Revisions revisions={d.revisions} />
                  <WhoMayChangeIt />
                  <QuietHours settings={settings.data?.settings} />
                </div>
              </div>
            </>
          )}
        </>
      )}
    </>
  );
}

// The spec, in the schema's own field names. Inventing friendlier labels
// would mean a household reads one vocabulary here and another in the
// document, the CLI and every error the engine returns.
export function WhatItDoes({
  spec,
  specError,
  document,
}: {
  spec: AutomationSpec | null;
  specError?: string;
  document: string;
}) {
  return (
    <section className="card">
      <h3>What it does</h3>
      {specError || !spec ? (
        <>
          {/* A routine whose spec will not parse is exactly the one
              someone needs to look at, so this shows the document rather
              than an apology. */}
          <div className="callout callout-warn">
            This document does not parse, so Wildstone cannot run it. {specError}
          </div>
          <pre className="doc">{document}</pre>
        </>
      ) : (
        <>
          <p>{spec.description}</p>
          <div className="deflist">
            <div>
              <span>trigger</span>
              <span>
                {spec.trigger.eventType}
                {spec.trigger.subject && ` · ${spec.trigger.subject}`}
              </span>
            </div>
            {(spec.conditions ?? []).map((c, i) => (
              <div key={i}>
                <span>{i === 0 ? "conditions" : ""}</span>
                <span>
                  {[
                    c.kind,
                    c.eventType,
                    c.window && `window ${c.window}`,
                    c.after && `after ${c.after}`,
                    c.before && `before ${c.before}`,
                    c.days?.length ? c.days.join(",") : "",
                    c.sameSubject ? "same subject" : "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              </div>
            ))}
            {(spec.context ?? []).map((c, i) => (
              <div key={i}>
                <span>{i === 0 ? "context" : ""}</span>
                <span>
                  {[c.kind, c.eventType, c.limit && `limit ${c.limit}`].filter(Boolean).join(" · ")}
                </span>
              </div>
            ))}
            {spec.escalation && (
              <div>
                <span>escalation</span>
                <span>
                  {spec.escalation.prompt}
                  {spec.escalation.into && ` → ${spec.escalation.into}`}
                </span>
              </div>
            )}
            <div>
              <span>action</span>
              <span>
                {spec.action.tool} · tier {spec.action.tier}
              </span>
            </div>
            {/* What the action will actually do. The tool and the tier
                say how much authority it has; the params say what gets
                sent and to whom, and this screen is where someone
                presses "Let it act". Showing the first two without the
                third asks for a decision on a summary. */}
            {Object.entries(spec.action.params ?? {}).map(([k, v]) => (
              <div key={k}>
                <span>{`· ${k}`}</span>
                <span>{v}</span>
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  );
}

// One occurrence: a firing and everything it set off.
//
// The stream records a decision and its consequences as separate events
// sharing a correlation id, so a table of raw events would show one
// evening as three rows and make "did it actually do anything" a
// reading exercise. Grouping is the whole reason the field is on the
// envelope.
export type Occurrence = {
  key: string;
  at: string;
  subject?: string;
  outcome: string;
  tone: Kind;
  acted: boolean;
};

export function occurrences(runs: HouseholdEvent[]): Occurrence[] {
  const groups = new Map<string, HouseholdEvent[]>();
  for (const e of runs) {
    // An event with no correlation id is its own story. Grouping them
    // under the empty string would collapse every uncorrelated firing
    // in the window into one row.
    const key = e.correlationId || `seq:${e.seq}`;
    const g = groups.get(key);
    if (g) g.push(e);
    else groups.set(key, [e]);
  }
  return [...groups.entries()].map(([key, events]) => {
    const types = new Set(events.map((e) => e.type));
    const has = (t: string) => types.has(`automation.${t}`);
    const acted = has("acted");
    // Only a failed ACTION is a failure. A failed or skipped escalation
    // is the Butler not answering, and ADR-W024 A6 makes that a designed
    // path rather than a fault: the rule acts anyway, with the wording
    // its author wrote. Painting that red would report a routine as
    // broken on the exact occasion the deterministic backbone did its
    // job.
    const fellBack = has("escalation_failed") || has("escalation_skipped");
    let outcome = "suppressed";
    let tone: Kind = "mute";
    if (has("action_failed")) {
      outcome = "failed";
      tone = "bad";
    } else if (acted) {
      // Still worth saying which wording went out: the household asked
      // for the Butler's and got the rule's.
      outcome = fellBack ? "acted · own wording" : "acted";
      tone = "ok";
    } else if (has("escalated")) {
      outcome = "asked first";
      tone = "warn";
    } else if (has("fired")) {
      outcome = "fired";
      tone = "mute";
    }
    const at = events.reduce(
      (earliest, e) => (e.occurredAt < earliest ? e.occurredAt : earliest),
      events[0].occurredAt,
    );
    return {
      key,
      at,
      subject: events.find((e) => e.subject)?.subject,
      outcome,
      tone,
      acted,
    };
  });
}

export function Runs({ runs, routine }: { runs: HouseholdEvent[]; routine: HouseholdRoutine }) {
  const rows = occurrences(runs);
  return (
    <section className="card">
      <h3>Runs</h3>
      <div className="muted">The last 7 days.</div>
      {rows.length === 0 ? (
        <div className="muted">
          {routine.state === "enabled" || routine.state === "trial"
            ? "Nothing has triggered it this week."
            : "It has not been running."}
        </div>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">when</th>
                <th scope="col">subject</th>
                <th scope="col">outcome</th>
                <th scope="col">acted</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((o) => (
                <tr key={o.key}>
                  <td className="tnum" title={when(o.at)}>
                    {ago(o.at)}
                  </td>
                  <td>{o.subject ?? <span className="muted">unmapped</span>}</td>
                  <td>
                    <StatusPill kind={o.tone}>{o.outcome}</StatusPill>
                  </td>
                  {/* What makes trial legible. A routine in trial does
                      everything except act, and this is the column
                      where you can see that it kept its word. */}
                  <td className="muted">
                    {o.acted ? "yes" : routine.state === "trial" ? "no · trial" : "no"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export function Revisions({ revisions }: { revisions: Detail["revisions"] }) {
  return (
    <section className="card">
      <h3>Revisions</h3>
      {revisions.length === 0 ? (
        <div className="muted">No recorded history.</div>
      ) : (
        <ul className="work-evidence">
          {revisions.map((v) => (
            <li key={v.revision}>
              <span className="tnum">rev {v.revision}</span>
              <strong>{v.author}</strong>
              <span className="timeline-src" title={when(v.at)}>
                {ago(v.at)}
              </span>
              {v.note && <span className="timeline-src">{v.note}</span>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function WhoMayChangeIt() {
  return (
    <section className="card">
      <h3>Who may change it</h3>
      <div className="muted">
        Any trusted adult in this house can stop this routine or put it back in trial. Changing what
        it does is recorded as a new revision, under the name of whoever made it.
      </div>
    </section>
  );
}

// Quiet hours are a property of the house, not of this routine
// (ButlerSettingsView). The card says so rather than letting its
// position on a routine screen imply otherwise — per-routine quiet hours
// would be a schema change, and a screen that quietly suggests a setting
// exists is worse than one that names the real one.
function QuietHours({ settings }: { settings?: { quietStartMin: number; quietEndMin: number; timezone: string } }) {
  if (!settings) return null;
  const clock = (min: number) =>
    `${String(Math.floor(min / 60)).padStart(2, "0")}:${String(min % 60).padStart(2, "0")}`;
  return (
    <section className="card">
      <h3>Quiet hours</h3>
      <div className="muted">
        Household-wide, not this routine's own: {clock(settings.quietStartMin)} to{" "}
        {clock(settings.quietEndMin)} {settings.timezone}.
      </div>
    </section>
  );
}
