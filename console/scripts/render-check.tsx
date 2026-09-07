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
import { Freshness, Shell, TokenGate } from "../src/App";
import { HomeView, buildProblems } from "../src/views/Home";
import { ServicesView, InstalledTile, AvailableRow } from "../src/views/Services";
import { groupServices, upstreamContainer } from "../src/services";
import { eventKind } from "../src/ui/HourChart";
import type { ConsoleSnapshot } from "../src/state";
import type { BackupRun, EventRow, Observed, PackageView, PlanRecord, ResourceView } from "../src/api";
import { PlansView, PendingPlan, PlanSteps } from "../src/views/Plans";
import { ActivityView } from "../src/views/Activity";
import { BackupsView, byPlan, explain } from "../src/views/Backups";
import { ConsoleStateContext, ConsoleStateProvider } from "../src/state";
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
check("derived history is silent below the minimum", thin === "", thin);
// Runs the API recorded are real history however few there are, and
// hiding them would be its own kind of lie.
const recorded = renderToString(h(Sparkline, { samples: ["ok", "bad"], min: 1 }));
check("recorded history draws however short it is", (recorded.match(/<span/g) ?? []).length === 2, recorded);
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
    service: {
      key: "Container/jellyfin", kind: "Container", name: "jellyfin", node: "n1",
      health: "bad", state: "unhealthy", detail: "health 500",
      url: "https://jellyfin.home.arpa", urlHealthy: false,
    },
  }),
);
check("tile names the service and where it answers", tile.includes("jellyfin.home.arpa"), tile);
check("tile offers the launch arrow when there is an address", tile.includes("launch"), tile);
// A container no Route points at is reachable only from inside the
// household. That is a fact about it, not a gap in what we know, so it
// says so rather than showing an empty line.
const oneTile = renderToString(
  h(ServiceTile, {
    service: { key: "Container/lldap", kind: "Container", name: "lldap", health: "ok", state: "healthy" },
  }),
);
check("unrouted tile says internal only", oneTile.includes("internal only"), oneTile);
check("tile without an address offers no launch arrow", !oneTile.includes("launch"), oneTile);

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

// The header's reconcile age comes from the newest reconcile event of
// ANY outcome. Matching one exact type is how it went blank the first
// time — Keystone emits reconcile.applied, never reconcile.tick.
const fresh = renderToString(
  h(ConsoleStateContext.Provider, {
    value: { error: null, snap: {
      observed: [], nodes: [], plans: [{ seq: 1, id: "p", createdAt: "", principal: "x", summary: "", risk: "low", state: "applied", basedOnHead: "5a01c4e9f0" }],
      events: [{ seq: 1, time: new Date(Date.now() - 120000).toISOString(), type: "reconcile.applied", node: "n1" }],
      pending: [], unhealthy: [], awaiting: [], history: new Map(), lastPollAt: new Date(),
    } },
  }, h(Freshness)),
);
check("the header dates the last reconcile pass", text(fresh).includes("reconciled 2m ago"), text(fresh));
check("the header shows the statefile head, shortened", text(fresh).includes("statefile 5a01c4"), text(fresh));
check("a healthy poll reads live", fresh.includes("live"));

// ---------------------------------------------- the service join
// Home and Services are drawn from three calls joined client-side. The
// join is the part with logic, and none of it is visible to tsc.

// A Route's spec is the one place the statefile records which container
// an address belongs to.
check("upstream names the container", upstreamContainer('spec:\n  upstream: "container/authelia:9091"\n') === "authelia");
check("upstream tolerates no quotes", upstreamContainer("  upstream: container/lldap:17170") === "lldap");
check("an ip upstream names no container", upstreamContainer("  upstream: 10.0.0.4:8080") === undefined);
check("no yaml, no container", upstreamContainer(undefined) === undefined);

