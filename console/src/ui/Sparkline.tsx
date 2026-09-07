import { Kind } from "./status";

// Bar heights carry the meaning as well as the colour, so the shape is
// readable without colour vision: a run of failures dips.
const BAR: Record<Kind, { h: number; c: string }> = {
  ok: { h: 13, c: "color-mix(in srgb, var(--mark) 62%, transparent)" },
  warn: { h: 12, c: "color-mix(in srgb, var(--st-warn) 75%, transparent)" },
  bad: { h: 4, c: "var(--st-bad)" },
  mute: { h: 4, c: "var(--hair)" },
};

// Until an observation-history endpoint exists these samples come from
// the event stream the console already polls, so a fresh load has very
// few. Render nothing below MIN rather than implying history we do not
// have — the caller shows the caption that says how far back it goes.
export const MIN_SAMPLES = 4;

export function Sparkline({
  samples,
  height = 15,
  label,
}: {
  samples: Kind[];
  height?: number;
  label?: string;
}) {
  if (samples.length < MIN_SAMPLES) return null;
  return (
    <div
      style={{ display: "flex", gap: 2, height, alignItems: "flex-end" }}
      title={label ?? `last ${samples.length} checks seen since this page loaded`}
      aria-hidden
    >
      {samples.map((s, i) => (
        <span
          key={i}
          style={{ flex: 1, borderRadius: 1, height: BAR[s].h, background: BAR[s].c }}
        />
      ))}
    </div>
  );
}
