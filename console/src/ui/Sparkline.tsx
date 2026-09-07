import { Kind } from "./status";

// Bar heights carry the meaning as well as the colour, so the shape is
// readable without colour vision: a run of failures dips.
const BAR: Record<Kind, { h: number; c: string }> = {
  ok: { h: 13, c: "color-mix(in srgb, var(--mark) 62%, transparent)" },
  warn: { h: 12, c: "color-mix(in srgb, var(--st-warn) 75%, transparent)" },
  bad: { h: 4, c: "var(--st-bad)" },
  mute: { h: 4, c: "var(--hair)" },
};

// Health history is accumulated from the event stream, because there is
// no endpoint for it — so a fresh load has almost none, and four bars
// drawn from four seconds of watching would imply history we do not
// have. Below MIN_SAMPLES, derived history draws nothing.
//
// Runs the API actually recorded are different: two recorded backup
// runs ARE two runs, and hiding them would be its own kind of lie. Those
// callers pass min={1}.
export const MIN_SAMPLES = 4;

export function Sparkline({
  samples,
  height = 15,
  label,
  min = MIN_SAMPLES,
}: {
  samples: Kind[];
  height?: number;
  label?: string;
  min?: number;
}) {
  if (samples.length < min) return null;
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
