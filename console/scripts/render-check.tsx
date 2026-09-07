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
import { Butler, StatusCard, SURFACES, Thread, ToolTrace, Turn } from "../src/views/Butler";
import { Chat, IssuedCode } from "../src/views/Chat";
import { Freshness, Shell, TokenGate } from "../src/App";
import { HomeView, buildProblems } from "../src/views/Home";
import { ServicesView, InstalledTile, AvailableRow, matching } from "../src/views/Services";
import { groupServices, upstreamContainer } from "../src/services";
import { eventKind } from "../src/ui/HourChart";
import type { ConsoleSnapshot } from "../src/state";
import type { BackupRun, EventRow, Observed, PackageView, PlanRecord, ResourceView } from "../src/api";
import { PlansView, PendingPlan, PlanSteps } from "../src/views/Plans";
import { ActivityView } from "../src/views/Activity";
import { BackupsView, byPlan, explain } from "../src/views/Backups";
import { SecretsView, usedBy } from "../src/views/Secrets";
import { ChatView } from "../src/views/Chat";
import { IdentityCode } from "../src/views/Identity";
import { Inventory, NodeCard } from "../src/views/Nodes";
import type { ChatBinding, ChatTransport, NodeInfo, SecretInfo } from "../src/api";
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
import { Prerequisites } from "../src/views/Chat";
import { Household, AddMember } from "../src/views/Household";
import { ConnectChat, InstallStep, InviteStep, surfaceOf, vendorSecret } from "../src/views/ConnectChat";

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

// The thread (ADR-W027). A proposal renders as the SAME card Plans
// shows, with the same handlers — one component, two placements, which
// is what keeps "the Butler proposes but never acts" visible. This
// screen never approves anything by itself and must not imply it did.
const PROPOSAL: PlanRecord = {
  seq: 1, id: "plan-77", createdAt: "2026-09-07T00:01:00Z", principal: "butler", actingFor: "mika",
  summary: "prune the jellyfin transcode cache", risk: "medium", state: "proposed",
  plan: { steps: [{ kind: "Volume", name: "jellyfin-cache", action: "prune", risk: "medium", blastRadius: "drops cached transcodes" }] },
};
const answerTurn = renderToString(
  h(Turn, {
    turn: { ordinal: 1, role: "assistant", text: "I've proposed a plan; you approve it.", createdAt: "", planIds: ["plan-77"] },
    proposals: [PROPOSAL], onResolved: () => {},
  }),
);
check("a proposal in the thread is the plan card", answerTurn.includes("prune the jellyfin transcode cache"));
check("the thread's proposal keeps the approve button", answerTurn.includes("Approve") && answerTurn.includes("Reject"));
check("the thread's proposal shows the blast radius", answerTurn.includes("drops cached transcodes"));
// A proposal whose plan record has not been fetched still names it
// rather than silently dropping it.
const unfetched = renderToString(
  h(Turn, {
    turn: { ordinal: 1, role: "assistant", text: "Proposed.", createdAt: "", planIds: ["plan-88"] },
    proposals: [], onResolved: () => {},
  }),
);
check("an unfetched proposal is still named", unfetched.includes("plan-88") && unfetched.includes("open Plans"));

// The tool trace is a quiet line, not a panel: what the Butler read is
// worth being able to check and not worth reading every time.
const trace = renderToString(
  h(ToolTrace, {
    turn: { ordinal: 1, role: "assistant", text: "x", createdAt: "",
      transcriptJson: JSON.stringify({ provider: "ollama", calls: [
        { name: "get_health", input: { kind: "Container" }, result: "1 unhealthy" },
        { name: "get_health", input: {}, result: "ok" },
        { name: "get_resource", input: {}, result: "ok" },
      ] }) },
  }),
);
check("the trace counts the calls and names them once each", text(trace).includes("3 tool calls · get_health, get_resource"), text(trace));
check("the trace is collapsed by default", !trace.includes("1 unhealthy"), trace);
const noTrace = renderToString(h(ToolTrace, { turn: { ordinal: 1, role: "assistant", text: "x", createdAt: "" } }));
check("an answer with no tool calls shows no trace", noTrace === "", noTrace);
// A transcript that is not the JSON we expect must not take the screen
// down with it.
const junkTrace = renderToString(h(ToolTrace, { turn: { ordinal: 1, role: "assistant", text: "x", createdAt: "", transcriptJson: "not json" } }));
check("an unreadable transcript is survived, not thrown on", junkTrace === "", junkTrace);