const RESOURCES: ResourceView[] = [
  { kind: "Container", name: "jellyfin", owner: "capability/media", node: "n1", yaml: "" },
  { kind: "Volume", name: "jellyfin-cache", owner: "capability/media", node: "n1", yaml: "" },
  { kind: "Route", name: "media", owner: "capability/media", node: "n1",
    yaml: 'spec:\n  host: jellyfin.home.arpa\n  upstream: "container/jellyfin:8096"\n' },
  { kind: "Container", name: "authelia", owner: "capability/identity", node: "n1", yaml: "" },
  { kind: "Container", name: "lldap", owner: "capability/identity", node: "n1", yaml: "" },
  { kind: "Route", name: "auth", owner: "capability/identity", node: "n1",
    yaml: 'spec:\n  host: auth.home.arpa\n  upstream: "container/authelia:9091"\n' },
];
const PACKAGES: PackageView[] = [
  { name: "media-jellyfin", version: "1.2.0", installedVersion: "1.1.0", stale: true,
    capability: "media", enabled: true,
    addresses: [{ route: "media", url: "https://jellyfin.home.arpa", healthy: false, detail: "health 500" }] },
  { name: "identity", version: "1.9.3", capability: "identity", enabled: true,
    addresses: [{ route: "auth", url: "https://auth.home.arpa", healthy: true }] },
  { name: "photos-immich", version: "1.4.0", capability: "photos", enabled: false,
    params: [{ name: "public_domain", required: true }, { name: "node", required: true }, { name: "timezone", default: "UTC" }] },
  { name: "home-automation", version: "0.7.1", capability: "automation", enabled: false,
    blockers: ["needs a Zigbee coordinator; none was probed"] },
];
const SICK: Observed[] = [
  { kind: "Container", name: "jellyfin", node: "n1", healthy: false, detail: "health 500 — transcode cache full", updatedAt: "2026-09-07T00:00:00Z" },
  { kind: "Container", name: "authelia", node: "n1", healthy: true, updatedAt: "2026-09-07T00:00:00Z" },
  { kind: "Container", name: "lldap", node: "n1", healthy: true, updatedAt: "2026-09-07T00:00:00Z" },
];

const groups = groupServices(PACKAGES, RESOURCES, SICK);
check("a group is an owner, named without its prefix", groups.map((g) => g.label).join(",") === "media,identity", JSON.stringify(groups.map((g) => g.label)));
check("unhealthy groups sort first", groups[0].label === "media");
check("only containers become tiles", groups[0].services.length === 1 && groups[0].services[0].name === "jellyfin");
check("a group counts all its resources, not just its tiles", groups[0].resourceCount === 3);
check("the route's upstream gives the tile its address", groups[0].services[0].url === "https://jellyfin.home.arpa");
check("a container no route points at has none", groups[1].services.find((s) => s.name === "lldap")?.url === undefined);
check("the group reports the installed version, not the catalog's", groups[0].version === "1.1.0");
check("a newer catalog version is flagged", groups[0].stale === true && groups[0].catalogVersion === "1.2.0");

// F75: a resource held back by a plan awaiting a human is amber. It is
// not a fault, and colouring it red is how a restart made a working
// homelab look like it had fallen over.
const waiting = groupServices(
  PACKAGES, RESOURCES,
  [{ kind: "Container", name: "jellyfin", healthy: false, awaitingApproval: true, updatedAt: "2026-09-07T00:00:00Z" }],
);
check("awaiting approval is amber, not red", waiting[0].services[0].health === "warn", waiting[0].services[0].health);
check("awaiting approval says what it is waiting for", waiting[0].services[0].state === "waiting on a plan");
// A container nothing has reported on is not a healthy one.
const unobserved = groupServices(PACKAGES, RESOURCES, []);
check("an unobserved container is muted, not green", unobserved[0].services[0].health === "mute");

// Event types are classified by shape, so a type added to Keystone
// tomorrow is drawn as ordinary traffic rather than as a fault.
for (const [type, want] of [
  ["reconcile.failed", "bad"], ["reconcile.blocked", "bad"], ["statefile.invalid", "bad"],
  ["secret.rotate-failed", "bad"], ["publisher.mint-failed", "bad"],
  ["plan.proposed", "warn"], ["reconcile.skipped-offline", "warn"], ["drift.pending-approval", "warn"],
  ["drift.superseded", "warn"], ["policy.exempted", "warn"],
  // A plan someone rejected is a decision, not a fault.
  ["plan.rejected", "warn"], ["policy.rejected", "warn"],
  ["reconcile.applied", "ok"], ["backup.completed", "ok"], ["drift.repaired", "ok"],
  ["secret.rotated", "ok"], ["chat.bound", "ok"], ["token.minted", "ok"],
  // An event type nobody here has an opinion about is ordinary
  // traffic. Keystone gaining one must not paint the chart red.
  ["butler.session", "mute"], ["report.morning", "mute"], ["something.entirely.new", "mute"],
] as [string, string][]) {
  check(`eventKind(${type}) is ${want}`, eventKind(type) === want, eventKind(type));
}

