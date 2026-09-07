import { ReactNode } from "react";

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
