import { CalendarCheck, Gauge, Question } from "@phosphor-icons/react";
import { api, CommitmentStanding, MeterView } from "../api";
import { ago, ErrorLine, useAction, useLoad, when } from "../lib";
import { Kind, StatusPill } from "../ui/status";
import { ProblemCard } from "../ui/ProblemCard";

// Commitments (ADR-W035 K6): what the household owes itself, from the
// operator's side.
//
// The household's own page shows the three things waiting on somebody
// today. This shows all of them, including the personal ones that page
// keeps to their owner, because the Console is the control plane rather
// than a bigger Homepage.
//
// The distinction this screen must not blur is the same one the engine
// and the family page both hold: a commitment nobody can evaluate and a
// commitment that is fine are different answers. "Can't tell" gets its
// own pill and its own sentence, never a quiet green.

export function Commitments() {
  const standings = useLoad(() => api.commitments(), []);
  const meters = useLoad(() => api.meters(), []);
  if (standings.error) return <ErrorLine error={standings.error} />;
  return (
    <CommitmentsView
      standings={standings.data ?? []}
      meters={meters.data ?? []}
      onChanged={() => {
        standings.reload();
        meters.reload();
      }}
      loading={standings.loading}
    />
  );
}

// unresolved is not a state the engine returns; it is a state the engine
// declines to reach. A rule it could not conclude from leaves the
// occurrence pending with the meter named, and reporting that as pending
// is how a screen tells a household everything is fine when nobody knows.
export function isUnresolved(s: CommitmentStanding): boolean {
  return (s.occurrence.unresolved?.length ?? 0) > 0 && !s.occurrence.trigger;
}

export function standingLabel(s: CommitmentStanding): string {
  if (s.disabled) return "paused";
  if (isUnresolved(s)) return "can't tell";
  return s.occurrence.state.replace("_", " ");
}

export function standingKind(s: CommitmentStanding): Kind {
  if (s.disabled) return "mute";
  if (isUnresolved(s)) return "warn";
  switch (s.occurrence.state) {
    case "overdue":
      return "bad";
    case "due_soon":
      return "warn";
    case "scheduled":
      return "ok";
    default:
      return "mute";
  }
}

// What needs attention first. Overdue, then can't-tell, then due soon —
// and a commitment nobody can evaluate outranks one that is merely
// approaching, because the second is progressing and the first is stuck.
const RANK: Record<string, number> = { bad: 0, warn: 1, ok: 2, mute: 3 };

export function byUrgency(list: CommitmentStanding[]): CommitmentStanding[] {
  return [...list].sort(
    (a, b) => RANK[standingKind(a)] - RANK[standingKind(b)] || a.name.localeCompare(b.name),
  );
}

export function CommitmentsView({
  standings,
  meters,
  onChanged,
  loading,
}: {
  standings: CommitmentStanding[];
  meters: MeterView[];
  onChanged: () => void;
  loading?: boolean;
}) {
  const sorted = byUrgency(standings);
  return (
    <>
      <div className="sub">
        Everything the household owes itself, and the numbers those are measured against. The
        engine works this out once a day and again whenever a meter is read; nothing here is
        stored, so what you see is recomputed from the readings and what has been done.
      </div>
      {sorted.length === 0 && !loading && (
        <div className="muted">
          Nothing is being tracked yet. A commitment is a resource like any other — propose one
          and approve it, and it appears here with its first cycle open.
        </div>
      )}
      <div className="bands">
        {sorted.map((s) => (
          <CommitmentCard key={s.name} standing={s} />
        ))}
      </div>
      {meters.length > 0 && <Meters meters={meters} onRead={onChanged} />}
    </>
  );
}

export function CommitmentCard({ standing }: { standing: CommitmentStanding }) {
  const kind = standingKind(standing);
  const occ = standing.occurrence;
  return (
    <ProblemCard
      severity={kind}
      icon={
        isUnresolved(standing) ? (
          <Question size={16} weight="fill" color={`var(--st-warn)`} />
        ) : (
          <CalendarCheck size={16} weight="fill" color={`var(--st-${kind === "mute" ? "warn" : kind})`} />
        )
      }
      title={standing.description || standing.name}
      meta={
        <>
          <StatusPill kind={kind}>{standingLabel(standing)}</StatusPill>
          <span className="muted">
            {standing.name}
            {standing.owner ? ` · ${standing.owner}` : ""}
            {standing.scope === "personal" ? " · personal" : ""}
          </span>
          {standing.booking && (
            <span className="muted" title={when(standing.booking.at)}>
              booked for {when(standing.booking.at)}
            </span>
          )}
        </>
      }
      body={
        <>
          <div>{occ.reason}</div>
          {occ.projection && (
            // Labelled, always. A projection is arithmetic about a truck
            // nobody has looked at, and a date presented without that
            // word is a date somebody will plan around.
            <div className="muted">
              Projected {when(occ.projection.at)} — an estimate from{" "}
              {occ.projection.source === "estimate" ? "the declared rate" : "how fast it has moved"},
              not a reading.
            </div>
          )}
          {occ.unresolved && occ.unresolved.length > 0 && (
            <div className="muted">Needs a reading: {occ.unresolved.join(", ")}</div>
          )}
        </>
      }
    >
      <Thresholds standing={standing} />
    </ProblemCard>
  );
}

