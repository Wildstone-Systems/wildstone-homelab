import { useState } from "react";
import {
  Archive,
  ArrowsClockwise,
  Key,
  Plugs,
  SealCheck,
  Sparkle,
  WarningCircle,
} from "@phosphor-icons/react";
import { EventRow } from "../api";
import { ago, ErrorLine, when } from "../lib";
import { KEEP, POLL_MS, useConsoleState } from "../state";
import { eventKind, HourChart } from "../ui/HourChart";
import { Dot } from "../ui/status";
import { HouseholdStream } from "./HouseholdStream";

// Activity (ADR-W016 A4, ADR-W027): the operational stream, newest
// first, live off the shell's one poll rather than a second timer of
// its own. Events was the old name; the stream carries plans, backups
// and node sessions too, and "Events" undersold it.
// Two streams, and they are not the same stream. The control plane's is
// what this screen has always shown; the household's (ADR-W024 A2) is
// what the HOUSE did, and until now the Console could not show it at
// all.
type Stream = "control" | "household";

export function Activity({ onOpenRoutine }: { onOpenRoutine?: (name: string) => void }) {
  const { snap, error } = useConsoleState();
  // Defaulting to the control plane: it is what this screen is today,
  // and an operator opening Activity during an incident should not land
  // on a different stream than they did last release.
  const [stream, setStream] = useState<Stream>("control");
  return (
    <>
      <div
        className="tabs"
        role="tablist"
        aria-label="Activity"
        // Arrow keys move between tabs, which is what a tablist is for.
        onKeyDown={(e) => {
          if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
            e.preventDefault();
            setStream(stream === "control" ? "household" : "control");
          }
        }}
      >
        <button
          role="tab"
          aria-selected={stream === "control"}
          // Only the selected tab is in the tab order; the arrow keys
          // move within the set. That is the roving-tabindex the role
          // implies.
          tabIndex={stream === "control" ? 0 : -1}
          className="tab"
          onClick={() => setStream("control")}
        >
          Control plane <span className="tab-count">{snap?.events.length ?? 0}</span>
        </button>
        <button
          role="tab"
          aria-selected={stream === "household"}
          tabIndex={stream === "household" ? 0 : -1}
          className="tab"
          onClick={() => setStream("household")}
        >
          Household
        </button>
      </div>
      {stream === "household" ? (
        <HouseholdStream onOpenRoutine={onOpenRoutine} />
      ) : (
        <ActivityView
          events={snap?.events ?? []}
          now={snap?.lastPollAt ?? new Date()}
          error={snap ? error : null}
          loading={!snap}
        />
      )}
    </>
  );
}

type Filter = "all" | "mine" | "failures" | "node";

const FILTERS: { id: Filter; label: string }[] = [
  { id: "all", label: "Everything" },
  { id: "mine", label: "Needs me" },
  { id: "failures", label: "Failures" },
  { id: "node", label: "By node" },
];

export function ActivityView({
  events,
  now,
  error,
  loading,
}: {
  events: EventRow[];
  now: Date;
  error: string | null;
  loading: boolean;
}) {
  const [filter, setFilter] = useState<Filter>("all");
  const [node, setNode] = useState<string>("");

  const nodes = [...new Set(events.map((e) => e.node).filter((n): n is string => !!n))].sort();
  // Filtering the kept window client-side. The stream is capped at 300
  // rows, so there is nothing here worth a `type=` parameter yet.
  const rows = [...events]
    .reverse()
    .filter((e) => {
      switch (filter) {
        case "mine":
          return eventKind(e.type) === "warn";
        case "failures":
          return eventKind(e.type) === "bad";
        case "node":
          return node === "" ? true : e.node === node;
        default:
          return true;
      }
    });

  return (
    <>
      <div className="strip">
        <HourChart events={events} now={now} />
      </div>

      <div className="sub">
        Everything the control plane did, newest first. Polling every {POLL_MS / 1000}s, keeping
        the last {KEEP} — {events.length} so far this session.
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
        {filter === "node" && (
          <select value={node} onChange={(e) => setNode(e.target.value)} aria-label="node">
            <option value="">every node</option>
            {nodes.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        )}
      </div>

      <table>
        <thead>
          <tr>
            <th scope="col">when</th>
            <th scope="col">what</th>
            <th scope="col">node</th>
            <th scope="col">detail</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((e) => {
            const kind = eventKind(e.type);
            return (
              <tr key={e.seq}>
                <td className="tnum" title={when(e.time)} style={{ whiteSpace: "nowrap" }}>
                  {ago(e.time, now)}
                </td>
                <td>
                  <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                    <Dot kind={kind} />
                    <EventIcon type={e.type} />
                    <span>{e.type}</span>
                  </span>
                </td>
                <td>{e.node ?? ""}</td>
                <td>
                  {e.message ?? ""}
                  {e.attrs && Object.keys(e.attrs).length > 0 && (
                    <span className="muted">
                      {" "}
                      {Object.entries(e.attrs)
                        .filter(([, v]) => v !== "")
                        .map(([k, v]) => `${k}=${v}`)
                        .join(" ")}
                    </span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>

      {rows.length === 0 && (
        <div className="muted">
          {loading
            ? "Waiting for the first poll…"
            : events.length === 0
              ? "Nothing has happened since the console was opened."
              : "Nothing matches this filter."}
        </div>
      )}
    </>
  );
}

// The icon says what KIND of thing happened; the dot beside it says how
// it went. Two glyphs rather than one, because "a backup" and "a backup
// that failed" are different questions.
export function EventIcon({ type }: { type: string }) {
  const size = 14;
  if (type.startsWith("backup.")) return <Archive size={size} />;
  if (type.startsWith("plan.") || type.startsWith("drift.")) return <SealCheck size={size} />;
  if (type.startsWith("reconcile.") || type.startsWith("prereq.")) return <ArrowsClockwise size={size} />;
  if (type.startsWith("node.") || type.includes("connect")) return <Plugs size={size} />;
  if (type.startsWith("secret.")) return <Key size={size} />;
  if (type.startsWith("butler.")) return <Sparkle size={size} />;
  return <WarningCircle size={size} />;
}
