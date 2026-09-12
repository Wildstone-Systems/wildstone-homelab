import { EventRow } from "../api";
import { Kind, KIND_COLOR } from "./status";

// What a control plane's event type means, matched on its VERB rather
// than against a list of the types Keystone happens to emit today. A
// type nobody here has an opinion about is drawn as ordinary traffic,
// which is the right default: an unrecognised event is not a fault.
//
// The verbs, against the stream as it stands: reconcile.applied /
// blocked / failed / skipped-offline, drift.repaired / foreign /
// superseded / pending-approval, prereq.repaired, plan.proposed /
// approved / rejected / superseded, backup.started / completed /
// failed, secret.rotated / rotate-failed, policy.exempted / rejected,
// statefile.adopted / invalid / oob-accepted / oob-reverted,
// token.minted, chat.bound / revoked, butler.session, report.morning.
export function eventKind(type: string): Kind {
  const verb = type.slice(type.lastIndexOf(".") + 1);
  // Something went wrong on its own.
  // The suffix is matched on both separators on purpose. The operational
  // stream hyphenates (secret.rotate-failed); the household stream's
  // engine does not (automation.action_failed, escalation_failed). With
  // only the hyphen, a routine whose action failed rendered grey — the
  // one row on the screen that had to be red.
  if (/^(failed|blocked|invalid)$/.test(verb) || /[-_]failed$/.test(verb)) return "bad";
  // Something is waiting on a person, or went a way they should know
  // about. A plan someone rejected is a decision, not a fault.
  if (/^(proposed|pending-approval|skipped-offline|superseded|rejected|foreign|exempted|oob-reverted)$/.test(verb))
    return "warn";
  // Something finished, and finished properly.
  if (/^(applied|approved|repaired|completed|started|minted|rotated|bound|adopted|created|oob-accepted)$/.test(verb))
    return "ok";
  // The household stream's verbs (ADR-W024). A rule that acted, a
  // question that got an answer, a meal that got written down.
  if (/^(acted|answered|logged|escalated)$/.test(verb)) return "ok";
  // Waiting on a person, or a rule that could not decide.
  if (/^(escalation_pending|unsure)$/.test(verb)) return "warn";
  // `suppressed` deliberately falls through to mute: a rule deciding
  // there is nothing to do is the common case, not an event.
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
