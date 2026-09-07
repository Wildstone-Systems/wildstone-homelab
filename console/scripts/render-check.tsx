// A render check for the Console, without a browser.
//
// `tsc --noEmit` proves the types line up and says nothing about whether
// the page renders. This renders it with react-dom/server against the
// exact JSON GET /api/v1/packages serves, which is the shape that
// actually broke once: pkgspec.ParamDecl had yaml tags only, so the API
// served {"Name","Required","Default"} while the client read
// name/required/default, and the generated form rendered every field
// blank on a 200 response.
//
// useEffect does not run under renderToString, so the data-loading half
// is not covered here. The generated form is, and it is where the logic
// lives. Run with: npm run render-check
import { renderToString } from "react-dom/server";
import { createElement as h } from "react";
import { Services, InstallForm } from "../src/views/Services";
import { Butler, StatusCard, Answer, SURFACES } from "../src/views/Butler";
import { Chat, IssuedCode } from "../src/views/Chat";
import { Shell, TokenGate } from "../src/App";
import { ConsoleStateProvider } from "../src/state";
import { kindOf, StatusPill } from "../src/ui/status";
import { MIN_SAMPLES, Sparkline } from "../src/ui/Sparkline";
import { FillBar, Metric } from "../src/ui/Metric";
import { ProblemCard } from "../src/ui/ProblemCard";
import { ServiceTile } from "../src/ui/ServiceTile";
import { Rail } from "../src/ui/Rail";

// The shell reads the token from sessionStorage before it renders
// anything, and node has none. A two-line shim is cheaper than
// threading storage through the component.
const store = new Map<string, string>();
(globalThis as unknown as { sessionStorage: Storage }).sessionStorage = {
  getItem: (k: string) => store.get(k) ?? null,
  setItem: (k: string, v: string) => void store.set(k, v),
  removeItem: (k: string) => void store.delete(k),
  clear: () => store.clear(),
  key: () => null,
  length: 0,
} as Storage;

// Exactly what GET /api/v1/packages serves, copied from the live VM.
const pkg = {
  name: "documents-paperless", version: "1.0.0", capability: "documents",
  enabled: false,
  params: [
    { name: "public_domain", required: true },
    { name: "docs_subdomain", default: "paperless" },
    { name: "node", required: true },
    { name: "identity_node" },
    { name: "timezone", default: "UTC" },
  ],
};