// ------------------------------------------------------ Home, both states
function snapshot(over: Partial<ConsoleSnapshot> = {}): ConsoleSnapshot {
  return {
    observed: SICK, nodes: [], events: [], plans: [], pending: [],
    unhealthy: SICK.filter((o) => !o.healthy), awaiting: [],
    history: new Map(), lastPollAt: new Date("2026-09-07T00:05:00Z"),
    ...over,
  };
}

const bad = renderToString(h(HomeView, {
  snap: snapshot({
    nodes: [{ id: "n1", connected: true, firstJoinedAt: "", lastSeenAt: "" },
            { id: "n2", connected: false, firstJoinedAt: "", lastSeenAt: "2026-09-07T00:00:00Z" }],
    pending: [{ seq: 1, id: "plan-0f3a91", createdAt: "2026-09-07T00:01:00Z", principal: "butler",
                summary: "prune the jellyfin transcode cache", risk: "medium", state: "proposed",
                plan: { steps: [{ kind: "Container", name: "jellyfin", action: "restart" }] } }],
  }),
  pkgs: PACKAGES, resources: RESOURCES, onNavigate: () => {},
}));
check("bad day names the unhealthy service", bad.includes("jellyfin is unhealthy"), bad.slice(0, 400));
check("bad day says why, in the reconciler's words", bad.includes("transcode cache full"));
check("bad day reports the offline node", bad.includes("n2 is not connected"));
check("bad day counts unhealthy against the total", text(bad).includes("unhealthy of 6"), text(bad).slice(0, 900));

// A plan that already explains an unhealthy resource is folded into
// that resource's card. The operator has one problem, not two.
const folded = buildProblems(snapshot({
  pending: [{ seq: 1, id: "plan-0f3a91", createdAt: "", principal: "butler", summary: "prune the cache",
              risk: "medium", state: "proposed",
              plan: { steps: [{ kind: "Container", name: "jellyfin" }] } }],
}));
check("a plan explaining a fault is folded into it", folded.length === 1, JSON.stringify(folded.map((p) => p.id)));
check("the folded card offers the plan", folded[0].cta === "Review plan" && folded[0].plan?.id === "plan-0f3a91");
// One that explains nothing on the board still needs a decision.
const standalone = buildProblems(snapshot({
  observed: [], unhealthy: [],
  pending: [{ seq: 1, id: "plan-77", createdAt: "", principal: "mika", summary: "install photos", risk: "low", state: "proposed" }],
}));
check("a plan explaining nothing gets its own card", standalone.length === 1 && standalone[0].id === "plan:plan-77");
check("with no fault to fix, the fault is not invented", !standalone.some((p) => p.severity === "bad"));

// The good day is the state users see most and the old console handled
// least: no banners, nothing waiting.
const well = renderToString(h(HomeView, {
  snap: snapshot({ observed: SICK.map((o) => ({ ...o, healthy: true, detail: undefined })), unhealthy: [],
                   nodes: [{ id: "n1", connected: true, firstJoinedAt: "", lastSeenAt: "" }] }),
  pkgs: PACKAGES, resources: RESOURCES, onNavigate: () => {},
}));
check("the good day says so plainly", well.includes("Everything converged"));
check("the good day raises no problem card", !well.includes("is unhealthy"), well.slice(0, 300));
check("the good day still shows the services", well.includes("jellyfin") && well.includes("lldap"));

// An empty statefile is a fresh install, not an error.
const empty = renderToString(h(HomeView, {
  snap: snapshot({ observed: [], unhealthy: [] }), pkgs: [], resources: [], onNavigate: () => {},
}));
check("an empty statefile points at Services", empty.includes("enable a capability"), empty.slice(0, 300));

