import { ReactNode } from "react";
import { modelLabel, type ButlerRow, type Model } from "../api";
import { when } from "../lib";

// The status vocabulary. Four kinds, and ONE place that decides what a
// wire value means — every screen asks here rather than growing its own
// opinion of what "skipped" looks like.
export type Kind = "ok" | "warn" | "bad" | "mute";

export function kindOf(value: string | undefined): Kind {
  switch ((value ?? "").toLowerCase()) {
    case "ok":
    case "low":
    case "applied":
    case "succeeded":
    case "connected":
    case "enabled":
    case "healthy":
      return "ok";
    case "medium":
    case "proposed":
    case "running":
    case "skipped":
    case "waiting":
      return "warn";
    case "high":
    case "failed":
    case "rejected":
    case "bad":
    case "disconnected":
    case "blocked":
    case "revoked":
      return "bad";
    default:
      return "mute";
  }
}

// The pill is the only place the status colours are used as a fill.
// Everywhere else they are ink or a 3px rule, so a screen full of
// amber never reads as a screen on fire.
export function StatusPill({
  kind,
  icon,
  title,
  children,
}: {
  kind: Kind;
  icon?: ReactNode;
  title?: string;
  children: ReactNode;
}) {
  return (
    <span className={`st st-${kind}`} title={title}>
      {icon}
      {children}
    </span>
  );
}

export const KIND_COLOR: Record<Kind, string> = {
  ok: "var(--st-ok)",
  warn: "var(--st-warn)",
  bad: "var(--st-bad)",
  mute: "var(--muted)",
};

// A live dot, optionally breathing. Decorative: the pill next to it
// carries the words.
export function Dot({ kind, pulse }: { kind: Kind; pulse?: boolean }) {
  return (
    <span
      className={pulse ? "dot pulse" : "dot"}
      style={{ background: KIND_COLOR[kind], color: KIND_COLOR[kind] }}
      aria-hidden
    />
  );
}

// Readiness, in words, for exactly one Butler.
//
// It lives beside kindOf for the same reason kindOf does: this is the
// one place a wire value becomes a sentence, and a second screen that
// grew its own opinion would eventually call the same Butler "ready"
// in the roster and "questions only" in the drawer.
//
// The order of the checks is the meaning. Retired outranks everything,
// because a retired Butler is not unhealthy — it is not answering, on
// purpose. Then unreachable, then the tool-probe verdict, and only what
// survives all three is ready.
export function healthOf(
  row: ButlerRow,
  model?: Model,
): { kind: Kind; label: string; detail: string; long: string } {
  const b = row.butler;
  // The failure detail belongs to the MODEL, not the Butler: three
  // Butlers on one unreachable model all fail for the same reason, and
  // it is the model that knows what the provider said.
  const why = model?.readinessDetail;
  if (b.retiredAt) {
    const on = when(b.retiredAt);
    return { kind: "mute", label: `retired ${on}`, detail: "kept, not deleted", long: `retired ${on}` };
  }
  if (!row.reachable) {
    const detail = !b.modelEnabled
      ? `${modelLabel(b)} is turned off in Model routes`
      : why || "its model is not answering";
    return { kind: "bad", label: "unavailable", detail, long: `unavailable — ${detail}` };
  }
  if (!row.carriesActions) {
    const detail = why || "failed tool check";
    return { kind: "warn", label: "questions only", detail, long: `questions only — ${detail}` };
  }
  return {
    kind: "ok",
    label: "ready",
    detail: `${modelLabel(b)} · ${b.provider ?? "?"}`,
    long: "ready · tool check passed",
  };
}
