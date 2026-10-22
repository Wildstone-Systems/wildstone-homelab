// Contrast check for the console's token set (ADR-W027).
//
// The design review found that muted text below a 58% mix of the ink
// fails 4.5:1 on both grounds. That is a fact about the tokens, not
// about any one screen, so it is checked once here rather than argued
// again in every PR that adds a label.
//
// Reads the literal values straight out of src/styles.css, so a token
// edited without its partner is caught by the check rather than by an
// operator squinting at a status pill.
import { readFileSync } from "fs";

const CSS = readFileSync(new URL("../src/styles.css", import.meta.url), "utf8");
const AA = 4.5;
const AA_LARGE = 3; // >=18.66px bold or >=24px: the metric numerals

function block(selector) {
  // Match the LAST brace-balanced declaration block opened by this
  // selector, so :root and its two theme overrides are read apart.
  const at = CSS.indexOf(selector);
  if (at === -1) throw new Error(`no ${selector} block in styles.css`);
  const open = CSS.indexOf("{", at);
  let depth = 0;
  for (let i = open; i < CSS.length; i++) {
    if (CSS[i] === "{") depth++;
    else if (CSS[i] === "}" && --depth === 0) return CSS.slice(open + 1, i);
  }
  throw new Error(`unbalanced ${selector} block`);
}

function tokens(text) {
  const out = {};
  for (const [, k, v] of text.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;]+);/gi)) out[k] = v.trim();
  return out;
}

const hex = (h) => {
  const s = h.trim().replace("#", "");
  const n = s.length === 3 ? s.split("").map((c) => c + c).join("") : s;
  return [0, 2, 4].map((i) => parseInt(n.slice(i, i + 2), 16));
};

// color-mix(in srgb, C p%, transparent) drawn over a ground is plain
// source-over compositing: p of the colour, 1-p of what is behind it.
const mix = (fg, pct, ground) => fg.map((c, i) => c * pct + ground[i] * (1 - pct));

const lum = (rgb) =>
  rgb
    .map((c) => {
      const s = c / 255;
      return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    })
    .reduce((a, c, i) => a + c * [0.2126, 0.7152, 0.0722][i], 0);

const ratio = (a, b) => {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
};

const dark = tokens(block(":root {"));
const light = { ...dark, ...tokens(block(':root[data-theme="light"]')) };

// The muted floor, per theme: the mix is read out of the token rather
// than assumed, so lowering it to save a shade fails here first.
function pct(value, theme, name) {
  const m = /var\(--color-text\)\s*(\d+)%/.exec(value ?? "");
  if (!m) {
    console.log(`FAIL: ${theme} ${name} is not a percentage mix of --color-text`);
    process.exit(1);
  }
  return Number(m[1]) / 100;
}
const mutedMix = (t, theme) => pct(t["--muted"], theme, "--muted");

let failures = 0;
function check(theme, label, fg, bg, floor = AA) {
  const r = ratio(fg, bg);
  const ok = r >= floor;
  if (!ok) failures++;
  console.log(
    `${ok ? "ok:  " : "FAIL:"} ${theme.padEnd(5)} ${label.padEnd(34)} ${r.toFixed(2)}:1 (needs ${floor})`,
  );
}

for (const [theme, t] of [["dark", dark], ["light", light]]) {
  const bg = hex(t["--color-bg"]);
  const surface = hex(t["--color-surface"]);
  const ink = hex(t["--color-text"]);

  for (const [gname, ground] of [["bg", bg], ["surface", surface]]) {
    check(theme, `text on ${gname}`, ink, ground);
    const mutedPct = mutedMix(t, theme);
    check(theme, `muted (${Math.round(mutedPct * 100)}%) on ${gname}`, mix(ink, mutedPct, ground), ground);
    // The accent carries links and the primary button's label.
    check(theme, `accent on ${gname}`, hex(t["--color-accent"]), ground);
    // Rail glyphs are 1.5px strokes: they sit above the text floor in
    // both themes rather than at it.
    const iconPct = pct(t["--icon"], theme, "--icon");
    check(theme, `icon (${Math.round(iconPct * 100)}%) on ${gname}`, mix(ink, iconPct, ground), ground);
  }

  // Status as ink on the ground: the metric numerals and the problem
  // card's aside figure. Those are >=22px semibold, so large-text AA.
  for (const s of ["ok", "warn", "bad"]) {
    check(theme, `st-${s} numeral on bg`, hex(t[`--st-${s}`]), bg, AA_LARGE);
    // Status as pill text on its own fill: 11px, so full AA.
    check(theme, `st-${s} on st-${s}-bg`, hex(t[`--st-${s}`]), hex(t[`--st-${s}-bg`]));
  }

  // The sparkline's ok bar is a 62% mix of the accent tint on the
  // surface it sits on. It is a mark, not text, so 3:1 is the bar.
  check(theme, "sparkline ok bar on surface", mix(hex(t["--mark"]), 0.62, surface), surface, AA_LARGE);

  // Three states the Routines and Activity work introduced.
  //
  // An unmapped subject is st-warn as ordinary 13px table text on the
  // page ground, not pill text on a tint — so it answers to full AA
  // rather than to the pill floor above. It is the cell that explains
  // why a routine never fired; it has to be readable.
  check(theme, "unmapped subject on bg", hex(t["--st-warn"]), bg);

  // A correlated row on the household stream fills with --faint, and
  // the text over it was designed against the plain ground. Both the
  // ink and the muted text have to survive the tint.
  const faintPct = pct(t["--faint"], theme, "--faint");
  const correlatedGround = mix(ink, faintPct, bg);
  check(theme, "text on a correlated row", ink, correlatedGround);
  check(theme, "muted text on a correlated row", mix(ink, mutedMix(t, theme), correlatedGround), correlatedGround);

  // A .mono chip sits on --faint over the surface: the trigger and the
  // tier on a routine card, read character by character.
  const chipGround = mix(ink, faintPct, surface);
  check(theme, "chip text on its faint fill", ink, chipGround);
}

console.log(failures === 0 ? "\nALL CONTRAST CHECKS PASSED" : `\n${failures} CONTRAST FAILURE(S)`);
process.exit(failures === 0 ? 0 : 1);
