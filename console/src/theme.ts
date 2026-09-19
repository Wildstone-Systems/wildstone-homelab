// Wearing the theme this person chose.
//
// THE DERIVATION IS NOT HERE, AND THAT IS THE POINT. Keystone derives
// the token block — grounds, inks, status tints, shadow rings — and
// enforces the same contrast floors that scripts/contrast-check.mjs
// holds the shipped sheet to. A second implementation of those rules in
// TypeScript would be a second place for them to be wrong, and the one
// that shipped in the browser would be the one nobody checked. So this
// file fetches a map of custom properties and sets them. It knows
// nothing about colour.
//
// WHY INLINE PROPERTIES RATHER THAN A STYLESHEET. The Console's static
// files are served before anybody has authenticated — the bundle is
// handed out to a browser that has not yet sent its bearer token — so
// there is no request at which a per-person stylesheet could be
// injected, the way the household portal's pages are. Setting the
// properties on the root element instead sidesteps specificity
// entirely: an inline custom property beats every rule in styles.css,
// including the two theme blocks, with no selector to match.
//
// WHY A CACHE. The fetch that says which theme somebody is on cannot
// happen before the first paint, so without one the Console would flash
// its own palette on every load for anybody who chose another. The
// cached block is applied synchronously at start and then corrected by
// the answer, which for the overwhelmingly common case — same person,
// same browser, same theme — corrects it to itself.

import type { Appearance } from "./api";

const CACHE = "console.appearance";

// What a cache entry holds: enough to paint, and nothing else. The
// theme list is deliberately not cached — it is picker data, it changes
// when an Owner adds a theme, and a stale copy of it would be a list
// somebody cannot choose from.
interface Cached {
  theme: string;
  tokens: Record<string, string>;
  dark: boolean;
}

// Tokens this file has set, so that switching to a theme which defines
// fewer of them removes the ones the last theme left behind. Without
// it, moving from a theme to the default would leave the old grounds on
// the root element with nothing to clear them.
let applied: string[] = [];

function read(): Cached | null {
  try {
    const raw = localStorage.getItem(CACHE);
    if (!raw) return null;
    const v = JSON.parse(raw) as Cached;
    // A shape check rather than trust: this is storage a person's own
    // browser holds, it survives upgrades, and a malformed entry must
    // not be able to stop the Console painting.
    if (!v || typeof v.theme !== "string" || typeof v.tokens !== "object" || v.tokens === null) {
      return null;
    }
    return { theme: v.theme, tokens: v.tokens, dark: v.dark !== false };
  } catch {
    // Private browsing, cleared site data, a quota — none of which is a
    // reason not to render.
    return null;
  }
}

function write(v: Cached | null) {
  try {
    if (v === null) localStorage.removeItem(CACHE);
    else localStorage.setItem(CACHE, JSON.stringify(v));
  } catch {
    // The cache is an optimisation. Losing it costs one flash.
  }
}

// apply puts a token block on the root element, clearing whatever the
// last one set.
//
// data-theme is set alongside, because the sheets still use it: a rule
// that keys off the attribute — and the prefers-color-scheme block that
// excludes it — must agree with the ground the tokens actually paint,
// or a light theme gets the dark sheet's assumptions underneath it.
export function apply(tokens: Record<string, string> | undefined, dark: boolean) {
  const root = document.documentElement;
  for (const name of applied) {
    if (!tokens || !(name in tokens)) root.style.removeProperty(name);
  }
  applied = [];
  if (tokens) {
    for (const [name, value] of Object.entries(tokens)) {
      // color-scheme is a property, not a custom property: it tells the
      // browser which form controls and scrollbars to draw, and a theme
      // that did not set it would get the sheet's idea of the ground
      // under its own.
      root.style.setProperty(name, value);
      applied.push(name);
    }
    root.dataset.theme = dark ? "dark" : "light";
    return;
  }
  // No theme: hand the sheets back their own two blocks, and let the
  // OS decide between them as it did before any of this existed.
  delete root.dataset.theme;
}

// start applies the cached block before React mounts. Called from
// main.tsx, synchronously, so the first paint is already themed.
export function start() {
  const cached = read();
  if (cached) apply(cached.tokens, cached.dark);
}

// adopt applies a fresh answer from the server and caches it.
export function adopt(a: Appearance) {
  apply(a.tokens, a.dark);
  if (a.tokens && a.theme) write({ theme: a.theme, tokens: a.tokens, dark: a.dark });
  else write(null);
}