// A conversation reads as one: the question, then the answer.
const thread = renderToString(
  h(Thread, {
    surface: "admin", pending: null, proposals: [], onProposalResolved: () => {},
    session: {
      seq: 1, id: "bs-1", createdAt: "", principal: "butler", actingFor: "mika",
      provider: "ollama", mode: "full", surface: "admin", finalText: "Two nodes.", capped: false,
      turns: [
        { ordinal: 0, role: "user", text: "what nodes do I have?", createdAt: "" },
        { ordinal: 1, role: "assistant", text: "Two nodes: n1 and n2.", createdAt: "" },
      ],
    },
  }),
);
check("the thread shows the question and the answer in order",
  thread.indexOf("what nodes do I have?") < thread.indexOf("Two nodes: n1 and n2."), thread);
check("the question is styled as the person's", thread.includes("msg-user"));
// A turn that hit the tool-call cap says its answer may be incomplete.
const capped = renderToString(
  h(Turn, { turn: { ordinal: 1, role: "assistant", text: "partial", createdAt: "", capped: true }, proposals: [], onResolved: () => {} }),
);
check("a capped turn admits it may be incomplete", capped.includes("may be incomplete"));

// An empty thread teaches the surface rather than showing a blank box.
const emptyThread = renderToString(
  h(Thread, { surface: "family", session: null, pending: null, proposals: [], onProposalResolved: () => {} }),
);
check("an empty thread teaches the surface's ceiling", emptyThread.includes("no access to the home lab"), emptyThread);

// The two surfaces (ADR-W026 A1). The screen defaults to the operator's
// and offers the household's beside it; the household blurb has to say
// what that butler CANNOT do, because "ask the butler" reading the same
// on both surfaces is how a person ends up asking the family butler to
// restart a container and believing it might.
check("butler screen offers both surfaces", butler.includes("Operator") && butler.includes("Household"));
check("butler screen defaults to the operator surface",
  /class="btn btn-primary"[^>]*>Operator/.test(butler), butler.slice(0, 600));
