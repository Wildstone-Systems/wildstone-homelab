import { useState } from "react";
import { ForkKnife, House, Repeat, Sparkle, WarningCircle } from "@phosphor-icons/react";
import { api, type HouseholdEvent } from "../api";
import { ago, ErrorLine, useLoad, when } from "../lib";
import { Drawer } from "../ui/Drawer";
import { eventKind } from "../ui/HourChart";
import { Dot } from "../ui/status";

// The household stream (ADR-W024 A2).
//
// A second stream with its own envelope and its own cursor, deliberately
// not the operational one: what the HOUSE did, rather than what the
// control plane did. It has had an intake and a cursor endpoint since
// A2 and no view at all, so a household could not see the events its
// own routines fire on.
//
// Its own useLoad rather than the shell's poll. The shell carries the
// operational stream only, and widening ConsoleStateProvider before this
// stream is proven cheap would put a household's whole timeline behind
// every screen in the Console.

type Filter = "all" | "person" | "type" | "source" | "routines" | "unmapped";

const FILTERS: { id: Filter; label: string }[] = [
  { id: "all", label: "Everything" },
  { id: "person", label: "By person" },
  { id: "type", label: "By type" },
  { id: "source", label: "By source" },
  { id: "routines", label: "Routine outcomes" },
  // ADR-W024 A8: an unmapped subject is a valid event that simply
  // cannot fire a per-member rule. A routine that silently never fires
  // is the bug these rows explain, so they get a filter of their own.
  { id: "unmapped", label: "Unmapped subjects" },
];

const LIMIT = 200;

export function HouseholdStream({ onOpenRoutine }: { onOpenRoutine?: (name: string) => void }) {
  const events = useLoad(() => api.householdEvents(0, LIMIT));
  return (
    <HouseholdStreamView
      events={events.data ?? []}
      error={events.error}
      loading={events.loading}
      onOpenRoutine={onOpenRoutine}
    />
  );
}