// ------------------------------------------------------------ Services
const catalog = renderToString(h(ServicesView, { pkgs: PACKAGES, onInstalled: () => {} }));
check("catalog separates installed from available", catalog.includes("Installed") && catalog.includes("Available"));
check("an installed capability shows where it answers", catalog.includes("https://auth.home.arpa"));
check("an address that does not answer says so", catalog.includes("not answering"));
check("a stale install offers the newer version", text(catalog).includes("update to 1.2.0"), text(catalog).slice(0, 600));
// What stands in the way is stated before Install is pressed, not
// after a plan is approved.
check("a blocker is stated on the row", catalog.includes("Zigbee coordinator"));
check("a blocked package cannot be installed", /Zigbee[\s\S]*?<\/tr>/.test(catalog) && catalog.includes("disabled"), "");
check("required params are named before the form opens", text(catalog).includes("public_domain, node"));

const blockedRow = renderToString(h("table", null, h("tbody", null, h(AvailableRow, { pkg: PACKAGES[3], onInstall: () => {} }))));
check("the blocked row's Install is disabled", blockedRow.includes("disabled"), blockedRow);
const openRow = renderToString(h("table", null, h("tbody", null, h(AvailableRow, { pkg: PACKAGES[2], onInstall: () => {} }))));
check("an unblocked row's Install is not", !openRow.includes("disabled"), openRow);

const noAddr = renderToString(h(InstalledTile, { pkg: { name: "monitoring", version: "0.4.0", capability: "monitoring", enabled: true }, onReconfigure: () => {} }));
check("an installed capability with no address says so", noAddr.includes("no address"), noAddr);

// ------------------------------------------------------------- Plans
// The one screen where a click changes the household. Everything
// approval needs has to be ON the card, and the result has to land
// there too (ADR-W027 A7).
const PENDING: PlanRecord = {
  seq: 9, id: "plan-0f3a91", createdAt: "2026-09-07T00:01:00Z", principal: "butler",
  actingFor: "mika", summary: "prune the jellyfin transcode cache", risk: "high",
  state: "proposed", basedOnHead: "5a01c4e9f0",
  plan: { steps: [
    { description: "stop the container", kind: "Container", name: "jellyfin", action: "stop", risk: "medium", blastRadius: "jellyfin is offline for about 20 seconds" },
    { description: "restore the volume", kind: "Volume", name: "jellyfin-cache", action: "restore", risk: "high", blastRadius: "replaces the cache volume's contents" },
  ] },
};
const RESOLVED: PlanRecord = {
  seq: 8, id: "plan-9b2d10", createdAt: "2026-09-06T22:00:00Z", principal: "mika",
  summary: "install monitoring", risk: "low", state: "applied",
  resolvedBy: "mika", resolvedAt: "2026-09-06T22:04:00Z",
};

const card = renderToString(h(PendingPlan, { plan: PENDING, onResolved: () => {} }));
check("the plan card carries the decision", card.includes("Approve") && card.includes("Reject"));
check("the plan card names who proposed it, for whom", text(card).includes("Proposed by butler acting for mika"), text(card).slice(0, 700));
check("the plan card names the statefile it was built against", text(card).includes("5a01c4e9"));
check("the plan card shows every step's blast radius", card.includes("offline for about 20 seconds") && card.includes("replaces the cache volume"));
// A high-risk plan that could be taken a safer way says so, because
// the safer way is on a different screen and nobody will guess.
check("a high-risk plan says to read the blast radius", card.includes("High risk"));
check("a restoring plan offers the reversible route", text(card).includes("alongside the live data"), text(card).slice(0, 900));
const gentle = renderToString(h(PendingPlan, { plan: { ...PENDING, risk: "low", plan: { steps: [] } }, onResolved: () => {} }));
check("a low-risk plan is not shouted at", !gentle.includes("High risk"));
check("a plan with no steps says so", gentle.includes("carries no expanded steps"));