// Before a status response arrives the screen shows the unconfigured
// card, which is what a fresh install sees — never a blank thread.
check("butler screen is never blank while it checks", butler.includes("Checking the Butler") || butler.includes("Not configured"), butler.slice(0, 400));
const adminThread = renderToString(
  h(Thread, { surface: "admin", session: null, pending: null, proposals: [], onProposalResolved: () => {} }),
);
check("the operator thread promises approval, not action", adminThread.includes("waits for your approval"), adminThread);
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
check("issued code warns it is shown once", code.includes("only time it is shown"), code);

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
for (const label of ["Home", "Services", "Butler", "Chat", "Household", "Connect chat", "Plans", "Activity", "Backups", "Nodes", "Secrets", "Identity"]) {
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

// Forty-six available packages is a scroll, and the catalog only
// grows. Search filters on name AND capability, because which one you
// remember depends on why you are looking.
const CATALOG: PackageView[] = [
  { name: "media-jellyfin", version: "1.0.0", capability: "media", profile: "jellyfin", enabled: false },
  { name: "photos-immich", version: "1.0.0", capability: "photos", enabled: false },
  { name: "books-kavita", version: "1.0.0", capability: "books", enabled: false },
];
check("search finds a package by its name", matching(CATALOG, "jellyfin").length === 1);
check("search finds a package by what it does", matching(CATALOG, "photos").length === 1);
check("search finds a package by its profile", matching(CATALOG, "jellyfin")[0].name === "media-jellyfin");
check("search is case-insensitive and substring", matching(CATALOG, "KAVI").length === 1);
check("an empty query is not a filter", matching(CATALOG, "   ").length === 3);
check("a query that matches nothing returns nothing", matching(CATALOG, "zzz").length === 0);

const searched = renderToString(h(ServicesView, { pkgs: CATALOG, onInstalled: () => {} }));
check("the catalog offers a search box", searched.includes('aria-label="search the catalog"'), searched.slice(0, 400));
// The install form opens beside the catalog, not under it: inline at
// the bottom meant scrolling past every row to reach it.
check("the install form is not rendered inline", !searched.includes("Propose plan"), searched.slice(0, 300));

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

// ------------------------------------------------------------- Nodes
// Uptime is the numeral because it is the question: a node up for
// eleven days and one that reconnected ninety seconds ago were both
// just "connected".
const NODES: NodeInfo[] = [
  { id: "n1", connected: true, agentVersion: "0.9.2", protocolMinor: 3,
    firstJoinedAt: "2026-06-01T00:00:00Z", lastSeenAt: "2026-09-07T00:05:00Z",
    connectedSince: new Date(Date.now() - 3 * 86400000 - 4 * 3600000).toISOString() },
  { id: "n2", connected: false, agentVersion: "0.9.2",
    firstJoinedAt: "2026-06-01T00:00:00Z", lastSeenAt: new Date(Date.now() - 6 * 60000).toISOString() },
];
const up = renderToString(h(NodeCard, { node: NODES[0], samples: ["ok", "ok", "ok", "ok"] }));
check("a connected node leads with its uptime", text(up).includes("3d 4h") && text(up).includes("connected"), text(up));
const down = renderToString(h(NodeCard, { node: NODES[1], samples: [] }));
check("a disconnected node dates its last sighting", text(down).includes("6m") && text(down).includes("since last seen"), text(down));
check("a disconnected node is not pulsing", !down.includes("dot pulse"), down);

const fleet = renderToString(h(Inventory, { nodes: NODES, error: null, loading: false, reload: () => {} }));
check("the fleet counts what is connected", text(fleet).includes("1 of 2 connected"), text(fleet));
const noFleet = renderToString(h(Inventory, { nodes: [], error: null, loading: false, reload: () => {} }));
check("an empty fleet says how to add one", noFleet.includes("Mint a join token"));

// ----------------------------------------------------------- Secrets
// Rotation re-creates dependents, so the list of names was a dare
// without the column that says what rotating one will restart.
const owners = usedBy(PACKAGES.concat([{
  name: "identity", version: "1.9.3", capability: "identity", enabled: true,
  resources: [
    { kind: "Secret", name: "authelia-jwt-secret" },
    { kind: "Secret", name: "lldap-jwt-secret" },
    { kind: "Container", name: "authelia" },
  ],
}]));
check("a secret is attributed to the capability that owns it", owners.get("authelia-jwt-secret")?.join(",") === "identity", JSON.stringify([...owners]));
check("a container is not mistaken for a secret", owners.get("authelia") === undefined);

const SECRETS: SecretInfo[] = [
  { name: "authelia-jwt-secret", version: 2, updatedUnixSec: 1757000000 },
  { name: "orphaned-secret", version: 1 },
];
const secrets = renderToString(h(SecretsView, { node: "n1", secrets: SECRETS, owners, reload: () => {} }));
check("the secrets table says what uses each one", secrets.includes("identity"));
// A secret nothing uses is a fact worth stating: it is the one that is
// safe to rotate and the one that may not belong there.
check("a secret nothing uses says so", secrets.includes("nothing installed uses it"));
check("secrets say a typed value never comes back", secrets.includes("never readable again"));
const noSecrets = renderToString(h(SecretsView, { node: "n1", secrets: [], owners: new Map(), reload: () => {} }));
check("a node with no secrets says so", noSecrets.includes("No secrets on this node yet"));

// -------------------------------------------------------------- Chat
const BINDINGS: ChatBinding[] = [
  { id: 1, transport: "telegram", account: "@sam", displayName: "Sam", surface: "family",
    identity: "sam", createdAt: "2026-09-01T00:00:00Z", createdBy: "mika",
    lastUsedAt: "2026-09-06T00:00:00Z" },
  { id: 2, transport: "telegram", account: "@old", surface: "admin", identity: "mika",
    createdAt: "2026-08-01T00:00:00Z", createdBy: "mika", revokedAt: "2026-09-02T00:00:00Z" },
];
const TRANSPORTS: ChatTransport[] = [
  { transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: true, credentialed: true, online: true },
  { transport: "telegram", surface: "admin", capability: "chat-telegram-admin", installed: true, credentialed: true, online: true },
];
const chatProps = {
  transports: TRANSPORTS,
  members: [{ value: "sam", label: "sam · Sam" }],
  principals: [{ value: "mika", label: "mika · operator" }],
  onToggleRevoked: () => {},
  reload: () => {},
};
const chat2 = renderToString(h(ChatView, { ...chatProps, bindings: BINDINGS, showRevoked: true }));
// "family" and "admin" are wire values. The screen says Household and
// Operator, which is what the Butler's own surface switch calls them.
check("a binding names its butler the way the Butler screen does", chat2.includes("Household") && chat2.includes("Operator"), chat2.slice(0, 600));
check("a revoked binding is shown as revoked, not hidden", chat2.includes("revoked"));
check("a binding that has never been used says never", chat2.includes("never"));
const noneBound = renderToString(h(ChatView, { ...chatProps, bindings: [], showRevoked: false }));
check("no bindings explains how one is made", noneBound.includes("send it to the bot"));

// ---------------------------------------------------------- Identity
// The code is read aloud or typed by hand, once. A misread character is
// a lockout, so it gets display size — and no invented countdown: the
// wire carries no expiry, so the screen dates the READ instead.
const idCode = renderToString(h(IdentityCode, { note: { code: "483 190", note: "for sam@home.arpa" }, readAt: new Date(Date.now() - 60000) }));
check("the identity code is set at display size", idCode.includes("identity-code"), idCode);
check("the identity code says when it was read", text(idCode).includes("read 1m ago"), text(idCode));
check("the identity screen invents no expiry", !idCode.includes("expires"), idCode);
const noCode = renderToString(h(IdentityCode, { note: { note: "" }, readAt: new Date() }));
check("no code waiting says what to do", noCode.includes("Ask the person to request one"));
// An invite needs a person to invite and a bot to send the code to. An
// empty dropdown is not an answer, so the screen says which is missing
// and where it is fixed.
const missing = renderToString(
  h(Prerequisites, {
    surface: "family", usable: [], chosen: undefined, allTransports: [
      { transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: false, credentialed: false, online: false },
    ], haveIdentities: false,
  }),
);
check("no members points at the Household screen", missing.includes("Household screen"));
check("no adapter names the package to install", missing.includes("chat-telegram-family"));

// Installed but never given a token: the bot is running and cannot
// redeem anything, which looks identical to a working one.
const uncredentialed = renderToString(
  h(Prerequisites, {
    surface: "family",
    usable: [{ transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: true, credentialed: false, online: true }],
    chosen: { transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: true, credentialed: false, online: true },
    allTransports: [], haveIdentities: true,
  }),
);
check("an adapter with no token is called out", uncredentialed.includes("wild chat adapter"));

// Household members (ADR-W020 A3). The screen has to say the two things
// an operator would otherwise assume wrongly: adding somebody proposes a
// plan rather than creating an account, and no password is set here.
const household = renderToString(h(Household));
check("Household renders without throwing", household.length > 0);
check("household screen says adding proposes a plan", household.includes("proposes a plan"));
check("household screen says the password is not set here", household.includes("reset flow"));

const addMember = renderToString(
  h(AddMember, {
    groups: [
      { name: "photos_users", owner: "capability/photos", node: "n1" },
      { name: "media_users", owner: "capability/media", node: "n1" },
    ],
    members: [{ username: "sam", groups: ["photos_users"], owner: "system", node: "n1" }],
    onDone: () => {},
  }),
);
check("group checkboxes are offered, not typed", addMember.includes("photos_users") && addMember.includes('type="checkbox"'));
check("one directory node is not asked for", !addMember.includes("directory node"));
check("no password field on the add form", !addMember.includes('type="password"'));

// A deployment with two directories has to be asked which one, because
// a person declared against the wrong node is never provisioned.
const twoNodes = renderToString(
  h(AddMember, {
    groups: [
      { name: "photos_users", owner: "capability/photos", node: "n1" },
      { name: "media_users", owner: "capability/media", node: "n2" },
    ],
    members: [],
    onDone: () => {},
  }),
);
check("two directories are disambiguated", twoNodes.includes("directory node"));

// Connect a chat app (ADR-W026 A2/A3). The steps are read from the
// deployment, and the payoff is a link that carries the code.
const connect = renderToString(h(ConnectChat));
check("Connect screen renders without throwing", connect.length > 0);
check("connect screen says each surface is its own bot", connect.includes("its own bot"));
check("surface comes from the capability name", surfaceOf("chat-telegram-admin") === "admin" && surfaceOf("chat-telegram-family") === "family");

// The install form is generated from the PACKAGE. chat-matrix-* asks
// for a homeserver_url that chat-telegram-* does not, and a form that
// sent only Telegram's params had every Matrix install rejected for a
// missing required param before a plan could exist.
const matrixPkg = {
  name: "chat-matrix-family", version: "1.0.0", capability: "chat-matrix-family", enabled: false,
  params: [
    { name: "node", required: true },
    { name: "adapter_image", required: true },
    { name: "keystone_url", required: true },
    { name: "homeserver_url", required: true },
  ],
  providedSecrets: ["chat-matrix-family-token", "chat-matrix-family-adapter"],
};
const install = renderToString(
  h(InstallStep, { pkg: matrixPkg as never, node: "n1", nodes: [], done: false, onDone: () => {} }),
);
check("install form asks for every package param", install.includes("homeserver_url") && install.includes("adapter_image"));
check("the node is not asked for twice", !install.includes(">node<"));
// #108 made the agent's pull carry registry auth from a node secret,
// and updated the packages and the install guide. This screen still
// said credentials were impossible, which is the shape of copy that
// rots silently: it is a sentence, not a type. Assert the correction so
// the next change to that behaviour has to come past here.
check("the install step does not claim credentials are impossible",
  !/NO credentials|carries no registry auth/.test(install), install.slice(0, 400));
check("the install step says a private registry needs a node secret",
  install.includes("secret named after the registry"), install.slice(0, 400));

// A param the deployment can enumerate is a choice, not a blank —
// retyping a node id from memory is how a capability lands on a machine
// that does not exist. The Services form has always done this; this
// screen asked for free text even when the answer was known.
const sourced = renderToString(h(InstallStep, {
  pkg: {
    name: "chat-telegram-family", version: "1.0.1", capability: "chat-telegram-family", enabled: false,
    params: [
      { name: "node", required: true },
      { name: "adapter_image", required: true, suggested: "reg:5000/chat-adapter@sha256:abc" },
      { name: "keystone_url", required: true, suggested: "http://100.64.0.59:9441" },
      { name: "extra", required: true, options: ["one", "two"] },
    ],
  } as never,
  node: "n1", nodes: [], done: false, onDone: () => {},
}));
check("an enumerable param renders as a picker", /<select[^>]*aria-label="extra"/.test(sourced), sourced.slice(0, 600));
check("the picker offers what the deployment knows", sourced.includes(">one<") && sourced.includes(">two<"));
// A known answer must be IN the field, not grey placeholder text behind
// it: that is how an operator ends up believing it is theirs to work out.
check("a suggested answer is prefilled, not merely hinted",
  sourced.includes('value="http://100.64.0.59:9441"'), sourced.slice(0, 900));
check("a suggested image is prefilled too",
  sourced.includes('value="reg:5000/chat-adapter@sha256:abc"'), sourced.slice(0, 900));
// And with nothing suggested the field stays empty and gates the button.
check("an unanswerable param still gates the install", install.includes("fill in"), install.slice(0, 300));
check("propose is gated until the required params are filled", install.includes("fill in"));

// The operator-supplied secret is read from the package, never guessed:
// Telegram declares <capability>-bot and Matrix <capability>-token, and
// a guessed name writes the token into a secret nothing reads.
check("matrix vendor secret", vendorSecret(matrixPkg as never) === "chat-matrix-family-token");
check("telegram vendor secret", vendorSecret({
  capability: "chat-telegram-family",
  providedSecrets: ["chat-telegram-family-bot", "chat-telegram-family-adapter"],
} as never) === "chat-telegram-family-bot");

const online = {
  transport: "telegram", surface: "family" as const, capability: "chat-telegram-family",
  installed: true, credentialed: true, handle: "wildstone_house_bot", online: true,
};
const invite = renderToString(
  h(InviteStep, {
    surface: "family", transport: online,
    members: [{ username: "sam", displayName: "Sam", groups: [], owner: "system", node: "n1" }],
    principals: [],
  }),
);
check("invite offers the declared member", invite.includes("Sam"));
check("invite asks for a link, not a code", invite.includes("Get invite link"));

// With nobody declared there is nothing to invite, and the screen says
// where that is fixed rather than showing an empty dropdown.
const nobody = renderToString(
  h(InviteStep, { surface: "family", transport: online, members: [], principals: [] }),
);
check("no members points at the Household screen", nobody.includes("Household screen"));

// The link replaces the code when there is one: two things to hand over
// invites handing over the wrong one.
const linked = renderToString(
  h(IssuedCode, {
    claim: {
      code: "K7QP-3XMR-9TWD", surface: "family", identity: "sam",
      expiresAt: "2026-09-07T18:00:00Z", note: "Send this link to sam.",
      handle: "wildstone_house_bot",
      link: "https://t.me/wildstone_house_bot?start=K7QP-3XMR-9TWD",
    },
  }),
);
check("the link is what is shown", linked.includes("t.me/wildstone_house_bot?start=K7QP-3XMR-9TWD"));
check("the raw code is not shown beside it", !linked.includes(">K7QP-3XMR-9TWD<"));

console.log(failures === 0 ? "\nALL CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