export function HouseholdStreamView({
  events,
  error,
  loading,
  now = new Date(),
  onOpenRoutine,
}: {
  events: HouseholdEvent[];
  error: string | null;
  loading: boolean;
  now?: Date;
  onOpenRoutine?: (name: string) => void;
}) {
  const [filter, setFilter] = useState<Filter>("all");
  const [selected, setSelected] = useState<HouseholdEvent | null>(null);
  const [opened, setOpened] = useState<HouseholdEvent | null>(null);
  const [subject, setSubject] = useState("");
  const [type, setType] = useState("");
  const [source, setSource] = useState("");

  const subjects = [...new Set(events.map((e) => e.subject).filter((s): s is string => !!s))].sort();
  const types = [...new Set(events.map((e) => e.type))].sort();
  const sources = [...new Set(events.map((e) => e.source))].sort();

  const rows = matchingEvents(events, { filter, subject, type, source });

  return (
    <>
      <div className="sub">
        What the house did, newest first. This is the household's own timeline — the events routines
        fire on — and not the control plane's.
      </div>
      <ErrorLine error={error} />

      <div className="row">
        {FILTERS.map((f) => (
          <button
            key={f.id}
            className={"btn " + (filter === f.id ? "btn-primary" : "btn-secondary")}
            onClick={() => setFilter(f.id)}
          >
            {f.label}
          </button>
        ))}
        {filter === "person" && (
          <select value={subject} onChange={(e) => setSubject(e.target.value)} aria-label="person">
            <option value="">everyone</option>
            {subjects.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        )}
        {filter === "type" && (
          <select value={type} onChange={(e) => setType(e.target.value)} aria-label="type">
            <option value="">every type</option>
            {types.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        )}
        {filter === "source" && (
          <select value={source} onChange={(e) => setSource(e.target.value)} aria-label="source">
            <option value="">every source</option>
            {sources.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        )}
      </div>

      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">when</th>
              <th scope="col">what</th>
              <th scope="col">subject</th>
              <th scope="col" className="col-source">source</th>
              <th scope="col">detail</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((e) => (
              <tr
                key={e.seq}
                // A firing and its outcome are one story, which is the
                // whole reason correlation_id is on the envelope.
                className={correlated(e, selected) ? "correlated" : undefined}
                // Selecting is a real control, so it answers to the
                // keyboard as well as the mouse. A row that only responds
                // to a click is a control a keyboard cannot reach.
                tabIndex={0}
                aria-pressed={selected?.seq === e.seq}
                onClick={() => setSelected(selected?.seq === e.seq ? null : e)}
                onKeyDown={(ev) => {
                  if (ev.key === "Enter" || ev.key === " ") {
                    ev.preventDefault();
                    setSelected(selected?.seq === e.seq ? null : e);
                  }
                  if (ev.key === "Escape") setSelected(null);
                }}
                style={{ cursor: "pointer" }}
              >
                <td className="tnum" title={when(e.occurredAt)} style={{ whiteSpace: "nowrap" }}>
                  {ago(e.occurredAt, now)}
                </td>
                <td>
                  <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                    <Dot kind={eventKind(e.type)} />
                    <HouseholdIcon type={e.type} />
                    <span className="mono">{e.type}</span>
                  </span>
                </td>
                <td>
                  {e.subject ?? (
                    // Not hidden and not blank. A8 makes this a real
                    // event that cannot fire a per-member rule, and that
                    // is the answer to "why did my routine never run".
                    <span style={{ color: "var(--st-warn)" }}>unmapped</span>
                  )}
                </td>
                <td className="muted mono col-source">{e.source}</td>
                <td className="muted">
                  <span className="only-narrow mono">{e.source} </span>
                  {e.entity ?? ""}{" "}
                  <button
                    className="btn btn-ghost"
                    aria-label={`Envelope for ${e.type}`}
                    onClick={(ev) => {
                      ev.stopPropagation();
                      setOpened(e);
                    }}
                  >
                    Envelope
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {rows.length === 0 && (
        <div className="muted">
          {loading
            ? "Loading…"
            : events.length === 0
              ? "Nothing has been published to the household stream yet."
              : "Nothing matches this filter."}
        </div>
      )}

      <Drawer open={!!opened} onClose={() => setOpened(null)} title="Event" wide>
        {opened && <Envelope event={opened} events={events} onOpenRoutine={onOpenRoutine} />}
      </Drawer>
    </>
  );
}

// correlated reports whether row shares a story with the selected one.
//
// An event with no correlation id correlates with NOTHING. Grouping on
// the empty string would light up every uncorrelated event on the stream
// at once, which is the exact opposite of the point.
export function correlated(row: HouseholdEvent, selected: HouseholdEvent | null): boolean {
  if (!selected || !selected.correlationId) return false;
  return row.correlationId === selected.correlationId;
}

// setOff finds what an event led to: the engine's own records sharing
// its correlation id, oldest first, since this is a sequence.
export function setOff(event: HouseholdEvent, events: HouseholdEvent[]): HouseholdEvent[] {
  if (!event.correlationId) return [];
  return events
    .filter(
      (e) =>
        e.seq !== event.seq &&
        e.correlationId === event.correlationId &&
        e.type.startsWith("automation."),
    )
    .sort((a, b) => a.seq - b.seq);
}

// routineOf names the rule a correlated automation record belongs to, so
// the drawer can offer to open it. Absent when nothing names one — and
// then the button is not rendered rather than pointing nowhere.
export function routineOf(chain: HouseholdEvent[]): string | undefined {
  return chain.find((e) => !!e.entity)?.entity;
}

export function Envelope({
  event,
  events,
  onOpenRoutine,
}: {
  event: HouseholdEvent;
  events: HouseholdEvent[];
  onOpenRoutine?: (name: string) => void;
}) {
  const chain = setOff(event, events);
  const routine = routineOf(chain);
  return (
    <>
      <h3>{event.type}</h3>
      <div className="deflist">
        <div>
          <span>event_id</span>
          <span>{event.eventId}</span>
        </div>
        {/* Both timestamps, both labelled. When a publisher batches or
            retries they differ, and that difference is the answer to
            "why did this fire late". */}
        <div>
          <span>occurred_at</span>
          <span>{when(event.occurredAt)}</span>
        </div>
        <div>
          <span>recorded_at</span>
          <span>{when(event.recordedAt)}</span>
        </div>
        <div>
          <span>subject</span>
          <span>{event.subject ?? "unmapped"}</span>
        </div>
        <div>
          <span>source</span>
          <span>{event.source}</span>
        </div>
        <div>
          <span>correlation</span>
          <span>{event.correlationId ?? "—"}</span>
        </div>
        <div>
          <span>schema</span>
          <span>{event.schemaVersion}</span>
        </div>
      </div>
      <div className="muted">
        The source is stamped from the publisher's own token, never read off the payload — it is a
        fact rather than a claim.
      </div>

      <h3>Payload</h3>
      {/* Rendered as text, always. This is JSON from outside the house:
          no markup, no auto-linking. A payload that renders as a
          clickable link is a phishing surface on the one screen whose
          job is to say the payload is untrusted. */}
      <pre className="doc">{JSON.stringify(event.payload ?? {}, null, 2)}</pre>

      {/* Not decoration. ADR-W024 A4 makes this a property of the
          system, and the screen that displays payloads is where a
          household learns it. */}
      <div className="callout callout-warn">
        Payload text is untrusted input. Redacted and enveloped before any model reads it.
      </div>

      <h3>What this set off</h3>
      {chain.length === 0 ? (
        <div className="muted">Nothing followed from this event.</div>
      ) : (
        <ul className="work-evidence">
          {chain.map((e) => (
            <li key={e.seq}>
              <Dot kind={eventKind(e.type)} />
              <span className="mono">{e.type}</span>
              {e.entity && <span className="timeline-src">{e.entity}</span>}
              <span className="timeline-src">{when(e.occurredAt)}</span>
            </li>
          ))}
        </ul>
      )}

      {routine && onOpenRoutine && (
        <div className="row">
          <button className="btn btn-secondary" onClick={() => onOpenRoutine(routine)}>
            Open routine
          </button>
        </div>
      )}
    </>
  );
}

// matchingEvents is the filter, pulled out so it can be tested without
// a DOM.
export function matchingEvents(
  events: HouseholdEvent[],
  q: { filter: Filter; subject: string; type: string; source: string },
): HouseholdEvent[] {
  return [...events]
    .sort((a, b) => b.seq - a.seq)
    .filter((e) => {
      switch (q.filter) {
        case "person":
          return q.subject === "" ? !!e.subject : e.subject === q.subject;
        case "type":
          return q.type === "" ? true : e.type === q.type;
        case "source":
          return q.source === "" ? true : e.source === q.source;
        case "routines":
          return e.type.startsWith("automation.");
        case "unmapped":
          return !e.subject;
        default:
          return true;
      }
    });
}

// What kind of thing happened. The dot beside it says how it went.
export function HouseholdIcon({ type }: { type: string }) {
  const size = 14;
  if (type.startsWith("automation.")) return <Repeat size={size} />;
  if (type.startsWith("person.")) return <House size={size} />;
  if (type.startsWith("pantry.")) return <ForkKnife size={size} />;
  if (type.startsWith("butler.")) return <Sparkle size={size} />;
  return <WarningCircle size={size} />;
}