// The arithmetic, for somebody asking why the engine thinks that. Every
// threshold, whether or not it is the one that decided.
export function Thresholds({ standing }: { standing: CommitmentStanding }) {
  return (
    <table className="table">
      <thead>
        <tr>
          <th>meter</th>
          <th>now</th>
          <th>due at</th>
          <th>remaining</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {standing.occurrence.rules.map((r) => (
          <tr key={r.meter}>
            <td>
              {r.meter}
              {r.meter === standing.occurrence.trigger ? " ·" : ""}
            </td>
            <td>{r.known ? (r.calendar ? when(r.observedAt) : r.position) : "—"}</td>
            <td>{r.calendar ? when(r.dueTime) : r.dueAt}</td>
            <td>{r.known && !r.calendar ? r.remaining : ""}</td>
            <td className="muted">{r.unresolved ?? (r.stale ? "stale" : "")}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// The meters, and a box to type a number into. The shortest path from
// "the truck is due in 300 miles" to "here is what it says".
export function Meters({ meters, onRead }: { meters: MeterView[]; onRead: () => void }) {
  return (
    <>
      <h2>Meters</h2>
      <div className="sub">
        The numbers commitments are measured against. The calendar is built in; the rest are
        declared, and somebody has to read them.
      </div>
      <div className="bands">
        {meters.map((m) => (
          <MeterCard key={m.name} meter={m} onRead={onRead} />
        ))}
      </div>
    </>
  );
}

export function MeterCard({ meter, onRead }: { meter: MeterView; onRead: () => void }) {
  const record = useAction();
  // Three standings, and they must not look alike: never read, read too
  // long ago, and current. The first two are both "no usable number" and
  // mean different things to whoever has to fix it.
  const kind: Kind = meter.builtIn ? "mute" : !meter.latest ? "warn" : meter.stale ? "warn" : "ok";
  const standing = meter.builtIn
    ? "built in"
    : !meter.latest
      ? "never read"
      : meter.stale
        ? "too old to rely on"
        : "current";
  return (
    <ProblemCard
      severity={kind}
      icon={<Gauge size={16} weight="fill" color={`var(--st-${kind === "mute" ? "warn" : kind})`} />}
      title={meter.description || meter.name}
      meta={
        <>
          <StatusPill kind={kind}>{standing}</StatusPill>
          <span className="muted">
            {meter.name}
            {meter.unit ? ` · ${meter.unit}` : ""}
          </span>
          {meter.latest && (
            <span className="muted" title={when(meter.latest.at)}>
              {meter.latest.value} {meter.unit} · read {ago(meter.latest.at)}
            </span>
          )}
        </>
      }
      body={
        meter.builtIn
          ? "Every household has one. It is declared nowhere and takes no readings."
          : meter.latest
            ? `Last read by ${meter.latest.recordedBy}.`
            : "Nothing has been recorded against this yet, so nothing measured by it can say whether it is due."
      }
      actions={
        meter.builtIn ? null : (
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault();
              const input = (e.currentTarget.elements.namedItem("value") as HTMLInputElement) ?? null;
              const value = Number(input?.value);
              if (!input?.value || Number.isNaN(value)) return;
              record.run(() => api.recordReading(meter.name, value).then(onRead), "recorded");
              input.value = "";
            }}
          >
            <input
              name="value"
              type="number"
              step="any"
              min="0"
              className="input"
              placeholder={`what it reads now${meter.unit ? ` (${meter.unit})` : ""}`}
              aria-label={`Reading for ${meter.name}`}
            />
            <button className="btn btn-primary" disabled={record.state.phase === "busy"}>
              {record.state.phase === "busy" ? "Recording…" : "Record"}
            </button>
          </form>
        )
      }
    />
  );
}
