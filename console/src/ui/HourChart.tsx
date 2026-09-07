import { EventRow } from "../api";
import { Kind, KIND_COLOR } from "./status";

// What a control plane's event types mean, by shape rather than by a
// list that goes stale the moment someone adds one. A type nobody here
// has an opinion about is drawn as ordinary traffic, not as a fault.
export function eventKind(type: string): Kind {
  if (/\.(failed|blocked)$/.test(type) || type.includes("disconnected")) return "bad";
  if (/\.(proposed|superseded|pending-approval|skipped-offline|foreign)$/.test(type)) return "warn";
  if (/\.(applied|approved|repaired|completed|started|minted|rotated|set)$/.test(type)) return "ok";
  return "mute";
}

const RANK: Record<Kind, number> = { bad: 0, warn: 1, ok: 2, mute: 3 };

// The last hour, binned. Each bar is the worst thing that happened in
// its slice of time, so a single failure stays visible next to a
// hundred routine ticks instead of being averaged away.
export function HourChart({
  events,
  buckets = 24,
  minutes = 60,
  now = new Date(),
}: {
  events: EventRow[];
  buckets?: number;
  minutes?: number;
  now?: Date;
}) {
  const span = minutes * 60_000;
  const start = now.getTime() - span;
  const width = span / buckets;

  const bins: (Kind | undefined)[] = Array(buckets).fill(undefined);
  const counts = Array(buckets).fill(0);
  for (const e of events) {
    const t = new Date(e.time).getTime();
    if (isNaN(t) || t < start || t > now.getTime()) continue;
    const i = Math.min(buckets - 1, Math.floor((t - start) / width));
    counts[i]++;
    const k = eventKind(e.type);
    const cur = bins[i];
    if (!cur || RANK[k] < RANK[cur]) bins[i] = k;
  }

  const label = (i: number) =>
    new Date(start + i * width).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

  return (
    <div className="cell" style={{ flex: 1, minWidth: 250, borderRight: 0 }}>
      <div className="eyebrow" style={{ marginBottom: "var(--space-3)" }}>
        Last hour
      </div>
      <div style={{ display: "flex", gap: 3, height: 36, alignItems: "flex-end" }}>
        {bins.map((k, i) => (
          <span
            key={i}
            title={`${label(i)} · ${counts[i]} event${counts[i] === 1 ? "" : "s"}`}
            style={{
              flex: 1,
              borderRadius: 2,
              // An empty slice still draws a hairline: a gap that reads
              // as "nothing happened" is different from one that reads
              // as "the chart stops here".
              height: k === undefined ? 3 : k === "bad" ? 36 : k === "warn" ? 24 : 14,
              background: k === undefined ? "var(--hair)" : KIND_COLOR[k],
              opacity: k === "ok" ? 0.55 : 1,
            }}
          />
        ))}
      </div>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          fontSize: 10.5,
          color: "var(--muted)",
          marginTop: 5,
        }}
      >
        <span className="tnum">{label(0)}</span>
        <span className="tnum">{label(buckets)}</span>
      </div>
    </div>
  );
}