const plans = renderToString(h(PlansView, { plans: [PENDING, RESOLVED], onResolved: () => {} }));
check("pending plans are cards, resolved ones a table", plans.includes("Resolved") && plans.includes("plan-9b2d10"));
check("a resolved plan records who resolved it", plans.includes("mika"));
// "never" would be a worse answer than nothing for a plan resolved
// before the API recorded who and when.
const older = renderToString(h(PlansView, { plans: [{ ...RESOLVED, resolvedBy: undefined, resolvedAt: undefined }], onResolved: () => {} }));
check("a plan with no resolution record says nothing, not never", !older.includes("never"), older);
const quiet = renderToString(h(PlansView, { plans: [RESOLVED], onResolved: () => {} }));
check("nothing pending says so", quiet.includes("Nothing is waiting for you"));
check("nothing pending still shows the history", quiet.includes("plan-9b2d10"));

const steps = renderToString(h(PlanSteps, { plan: PENDING.plan }));
check("steps name their target as kind/name", steps.includes("Volume") && steps.includes("jellyfin-cache"));

// ---------------------------------------------------------- Activity
const EVENTS: EventRow[] = [
  { seq: 1, time: "2026-09-07T00:00:00Z", type: "reconcile.applied", node: "n1", message: "n1 converged" },
  { seq: 2, time: "2026-09-07T00:02:00Z", type: "reconcile.failed", node: "n2", message: "n2 step failed" },
  { seq: 3, time: "2026-09-07T00:03:00Z", type: "plan.proposed", message: "plan-0f3a91 proposed", attrs: { plan: "plan-0f3a91", actingFor: "" } },
];
const now = new Date("2026-09-07T00:05:00Z");
const act = renderToString(h(ActivityView, { events: EVENTS, now, error: null, loading: false }));
check("activity is newest first", act.indexOf("plan.proposed") < act.indexOf("reconcile.applied"), "");
check("activity offers its four filters", ["Everything", "Needs me", "Failures", "By node"].every((f) => act.includes(f)));
check("activity shows ages, not raw timestamps", text(act).includes("2m ago") && !act.includes("2026-09-07T00:03"), "");
// An attribute with no value is noise, not information.
check("empty attributes are dropped", !act.includes("actingFor="), "");
const quietAct = renderToString(h(ActivityView, { events: [], now, error: null, loading: false }));
check("an empty stream says why it is empty", quietAct.includes("since the console was opened"));
const loadingAct = renderToString(h(ActivityView, { events: [], now, error: null, loading: true }));
check("a stream with no poll yet says it is waiting", loadingAct.includes("Waiting for the first poll"));

// ----------------------------------------------------------- Backups
const RUNS: BackupRun[] = [
  { seq: 1, planName: "identity", runKind: "scheduled", startedAt: "2026-09-06T03:00:00Z", finishedAt: "2026-09-06T03:01:16Z", status: "succeeded", detail: "214 MB in 76s" },
  { seq: 2, planName: "jellyfin", runKind: "scheduled", startedAt: "2026-09-07T03:00:00Z", status: "failed", detail: "repository is locked by a stale run." },
  { seq: 3, planName: "identity", runKind: "scheduled", startedAt: "2026-09-07T03:00:00Z", finishedAt: "2026-09-07T03:01:02Z", status: "succeeded" },
];
const ledgers = byPlan(RUNS);
check("a failing plan sorts above a healthy one", ledgers[0].name === "jellyfin", ledgers.map((l) => l.name).join(","));
check("each plan keeps its newest run first", ledgers[1].runs[0].seq === 3);

// A failure that repeats the engine's sentence leaves the operator
// where they started. A locked repository has one specific answer.
const locked = explain(RUNS[1], ledgers[0]);
check("a locked repository explains itself", locked.includes("stale lock") || locked.includes("stale run"), locked);
check("a locked repository says unlocking is a plan", locked.includes("goes through a plan"), locked);

const backups = renderToString(h(BackupsView, { runs: RUNS, onRan: () => {} }));
check("each backup plan gets a card", backups.includes("jellyfin") && backups.includes("identity"));
check("a backup card offers both actions", backups.includes("Run now") && backups.includes("Restore alongside"));
check("a backup card shows its run history", text(backups).includes("Last 2 runs") && text(backups).includes("Last run"), text(backups).slice(0, 400));
check("a two-run history is actually drawn", backups.includes("last 2 recorded runs"), "");
const noBackups = renderToString(h(BackupsView, { runs: [], onRan: () => {} }));
check("no runs explains when a plan appears", noBackups.includes("after its first run"));

console.log(failures === 0 ? "\nALL CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
