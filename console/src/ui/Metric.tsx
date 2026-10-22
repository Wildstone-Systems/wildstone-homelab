import { ReactNode } from "react";
import { Kind, KIND_COLOR } from "./status";

// One large numeral per fact. The label sits above it, the unit beside
// it, and anything else the caller wants goes underneath — the metric
// strip on Home is four of these in a row.
export function Metric({
  label,
  value,
  unit,
  kind = "mute",
  children,
}: {
  label: string;
  value: ReactNode;
  unit?: string;
  kind?: Kind;
  children?: ReactNode;
}) {
  return (
    <div className="cell">
      <div className="eyebrow" style={{ marginBottom: "var(--space-3)" }}>
        {label}
      </div>
      <div style={{ display: "flex", alignItems: "baseline", gap: 6 }}>
        <span className="num" style={kind === "mute" ? undefined : { color: KIND_COLOR[kind] }}>
          {value}
        </span>
        {unit && <span className="unit">{unit}</span>}
      </div>
      {children && (
        <div style={{ display: "flex", gap: "var(--space-2)", marginTop: "var(--space-3)", flexWrap: "wrap" }}>
          {children}
        </div>
      )}
    </div>
  );
}

// A fraction of something finite: a full volume, a run in progress.
// fraction is 0..1; pass undefined for "running, length unknown" and
// the bar goes indeterminate rather than lying about 0%.
export function FillBar({
  fraction,
  kind = "mute",
  caption,
}: {
  fraction?: number;
  kind?: Kind;
  caption?: ReactNode;
}) {
  const pct = fraction === undefined ? undefined : Math.max(0, Math.min(1, fraction)) * 100;
  return (
    <>
      <div
        className="bar"
        role="progressbar"
        aria-valuenow={pct === undefined ? undefined : Math.round(pct)}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <span
          style={{
            width: pct === undefined ? "35%" : `${pct}%`,
            background: KIND_COLOR[kind],
            animation: pct === undefined ? "indeterminate 1.6s ease-in-out infinite" : undefined,
          }}
        />
      </div>
      {caption && (
        <div style={{ fontSize: 11.5, color: "var(--muted)", marginTop: 6 }}>{caption}</div>
      )}
    </>
  );
}