// React SSR separates adjacent text nodes with <!-- --> and escapes
// apostrophes, so an assertion on rendered copy has to read the text
// the way a browser concatenates it, not the way the string arrives.
function text(html: string): string {
  return html.replace(/<!--.*?-->/g, "").replace(/&#x27;/g, "'").replace(/&quot;/g, '"');
}

let failures = 0;
function check(label: string, cond: boolean, detail = "") {
  if (!cond) { failures++; console.log("FAIL:", label, detail); }
  else console.log("ok:", label);
}

const form = renderToString(h(InstallForm, { pkg: pkg as never, onDone: () => {} }));
check("form renders without throwing", form.length > 0);
for (const p of pkg.params) check(`input for ${p.name}`, form.includes(p.name));
// React SSR separates adjacent text nodes with <!-- -->, so the marker
// is not literally adjacent in the HTML. Assert on the rendered label as
// a whole rather than on a string only a browser would concatenate.
const label = /<label[^>]*>(.*?)<\/label>/.exec(form)?.[1] ?? "";
check("required params marked", label.replace(/<!--.*?-->/g, "") === "public_domain *", JSON.stringify(label));
check("defaults shown as placeholders", form.includes('placeholder="paperless"'));
check("preview gated until required filled", form.includes("fill in public_domain, node"));

const list = renderToString(h(Services));
check("Services renders without throwing", list.length > 0);

// The Butler surface (ADR-W017): its status card has two shapes, and the
// unconfigured one is the one a fresh install sees. It has to say what to
// run, not just that something is missing.
const butler = renderToString(h(Butler));
check("Butler renders without throwing", butler.length > 0);

const off = renderToString(
  h(StatusCard, { status: { configured: false, reason: "no Butler provider configured" }, loading: false }),
);
check("unconfigured card names the reason", off.includes("no Butler provider configured"));
check("unconfigured card shows the ollama flags", off.includes("-butler-provider ollama"));
check("unconfigured card offers no key field", !off.includes('type="password"'));

const on = renderToString(
  h(StatusCard, {
    status: {
      configured: true, provider: "ollama", model: "llama3.1",
      endpoint: "http://localhost:11434", mode: "qa-only",
      tools: ["get_resource", "get_health"],
    },
    loading: false,
  }),
);
check("configured card shows the resolved backend", on.includes("llama3.1") && on.includes("11434"));
check("qa-only card explains the withheld tool", on.includes("propose_change"));

// A proposal is reported as awaiting approval — this screen never
// approves, and must not imply it did.
const answered = renderToString(
  h(Answer, { answer: { sessionId: "bs-1", mode: "full", finalText: "done", planIds: ["p-1"] } }),
);
check("answer points proposals at Plans", answered.includes("awaiting your approval"));
check("answer names the plan id", answered.includes("p-1"));

// The two surfaces (ADR-W026 A1). The screen defaults to the operator's
// and offers the household's beside it; the household blurb has to say
// what that butler CANNOT do, because "ask the butler" reading the same
// on both surfaces is how a person ends up asking the family butler to
// restart a container and believing it might.
check("butler screen offers both surfaces", butler.includes("Operator") && butler.includes("Household"));
check("butler screen defaults to the operator surface", butler.includes("waits for your approval"));
// The household blurb renders only once that surface is selected, so
// assert the copy contract itself rather than the default render.
check("household blurb names the ceiling", SURFACES.family.blurb.includes("no access to the home lab"));
check("household blurb does not promise proposals", !SURFACES.family.blurb.includes("propose changes —"));

// Chat bindings (ADR-W026 A4). The screen has to teach two things a
// person cannot recover from getting wrong: the code is shown once, and
// a binding is per-surface.
const chat = renderToString(h(Chat));
check("Chat renders without throwing", chat.length > 0);
check("chat screen says bindings are per-surface", chat.includes("per-surface"));
check("chat screen says chat cannot approve", chat.includes("approve"));

const code = renderToString(
  h(IssuedCode, {
    claim: {
      code: "K7QP-3XMR-9TWD", surface: "family", identity: "sam",
      expiresAt: "2026-09-06T18:00:00Z", note: "Send this code to the telegram bot.",
    },
  }),
);
check("issued code is shown", code.includes("K7QP-3XMR-9TWD"));
check("issued code warns it is shown once", code.includes("only time the code is shown"));

// ------------------------------------------------- shared components
// The redesign's vocabulary (ADR-W027). These are shared by every
// screen, so a change here is a change everywhere — which is the
// argument for checking them once, here, rather than per screen.

// One place decides what a wire value means. If a new state appears on
// the wire it lands in "mute" rather than being quietly coloured wrong.
const KINDS: [string, string][] = [
  ["applied", "ok"], ["succeeded", "ok"], ["connected", "ok"], ["healthy", "ok"],
  ["proposed", "warn"], ["running", "warn"], ["skipped", "warn"], ["medium", "warn"],
  ["failed", "bad"], ["rejected", "bad"], ["disconnected", "bad"], ["high", "bad"],
  ["something-new", "mute"], ["", "mute"],
];
for (const [value, want] of KINDS) {
  check(`kindOf(${value || "empty"}) is ${want}`, kindOf(value) === want, kindOf(value));
}
check("kindOf ignores case", kindOf("SUCCEEDED") === "ok");

const pill = renderToString(h(StatusPill, { kind: "warn", children: "proposed" }));
check("pill carries its kind as a class", pill.includes("st st-warn"), pill);

// A sparkline drawn from three samples would imply history the console
// does not have. It draws nothing until it has MIN_SAMPLES.
const thin = renderToString(h(Sparkline, { samples: Array(MIN_SAMPLES - 1).fill("ok") }));
check("sparkline is silent below the minimum", thin === "", thin);
const spark = renderToString(h(Sparkline, { samples: ["ok", "ok", "bad", "ok", "ok"] }));
check("sparkline draws one bar per sample", (spark.match(/<span/g) ?? []).length === 5, spark);
check("sparkline says how far back it goes", spark.includes("since this page loaded"));

const metric = renderToString(h(Metric, { label: "Unhealthy", value: 1, unit: "of 12", kind: "bad" }));
check("metric shows label, numeral and unit", metric.includes("Unhealthy") && metric.includes("of 12"));
check("metric numeral takes the status colour", metric.includes("--st-bad"), metric);

// A run whose length is unknown gets an indeterminate bar, not a 0%
// one: "nothing has happened yet" and "I don't know" are different.
const known = renderToString(h(FillBar, { fraction: 0.5, caption: "24 of 48 GB" }));
check("fill bar reports its fraction", known.includes('aria-valuenow="50"'), known);
check("fill bar shows its caption", known.includes("24 of 48 GB"));
const unknown = renderToString(h(FillBar, {}));
check("indeterminate bar claims no value", !unknown.includes("aria-valuenow"), unknown);

const problem = renderToString(
  h(ProblemCard, {
    severity: "bad",
    title: "jellyfin can't transcode",
    body: "Cache volume full.",
    meta: h(StatusPill, { kind: "warn", children: "plan-0f3a91" }),
    aside: h(FillBar, { fraction: 1, kind: "bad", caption: "100% of 48 GB" }),
    actions: h("button", { className: "btn btn-primary" }, "Review plan"),
  }),
);
check("problem card states the problem", text(problem).includes("jellyfin can't transcode"), problem);
check("problem card carries its severity rule", problem.includes("--st-bad"), problem);
check("problem card keeps the action on the card", problem.includes("Review plan"));
check("problem card renders its aside", problem.includes("100% of 48 GB"));

const tile = renderToString(
  h(ServiceTile, {
    name: "jellyfin", what: "Movies and TV", host: "jellyfin.home.arpa",
    health: "bad", state: "health 500", resourceCount: 5,
    launchHref: "https://jellyfin.home.arpa",
  }),
);
check("tile names the service and what it does", tile.includes("jellyfin") && tile.includes("Movies and TV"));
check("tile pluralises its resource count", text(tile).includes("5 resources"), tile);
const oneTile = renderToString(
  h(ServiceTile, { name: "lldap", health: "ok", state: "healthy", resourceCount: 1 }),
);
check("tile says 1 resource, not 1 resources", text(oneTile).includes("1 resource<"), oneTile);
check("tile without a link offers no launch arrow", !oneTile.includes("launch"), oneTile);

// Icons carry the nav, so every item must still have a name a screen
// reader and a tooltip can use.
const rail = renderToString(
  h(Rail, {
    items: [
      { id: "home", label: "Home", icon: null, pip: "bad" },
      { id: "secrets", label: "Secrets", icon: null, dividerBefore: true },
    ],
    current: "home",
    onSelect: () => {},
  }),
);
// Two items plus the nav's own label.
check("every rail item is named", (rail.match(/aria-label="/g) ?? []).length === 3, rail);
check("rail marks the current screen", rail.includes('aria-current="page"'));
check("rail pip takes the status colour", rail.includes("--st-bad"), rail);
check("rail draws the divider it was asked for", rail.includes("divider"));

// ------------------------------------------------------ shell states
// Unauthorised is the state a fresh install sees first, and the one
// screen that has to work before anything else does.
const gate = renderToString(h(TokenGate, { onDone: () => {} }));
check("gate asks for a token", gate.includes("Paste an API token"));
check("gate names the tool that mints more", gate.includes("wild"));
check("gate labels its input", gate.includes('aria-label="API token"'), gate);

// Loading: useEffect does not run under renderToString, so this is the
// shell with no snapshot yet — the state every screen starts in.
const shell = renderToString(h(ConsoleStateProvider, { children: h(Shell) }));
check("shell renders with no data yet", shell.length > 0);
check("shell shows the icon rail", shell.includes('aria-label="Console sections"'), shell);
for (const label of ["Home", "Services", "Butler", "Chat", "Plans", "Activity", "Backups", "Nodes", "Secrets", "Identity"]) {
  check(`rail offers ${label}`, shell.includes(`aria-label="${label}"`));
}
check("shell names the screen you are on", shell.includes("Household"), shell);
check("empty shell claims no freshness it cannot prove", !shell.includes(">live<"), shell);

console.log(failures === 0 ? "\nALL CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
