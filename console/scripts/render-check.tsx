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
import { existsSync, readdirSync, readFileSync } from "fs";
import { join } from "path";
import { renderToString } from "react-dom/server";
import { createElement as h } from "react";
import { Services, InstallForm } from "../src/views/Services";
import { Butler, bucket, ConversationRail, EmptyState, IssuedCredential, RuntimeCard, StatusCard, SURFACES, Thread, ThreadBar, ToolTrace, Turn } from "../src/views/Butler";
import type { RuntimeRecord } from "../src/api";
import { BotList, Chat, ConnectForm } from "../src/views/Chat";
import { Freshness, Shell, SignIn } from "../src/App";
import { HomeView, buildProblems } from "../src/views/Home";
import { ServicesView, InstalledTile, CatalogCard, InstalledDetail, AppLogo, tileKind, matching, RemoveForm, ownerCounts, ownedByPackage } from "../src/views/Services";
import { groupServices, upstreamContainer , healthOf, GROUPS, groupOf, mappedCapabilities } from "../src/services";
import { healthOf as butlerHealthOf } from "../src/ui/status";
import { eventKind } from "../src/ui/HourChart";
import type { ConsoleSnapshot } from "../src/state";
import { AUDIENCE_HOUSEHOLD, AUDIENCE_OWNER } from "../src/api";
import type { BackupRun, ButlerSession, EventRow, Observed, PackageView, PlanRecord, ResourceView } from "../src/api";
import { PlansView, PendingBand, PendingPlan, PlanSteps, ResolvedHistory } from "../src/views/Plans";
import { Activity, ActivityView } from "../src/views/Activity";
import { RoutineList, ROUTINE_MEANING } from "../src/views/Routines";
import { occurrences, Revisions, Runs, WhatItDoes } from "../src/views/RoutineDetail";
import { changeWarning, EditForm } from "../src/views/RoutineEdit";
import { diffReplays, summarise } from "../src/views/RoutineDryRun";
import { correlated, Envelope, HouseholdStreamView, matchingEvents, routineOf, setOff } from "../src/views/HouseholdStream";
import type { ReplayDecision, ReplayResult } from "../src/api";
import type { AutomationGrammar, RoutineDetail } from "../src/api";
import type { HouseholdEvent } from "../src/api";
import type { HouseholdRoutine } from "../src/api";
import type { AutomationSpec } from "../src/generated/resources";
import { BackupsView, byPlan, explain } from "../src/views/Backups";
import { byUrgency, CommitmentsView, isUnresolved, standingKind, standingLabel } from "../src/views/Commitments";
import { SecretsView, usedBy, purposeOf } from "../src/views/Secrets";
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

// Screens read sessionStorage (the step-up a sign-in interrupted, the
// last sign-in round trip) while they render, and node has none. A two-line shim is cheaper than
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
import { Household, AddMember, EditMember } from "../src/views/Household";
import { AttentionBanner, Butlers, NoButlers, NoMatches, RosterRow, RuntimesStrip, needsAttention, reachLine, select, toolLine } from "../src/views/Butlers";
import { HouseholdActions, Tool } from "../src/views/butlers/TabTools";
import { ButlerPicker, matches, openingFor, preferred } from "../src/views/butler/picker";
import { BlockedStrip } from "../src/views/Butler";
import { withheldFrom } from "../src/views/butlers/TabReach";
import { retireLines } from "../src/views/butlers/ButlerDrawer";
import { TabRuntime } from "../src/views/butlers/TabRuntime";
import { NewButlerDialog } from "../src/views/butlers/NewButlerDialog";
import { RouteList } from "../src/views/butlers/TabModel";
import { Confirm } from "../src/ui/Confirm";
import { Tabs } from "../src/ui/Tabs";
import type { ButlerRow as ButlerRosterRow, Model, RuntimeInventoryRow } from "../src/api";

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

// A path param renders a PICKER, not a blank box. The free-text version
// is what let "/[REDACTED]" through: the operator was shown a corrupted
// default and had nothing to check it against (ADR-W021 A5).
const dirPkg = {
  name: "audiobooks-audiobookshelf",
  version: "1.0.0",
  capability: "audiobooks",
  enabled: false,
  params: [
    { name: "node", required: true, source: "node", options: ["n1"] },
    { name: "audiobooks_path", default: "/srv/wildstone/media/audiobooks", source: "directory" },
  ],
};
const dirForm = renderToString(h(InstallForm, { pkg: dirPkg as never, onDone: () => {} }));
check("a directory param renders a picker", dirForm.includes('aria-label="folder path"'), dirForm.slice(0, 400));
check("the picker offers browsing", dirForm.includes("Browse"), dirForm.slice(0, 400));
// Until a node is chosen there is no filesystem to browse, and saying so
// beats a button that fails.
check("browsing waits for a node", dirForm.includes("Choose a node first"), dirForm.slice(0, 600));

// A capability already installed must be offered what it is RUNNING
// with, not the package's default. home-assistant was installed with
// timezone=CST and the default is UTC; offering UTC would move a
// household's clock on the next reconfigure.
const installedPkg = {
  name: "home-assistant",
  version: "1.1.0",
  capability: "home-assistant",
  enabled: true,
  params: [{ name: "timezone", default: "UTC", installed: "CST" }],
};
const installedForm = renderToString(h(InstallForm, { pkg: installedPkg as never, onDone: () => {} }));
check("installed value wins over the package default", installedForm.includes('value="CST"'), installedForm.slice(0, 500));
check("the package default is not offered instead", !installedForm.includes('value="UTC"'), installedForm.slice(0, 500));

// Converged is not the same as serving. Wildstone's "healthy" means the
// container matches desired state; whether the app inside can answer
// anybody is a different question, and only the app can answer it.
//
// open-webui was converged and refusing connections at the same time,
// for several minutes, while every surface read 0 unhealthy and the
// dashboard offered a link that would not load.
{
  const conv = (appHealth?: string, healthy = true) =>
    healthOf({ kind: "Container", name: "c", healthy, appHealth } as never);

  check("an app still starting reads amber, not green",
    conv("starting").health === "warn" && conv("starting").state === "starting up",
    JSON.stringify(conv("starting")));
  check("a converged container whose app is unhealthy is not called healthy",
    conv("unhealthy").health === "warn" && conv("unhealthy").state === "running, not ready",
    JSON.stringify(conv("unhealthy")));
  // Most images declare no healthcheck. Silence is not a problem.
  check("no healthcheck reads healthy", conv(undefined).health === "ok", JSON.stringify(conv(undefined)));
  check("a healthy app reads healthy", conv("healthy").health === "ok", JSON.stringify(conv("healthy")));
  // A container that is NOT converged is still the reconciler's problem,
  // whatever the app thinks of itself.
  check("unconverged stays red regardless of the app's opinion",
    conv("healthy", false).health === "bad", JSON.stringify(conv("healthy", false)));
}

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

// The runtime card (ADR-W039 §3.3). The type is read-only here; the
// two acts are the Console holder's, and the secret a rotation returns
// is shown once with the warning that it will not be shown again.
const nativeRuntime: RuntimeRecord = {
  type: "native", credentialRef: "runtime-health-1", health: "connected",
  negotiated: { delivery: "push" }, executionProfileRef: "platform-only",
};
const rtOwner = renderToString(h(RuntimeCard, {
  name: "health", runtime: nativeRuntime, loading: false, error: null, canManage: true, onChanged: () => {},
}));
check("the runtime card names the runtime type and its credential",
  text(rtOwner).includes("type: native") && rtOwner.includes("runtime-health-1"), text(rtOwner));
check("the runtime card shows health, delivery and profile",
  rtOwner.includes("connected") && text(rtOwner).includes("delivery: push") && text(rtOwner).includes("profile: platform-only"));
check("an Owner can rotate the credential and revoke the runtime",
  rtOwner.includes("Rotate credential") && rtOwner.includes("Revoke runtime"));
check("the runtime type is not editable here", !rtOwner.includes("<select") && !rtOwner.includes('type="text"'));
check("the runtime card never carries a secret", !rtOwner.includes("wst_"));
const rtMember = renderToString(h(RuntimeCard, {
  name: "health", runtime: nativeRuntime, loading: false, error: null, canManage: false, onChanged: () => {},
}));
check("without manage the runtime card offers no actions",
  !rtMember.includes("Rotate credential") && !rtMember.includes("Revoke runtime") && rtMember.includes("runtime-health-1"));
const rtRevoked = renderToString(h(RuntimeCard, {
  name: "health", runtime: { ...nativeRuntime, health: "revoked" }, loading: false, error: null, canManage: true, onChanged: () => {},
}));
check("a revoked runtime says so and says how it comes back",
  rtRevoked.includes("revoked") && rtRevoked.includes("new credential"), text(rtRevoked));
const rtHermes = renderToString(h(RuntimeCard, {
  name: "health", loading: false, error: null, canManage: true, onChanged: () => {},
  runtime: { type: "hermes", endpoint: "https://hermes-health.internal/", credentialRef: "runtime-health-2",
    health: "unregistered", negotiated: { delivery: "pull", processingClass: "local_only" }, executionProfileRef: "sandboxed" },
}));
check("an external runtime shows its endpoint and declared class",
  rtHermes.includes("hermes-health.internal") && text(rtHermes).includes("class: local_only") && rtHermes.includes("unregistered"));
// Assembled at run time so no token-shaped literal sits in the source
// for a secret scanner to (rightly) flag; see .gitleaksignore.
const fakeRuntimeSecret = ["wst", "0123abcd"].join("_");
const issued = renderToString(h(IssuedCredential, {
  credential: { credentialRef: "runtime-health-2", secret: fakeRuntimeSecret }, onDismiss: () => {},
}));
check("a rotated secret is shown once with a copy button",
  issued.includes(fakeRuntimeSecret) && issued.includes("Copy") && issued.includes("runtime-health-2"));
check("and with the warning that it will not be shown again", issued.includes("only time it will be shown"));
const placed = renderToString(h(IssuedCredential, {
  credential: { credentialRef: "runtime-health-2", node: "n1", secretName: "secret/runtime-health-token" }, onDismiss: () => {},
}));
check("a secret placed on a node is named, not shown",
  placed.includes("n1") && placed.includes("secret/runtime-health-token") && !placed.includes("wst_"));

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
// The surface toggle is gone. It was a control for choosing a
// correspondent that could not name one: a household with three
// household Butlers had a button meaning "one of these", and sending to
// it either picked for you or implied all three answered.
check("the surface toggle is gone", !butler.includes('role="tablist" aria-label="Butler surface"'), butler.slice(0, 900));
check("a Butler selector stands where it was", butler.includes("picker-wrap"), butler.slice(0, 900));
// Nothing is selected before the roster loads, and the screen says so
// rather than naming a Butler it has not read.
check("with no roster yet it names no Butler", text(butler).includes("No Butler yet"), text(butler).slice(0, 600));
// Before a status response arrives the screen shows the unconfigured
// card, which is what a fresh install sees — never a blank thread.
check("butler screen is never blank while it checks", butler.includes("Checking the Butler") || butler.includes("Not configured"), butler.slice(0, 400));
// A surface with no backend must not take the surface switch away with
// it: the OTHER surface may well be configured, and a screen you cannot
// switch away from is a dead end.
check("an unconfigured surface keeps the conversation rail", butler.includes('aria-label="Conversations"'), butler.slice(0, 400));

// The conversation list groups by day, in a fixed order, and names only
// the buckets it actually has.
// Anchored to NOW rather than to absolute dates: written with literal
// timestamps, this passed on the day it was written and called "today"
// yesterday every day after.
const NOW = new Date("2026-09-08T10:00:00Z");
const before = (ms: number) => new Date(NOW.getTime() - ms).toISOString();
const session = (id: string, createdAt: string): ButlerSession => ({
  seq: 1, id, createdAt, principal: "mika", actingFor: "", provider: "ollama",
  mode: "full", surface: "admin", finalText: id + " answer", capped: false,
});
const buckets = bucket([
  session("s-today", before(3600_000)),
  session("s-yesterday", before(26 * 3600_000)),
  session("s-week", before(4 * 86400_000)),
  session("s-old", before(99 * 86400_000)),
], NOW);
check("conversations group newest bucket first",
  buckets.map((b) => b.label).join(",") === "Today,Yesterday,This week,Earlier", JSON.stringify(buckets.map((b) => b.label)));
check("an empty bucket is not named", bucket([session("s", before(3600_000))], NOW).length === 1);
// A session with no usable timestamp is still listed rather than lost.
check("an undated conversation still lands somewhere",
  bucket([session("s", "")], NOW)[0].label === "Earlier");

// A Butler for the chat screen's fixtures. The selector replaced a
// surface toggle, so everything here is now per-Butler. It is its own
// literal rather than borrowing the roster's builder, which is declared
// further down this file.
const chatRow = (
  over: Partial<ButlerRosterRow["butler"]> = {},
  row: Partial<ButlerRosterRow> = {},
): ButlerRosterRow => ({
  butler: {
    name: "health", directive: "keeps track of training and sleep", surface: "family",
    agentClass: "member", model: "qwen3.5:4b", modelKind: "local", providerModel: "qwen3.5:4b",
    provider: "ollama", readiness: "ready", modelEnabled: true, enabled: true,
    createdAt: "2026-03-14T09:00:00Z", createdBy: "tom", updatedAt: "2026-09-09T09:00:00Z",
    ...over,
  },
  leavesTheHouse: false, carriesActions: true, reachable: true,
  people: 2, connections: 1, toolsAllowed: 3, toolsAvailable: 4, ...row,
});

// anHourAgoButStillToday is the rail fixture's timestamp: an hour ago,
// unless an hour ago was yesterday.
//
// "An hour ago" alone is wrong for one hour out of every day. Between
// midnight and 01:00 it lands on the PREVIOUS date, the rail correctly
// heads it "Yesterday", and a check asserting "Today" fails — on a
// commit that passed an hour earlier and will pass again an hour later,
// which is the worst kind of red because nothing in the diff explains
// it.
function anHourAgoButStillToday(): string {
  const now = new Date();
  const hourAgo = new Date(now.getTime() - 3600_000);
  const startOfToday = new Date(now);
  startOfToday.setHours(0, 0, 0, 0);
  return (hourAgo < startOfToday ? now : hourAgo).toISOString();
}

const convoRail = renderToString(h(ConversationRail, {
  rows: [chatRow()], current: chatRow(), onPickButler: () => {}, onManage: () => {},
  openID: "s-today",
  // Dated against the REAL clock: ConversationRail buckets with its own
  // default `now`, so a fixture timestamp would name a different group
  // every day after the one it was written on.
  sessions: { data: [session("s-today", anHourAgoButStillToday())], error: null, loading: false },
  onPick: () => {}, onNew: () => {}, onClose: () => {},
}));
check("the rail heads each day group", convoRail.includes("Today"), convoRail.slice(0, 400));
check("the rail marks the open conversation", convoRail.includes('class="convo on"'), convoRail);
check("the rail offers a new conversation", convoRail.includes("New conversation"));
const emptyRail = renderToString(h(ConversationRail, {
  rows: [chatRow()], current: chatRow(), onPickButler: () => {}, onManage: () => {},
  openID: null, sessions: { data: [], error: null, loading: false },
  onPick: () => {}, onNew: () => {}, onClose: () => {},
}));
// It names the BUTLER it is empty for, not the surface: "no household
// conversations" was true of three Butlers at once.
check("an empty rail names the Butler it is empty for", text(emptyRail).includes("No conversations with health yet"), text(emptyRail));
check("the rail carries the Butler selector", emptyRail.includes("Which Butler?") || emptyRail.includes("picker"), emptyRail.slice(0, 400));

// The header says what is answering. A popover that is shut must not
// render the settings form: the whole point of moving it there was that
// the thread stops moving when you open it.
const bar = renderToString(h(ThreadBar, {
  title: "why is jellyfin unhealthy?", configured: true, open: false,
  status: { configured: true, provider: "ollama", model: "llama3.1" },
  onToggle: () => {}, onClose: () => {}, onNew: () => {}, onManage: () => {},
}));
check("the header names the backend answering", text(bar).includes("ollama · llama3.1"), text(bar));
check("the header names the conversation", bar.includes("why is jellyfin unhealthy?"));
check("a shut popover renders nothing", !bar.includes("Butler settings"), bar);
const barOff = renderToString(h(ThreadBar, {
  title: "New conversation", configured: false, open: false, status: null,
  onToggle: () => {}, onClose: () => {}, onNew: () => {}, onManage: () => {},
}));
check("an unconfigured backend says so in the header", barOff.includes("Not configured"), barOff);

// The opening: the asks are here and nowhere else, so a suggested
// question cannot be pressed halfway through a conversation.
const opsRow = chatRow({ name: "ops", surface: "admin", agentClass: "owner" });
const opening = renderToString(h(EmptyState, { row: opsRow, busy: false, onSend: () => {} }));
for (const ask of SURFACES.admin.asks) {
  check(`the opening offers "${ask}"`, opening.includes(ask), opening.slice(0, 300));
}
// The opening is the BUTLER's now, derived from its own record — a
// table keyed by name would greet the fourth Butler a household made
// with a blank.
check("the opening names the Butler, not the surface", opening.includes("What should ops look into?"), opening.slice(0, 400));
check("the opening carries the composer", opening.includes('aria-label="Ask the Butler"'));
const family = renderToString(h(EmptyState, { row: chatRow(), busy: false, onSend: () => {} }));
check("a household opening names its ceiling", text(family).includes("cannot touch the home lab"), text(family).slice(0, 400));
check("a local Butler says its words stay home", text(family).includes("on your own hardware"), text(family).slice(0, 400));
const cloudOpening = renderToString(h(EmptyState, {
  row: chatRow({ provider: "anthropic" }, { leavesTheHouse: true }), busy: false, onSend: () => {},
}));
check("a cloud Butler says where its words go, first", text(cloudOpening).includes("outside the house: anything you tell it goes to anthropic"), text(cloudOpening).slice(0, 400));
const adminThread = renderToString(
  h(Thread, { surface: "admin", session: null, pending: null, proposals: [], onProposalResolved: () => {} }),
);
check("the operator thread promises approval, not action", adminThread.includes("waits for your approval"), adminThread);
// The household blurb renders only once that surface is selected, so
// assert the copy contract itself rather than the default render.
check("household blurb names the ceiling", SURFACES.family.blurb.includes("no access to the home lab"));
check("household blurb does not promise proposals", !SURFACES.family.blurb.includes("propose changes —"));

// Chat bindings (ADR-W026 A4, ADR-W033 B7). The screen has to teach two
// things a person cannot recover from getting wrong: the code is shown
// once, and a binding reaches ONE agent.
const chat = renderToString(h(Chat));
check("Chat renders without throwing", chat.length > 0);
check("chat screen says bindings are per agent", chat.includes("per\n        agent") || chat.includes("per agent"));
check("chat screen says chat cannot approve", chat.includes("approve"));


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
// One way in (ADR-W033 A4, A4.1): an Owner signing in as themselves
// through the identity provider. A bearer token is never a browser's way
// into the Console, so this screen must not offer a box to paste one.
const gate = renderToString(h(SignIn));
check("sign-in screen offers the sign-in", gate.includes("Sign in"), gate.slice(0, 700));
// An operator who chose a username and password at install must find
// here what matches what they were given.
check("sign-in names signing in as yourself", gate.includes("username and password you chose"), gate.slice(0, 700));
check("sign-in says who the Console is for", gate.includes("Owners"), gate.slice(0, 700));
check("sign-in explains a deployment where it is not set up", gate.includes("not set up"), gate.slice(0, 900));
check("sign-in names the tool that still works from the host", gate.includes("wild"));
check("sign-in offers no token box", !gate.includes("<input") && !/token/i.test(gate), gate);

// Loading: useEffect does not run under renderToString, so this is the
// shell with no snapshot yet — the state every screen starts in.
const shell = renderToString(h(ConsoleStateProvider, { children: h(Shell) }));
check("shell renders with no data yet", shell.length > 0);
check("shell shows the icon rail", shell.includes('aria-label="Console sections"'), shell);
for (const label of ["Home", "Services", "Butler", "Chat", "Household", "Plans", "Activity", "Backups", "Commitments", "Nodes", "Secrets", "Identity"]) {
  check(`rail offers ${label}`, shell.includes(`aria-label="${label}"`));
}
check("the rail has no separate Connect chat screen", !shell.includes('aria-label="Connect chat"'));
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
// Two questions, so two tabs. Installed is a status board; the catalog
// is a decision list, and a card that will not work here says so before
// Install is pressed rather than after a plan is approved.
const installedTab = renderToString(h(ServicesView, { pkgs: PACKAGES, onInstalled: () => {} }));
check("the board opens on what is installed", /aria-selected="true"[^>]*>Installed/.test(installedTab), installedTab.slice(0, 700));
check("both tabs are counted",
  /Installed <span class="tab-count">2<\/span>/.test(installedTab) && /Catalog <span class="tab-count">2<\/span>/.test(installedTab),
  installedTab.slice(0, 400));
// The tile is two lines: name, status, one muted meta line. Everything
// else moved behind it.
check("an installed tile carries its status", installedTab.includes("not answering") === false && installedTab.includes("answering"), installedTab.slice(0, 900));
check("an installed tile is the affordance, not a button on it",
  installedTab.includes('role="button"') && !installedTab.includes("Reconfigure"), installedTab.slice(0, 900));
check("an installed tile names its host, version and size", text(installedTab).includes("jellyfin.home.arpa · 1.1.0 · 0 resources"), text(installedTab).slice(0, 900));
// Worst first: media is 0-of-1 answering, identity is healthy.
check("the worst installed capability leads",
  installedTab.indexOf("media-jellyfin") < installedTab.indexOf("identity"), "");

const catalogTab = renderToString(h(ServicesView, { pkgs: PACKAGES, initialTab: "catalog", onInstalled: () => {} }));
check("the catalog tab lists what could be installed", catalogTab.includes("photos-immich"), catalogTab.slice(0, 600));
check("the catalog tab does not list what already is", !catalogTab.includes("media-jellyfin"), catalogTab.slice(0, 600));
// What stands in the way is stated on the card, not after a plan is
// approved — and the button is genuinely disabled, so a keyboard cannot
// reach the dead end either.
check("a blocker is stated on the card", catalogTab.includes("Zigbee coordinator"));
check("a blocked package cannot be installed", catalogTab.includes("disabled") && catalogTab.includes("Can’t install"), catalogTab.slice(-900));
check("required params are named before the form opens", text(catalogTab).includes("needs public_domain, node"), text(catalogTab).slice(0, 900));
check("a package needing no setup says so", text(catalogTab).includes("no setup"), text(catalogTab).slice(0, 900));
// Blocked sorts last, so the things you can act on are the things you
// read first.
check("blocked packages sort last",
  catalogTab.indexOf("photos-immich") < catalogTab.indexOf("home-automation"), "");

const blocked = renderToString(h(CatalogCard, { pkg: PACKAGES[3], onInstall: () => {} }));
check("the blocked card's Install is disabled", blocked.includes("disabled"), blocked);
const open_ = renderToString(h(CatalogCard, { pkg: PACKAGES[2], onInstall: () => {} }));
check("an unblocked card's Install is not", !open_.includes("disabled"), open_);
// Two profiles of one capability can run side by side, and whether that
// is what you meant is worth being asked before rather than after.
const sibling = renderToString(h(CatalogCard, {
  pkg: { name: "media-plex", version: "1.0.0", capability: "media", profile: "plex", enabled: false },
  siblingNote: "media is already installed as jellyfin — installing this runs both.",
  onInstall: () => {},
}));
check("a sibling profile says the capability is already held", text(sibling).includes("already installed as jellyfin"), text(sibling));

// Group chips, not capability chips: forty capability chips is the same
// crowding as the fifty table rows this replaces.
const GROUPED: PackageView[] = [
  { name: "media-jellyfin", version: "1.0.0", capability: "media", profile: "jellyfin", enabled: false },
  { name: "photos-immich", version: "1.0.0", capability: "photos", enabled: false },
  { name: "dns-adguard", version: "1.0.0", capability: "dns", enabled: false },
  { name: "zigbee2mqtt", version: "1.0.0", capability: "zigbee", enabled: false, blockers: ["no coordinator"] },
];
check("every group in the map is one of the eight families",
  mappedCapabilities().every((c) => (GROUPS as readonly string[]).includes(groupOf({ capability: c }))));
check("an unmapped capability lands in Ops rather than vanishing", groupOf({ capability: "not-a-capability" }) === "Ops");

// The group map must cover the catalog this repository actually ships.
// Without this a new package files itself under Ops by default and
// nobody finds out — the chip row keeps working, the package is just
// filed somewhere nobody would look for it. Read from packages/ rather
// than from a fixture, so adding a package is what fails the build.
const shippedCapabilities = (() => {
  const root = join(process.cwd(), "..", "packages");
  const caps = new Set<string>();
  for (const dir of readdirSync(root, { withFileTypes: true })) {
    if (!dir.isDirectory()) continue;
    const yaml = join(root, dir.name, "package.yaml");
    if (!existsSync(yaml)) continue;
    const m = /^\s*capability:\s*["']?([A-Za-z0-9_.-]+)/m.exec(readFileSync(yaml, "utf8"));
    if (m) caps.add(m[1]);
  }
  return [...caps].sort();
})();
check("the catalog was actually read", shippedCapabilities.length > 20, String(shippedCapabilities.length));
const unmapped = shippedCapabilities.filter((c) => !mappedCapabilities().includes(c));
check("every shipped capability has an explicit group",
  unmapped.length === 0, `unmapped: ${unmapped.join(", ")} — add them to GROUP_OF in src/services.ts`);
// And the other way: a row for a capability nothing ships is a row that
// outlived its package.
const stale_ = mappedCapabilities().filter((c) => !shippedCapabilities.includes(c));
check("the group map names no capability the catalog dropped",
  stale_.length === 0, `no longer shipped: ${stale_.join(", ")}`);
for (const [cap, want] of [["media", "Media"], ["photos", "Media"], ["dns", "Network"], ["zigbee", "Home"],
                           ["identity", "Identity"], ["ai-chat", "AI"], ["chat-matrix", "Chat"],
                           ["matrix-homeserver", "Chat"],
                           ["documents", "Documents"], ["monitoring", "Ops"]] as const) {
  check(`${cap} files under ${want}`, groupOf({ capability: cap }) === want);
}
const mediaOnly = renderToString(h(ServicesView, { pkgs: GROUPED, initialTab: "catalog", initialGroup: "Media", onInstalled: () => {} }));
check("a group filters the catalog to its own family",
  mediaOnly.includes("media-jellyfin") && mediaOnly.includes("photos-immich") && !mediaOnly.includes("dns-adguard"), mediaOnly.slice(0, 700));
const emptyGroup = renderToString(h(ServicesView, { pkgs: GROUPED, initialTab: "catalog", initialGroup: "Identity", onInstalled: () => {} }));
check("a group that matches nothing says so, and offers a way back",
  emptyGroup.includes("matches these filters") && emptyGroup.includes("Clear"), emptyGroup.slice(-500));
// Counting the whole catalog while hiding blocked cards gives a chip
// that reads 1 and shows nothing.
const hidden = renderToString(h(ServicesView, { pkgs: GROUPED, initialTab: "catalog", initialHideBlocked: true, onInstalled: () => {} }));
check("hiding blocked packages drops them from the grid", !hidden.includes("zigbee2mqtt"), hidden.slice(0, 700));
check("a group whose only members are blocked leaves the chip row", !hidden.includes(">Home <"), hidden.slice(0, 900));
check("the remaining chip counts still add up",
  /All <span class="chip-count">3<\/span>/.test(text(hidden)), text(hidden).slice(0, 900));

// The detail drawer is where everything the tile used to carry went.
const detail = renderToString(h(InstalledDetail, { pkg: PACKAGES[0], onReconfigure: () => {}, onRemove: () => {} }));
check("the detail drawer shows where it answers", detail.includes("https://jellyfin.home.arpa"));
check("the detail drawer keeps the per-address failure detail", text(detail).includes("not answering: health 500"), text(detail));
check("the detail drawer carries the decisions", detail.includes("Reconfigure") && detail.includes("Remove"));
check("a stale install is explained, not just marked", text(detail).includes("Running 1.1.0, the catalog has 1.2.0"), text(detail).slice(0, 600));
const noAddrDetail = renderToString(h(InstalledDetail, {
  pkg: { name: "monitoring", version: "0.4.0", capability: "monitoring", enabled: true },
  onReconfigure: () => {},
}));
check("no address is a fact about the capability, not a gap", noAddrDetail.includes("only from inside the household network"), noAddrDetail.slice(0, 500));
check("a capability with nothing removable offers no Remove", !noAddrDetail.includes("Remove"), noAddrDetail);

// tileKind is what both the sort and the pill read, so they cannot
// disagree about whether something is well.
check("a blocked capability reads as bad", tileKind({ ...PACKAGES[1], blockers: ["x"] }) === "bad");
check("no address is neither well nor unwell", tileKind({ name: "x", version: "1", capability: "x", enabled: true }) === "mute");
check("every address answering is well", tileKind(PACKAGES[1]) === "ok");
check("one address not answering is not", tileKind(PACKAGES[0]) === "bad");

// A logo is never an error state: the monogram is underneath, so a
// household with no route to the CDN sees letters rather than a broken
// image.
const noIcon = renderToString(h(AppLogo, { pkg: { name: "media-jellyfin", version: "1", capability: "media", profile: "jellyfin", enabled: false } }));
check("a package with no icon shows a monogram", noIcon.includes(">je<") && !noIcon.includes("<img"), noIcon);
const withIcon = renderToString(h(AppLogo, { pkg: { name: "media-jellyfin", version: "1", capability: "media", profile: "jellyfin", enabled: false, icon: "jellyfin" } }));
check("a package with an icon renders it over the monogram", withIcon.includes("jellyfin.svg") && withIcon.includes(">je<"), withIcon);
check("the logo is never announced; the name beside it carries that", withIcon.includes('aria-hidden="true"') && withIcon.includes('alt=""'));

const noneInstalled = renderToString(h(ServicesView, { pkgs: GROUPED, onInstalled: () => {} }));
check("nothing installed points at the catalog", noneInstalled.includes("Nothing is installed yet"), noneInstalled.slice(-500));
const noCatalog = renderToString(h(ServicesView, { pkgs: [], onInstalled: () => {} }));
check("an empty catalog is a deployment fact, not a filter miss", noCatalog.includes("-catalog-dir"), noCatalog.slice(-400));
// Nothing LEFT to install is a third sentence again: a Clear button
// here cannot change the result, so offering one reads as a broken
// screen rather than as a finished one.
const allInstalled = renderToString(h(ServicesView, {
  pkgs: PACKAGES.map((p) => ({ ...p, enabled: true })), initialTab: "catalog", onInstalled: () => {},
}));
check("an exhausted catalog says so rather than blaming the filters",
  allInstalled.includes("Everything in the catalog is already installed"), allInstalled.slice(-500));
check("an exhausted catalog offers no Clear it cannot honour",
  !allInstalled.includes(">Clear<"), allInstalled.slice(-500));

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

const searched = renderToString(h(ServicesView, { pkgs: CATALOG, initialTab: "catalog", onInstalled: () => {} }));
// The placeholder names WHICH half it filters: a search typed against
// Installed means nothing against the catalog.
check("the catalog offers a search box", searched.includes('aria-label="search the catalog"'), searched.slice(0, 400));
const searchInstalled = renderToString(h(ServicesView, { pkgs: PACKAGES, onInstalled: () => {} }));
check("the search box names the half it filters", searchInstalled.includes('aria-label="search installed"'), searchInstalled.slice(0, 500));
// The install form opens beside the catalog, not under it: inline at
// the bottom meant scrolling past every row to reach it.
check("the install form is not rendered inline", !searched.includes("Propose plan"), searched.slice(0, 300));

// The Console had no way to remove a capability at all, so a
// half-installed one could only be cleared from a terminal — and a
// capability that cannot converge blocks the whole node, which makes
// every LATER plan fail too.
const OWNED: ResourceView[] = [
  { kind: "Container", name: "chat-telegram-family", owner: "capability/chat-telegram-family", node: "n1", yaml: "" },
  { kind: "Secret", name: "chat-telegram-family-bot", owner: "capability/chat-telegram-family", node: "n1", yaml: "" },
  { kind: "Volume", name: "chat-telegram-family-data", owner: "capability/chat-telegram-family", node: "n1", yaml: "" },
  { kind: "SeedFile", name: "chat-telegram-family-config", owner: "capability/chat-telegram-family", node: "n1", yaml: "" },
];
// Removing one profile must not list — or delete — a sibling's
// resources. Filtering by capability did both: it showed Kavita's
// container in what removing Calibre-Web would delete, and then deleted
// it, along with the repository password the survivor still needs.
const BOOKS: ResourceView[] = [
  { kind: "Container", name: "calibre-web", owner: "capability/books", package: "books-calibre-web", node: "n1", yaml: "" },
  { kind: "Container", name: "kavita", owner: "capability/books", package: "books-kavita", node: "n1", yaml: "" },
  { kind: "Secret", name: "restic-books", owner: "capability/books", package: "books-kavita", node: "n1", yaml: "" },
];
const cwOwned = ownedByPackage(BOOKS, { name: "books-calibre-web", version: "1.0.3", capability: "books", enabled: true });
check("a removal lists only the named package's resources", cwOwned.length === 1 && cwOwned[0].name === "calibre-web", JSON.stringify(cwOwned));
// A deployment written before provenance existed still removes cleanly,
// as long as nothing else has claimed the capability.
const legacy: ResourceView[] = [
  { kind: "Container", name: "kavita", owner: "capability/books", node: "n1", yaml: "" },
];
check("an unstamped install is still removable",
  ownedByPackage(legacy, { name: "books-kavita", version: "1.0.3", capability: "books", enabled: true }).length === 1);
// ...but once a sibling HAS claimed it, an unstamped resource is
// ambiguous and is left alone.
const ambiguous: ResourceView[] = [
  { kind: "Container", name: "calibre-web", owner: "capability/books", package: "books-calibre-web", node: "n1", yaml: "" },
  { kind: "Secret", name: "restic-books", owner: "capability/books", node: "n1", yaml: "" },
];
check("an ambiguous unstamped resource is not claimed",
  ownedByPackage(ambiguous, { name: "books-kavita", version: "1.0.3", capability: "books", enabled: true }).length === 0);

// Removal is blocked until the dry run answers. The fallback list is a
// DIFFERENT list — it cannot know what a surviving sibling needs — so a
// click during the loading window would act on a list naming
// Secret/restic-books over a plan that excludes it.
const removeLoading = renderToString(h(RemoveForm, {
  pkg: { name: "books-kavita", version: "1.0.3", capability: "books", enabled: true },
  owned: BOOKS, loading: true, onDone: () => {},
}));
check("removal is not offered while the preview is loading", removeLoading.includes("disabled"), removeLoading.slice(0, 400));
// A preview that failed says so, and still does not offer the button.
const previewFailed = renderToString(h(RemoveForm, {
  pkg: { name: "books-kavita", version: "1.0.3", capability: "books", enabled: true },
  owned: BOOKS, previewError: "node unreachable", onDone: () => {},
}));
check("a failed preview is surfaced", previewFailed.includes("Could not work out what removing this would delete"), previewFailed.slice(0, 500));
check("and removal stays blocked", previewFailed.includes("disabled"), previewFailed.slice(0, 500));
// With the plan in hand, only what Keystone will delete is offered, and
// what a sibling keeps is shown as kept rather than silently dropped.
const withPlan = renderToString(h(RemoveForm, {
  pkg: { name: "books-kavita", version: "1.0.3", capability: "books", enabled: true },
  owned: BOOKS.filter((r) => r.package === "books-kavita"),
  plannedDeletes: [{ kind: "Container", name: "kavita" }],
  onDone: () => {},
}));
check("a shared resource the sibling needs is shown as kept", withPlan.includes("still needs these"), withPlan.slice(0, 900));

const removeForm = renderToString(h(RemoveForm, {
  pkg: { name: "chat-telegram-family", version: "1.0.2", capability: "chat-telegram-family", enabled: true },
  owned: OWNED, onDone: () => {},
}));
check("removal lists every resource it touches", OWNED.every((r) => removeForm.includes(r.name)), removeForm.slice(0, 400));
// Keystone refuses a Volume Delete outright (ADR-W004 A4), so a plan
// containing one can NEVER apply — it fails every pass until F60 sets
// the step aside, taking the rest of the removal with it. Proposing one
// is not a warning-worthy risk; it is a plan that cannot work.
check("a volume is never proposed for deletion", removeForm.includes("items stay"), removeForm.slice(0, 600));
// A SeedFile was written into the app's own volume and belongs to the
// household from the moment it landed (ADR-W029 A2), so it survives a
// removal exactly as the volume does. Listing it as "deleted" would tell
// the operator something untrue.
check(
  "a seeded file is never proposed for deletion",
  !/<td>SeedFile<\/td><td>chat-telegram-family-config<\/td><td>n1<\/td>/.test(removeForm),
  removeForm.slice(0, 800),
);
check(
  "the kept list names the seeded file",
  removeForm.includes("chat-telegram-family-config"),
  removeForm.slice(0, 800),
);
check("and the count excludes it", removeForm.includes("Propose removing 2 resources"), removeForm.slice(0, 900));
check("the volume is still shown, marked kept", removeForm.includes("kept"), removeForm.slice(0, 900));
// A capability that is nothing but a volume has nothing to propose.
const onlyVolumes = renderToString(h(RemoveForm, {
  pkg: { name: "data-only", version: "1.0.0", capability: "data-only", enabled: true },
  owned: [{ kind: "Volume", name: "somedata", owner: "capability/data-only", node: "n1", yaml: "" }],
  onDone: () => {},
}));
check("a capability of only volumes proposes nothing", onlyVolumes.includes("nothing to propose"), onlyVolumes.slice(0, 400));
check("and its button is disabled", onlyVolumes.includes("disabled"), onlyVolumes.slice(0, 400));
check("removal says nothing happens without approval", text(removeForm).includes("until you approve the plan"));
const nothingOwned = renderToString(h(RemoveForm, {
  pkg: { name: "ghost", version: "1.0.0", capability: "ghost", enabled: true },
  owned: [], onDone: () => {},
}));
check("removing something that owns nothing says so", nothingOwned.includes("nothing to remove"));
check("and names the owner it looked for", text(nothingOwned).includes("capability/ghost"), text(nothingOwned).slice(0, 400));

// "Nothing is owned by X" is the same sentence whether X is misspelt,
// whether the resources are filed under another owner, or whether the
// list arrived empty for a reason nobody can see. Showing the owners
// that WERE there is what makes the three distinguishable — this went
// unexplained on a live deployment for want of exactly that.
check("owner counts are tallied and ordered by size",
  JSON.stringify(ownerCounts([
    { kind: "Secret", name: "a", owner: "capability/identity", node: "n1", yaml: "" },
    { kind: "Secret", name: "b", owner: "capability/media", node: "n1", yaml: "" },
    { kind: "Secret", name: "c", owner: "capability/identity", node: "n1", yaml: "" },
  ])) === JSON.stringify([["capability/identity", 2], ["capability/media", 1]]));

const withOwners = renderToString(h(RemoveForm, {
  pkg: { name: "ghost", version: "1.0.0", capability: "ghost", enabled: true },
  owned: [],
  owners: [["capability/identity", 17], ["capability/media", 8]] as [string, number][],
  onDone: () => {},
}));
check("an empty result shows what the statefile DID report",
  withOwners.includes("capability/identity") && withOwners.includes("capability/media"),
  withOwners.slice(0, 600));
check("and totals them, so the list can be compared at a glance",
  text(withOwners).includes("25 resources"), text(withOwners).slice(0, 700));
// A statefile that reported nothing at all is a different problem again.
const noOwnersAtAll = renderToString(h(RemoveForm, {
  pkg: { name: "ghost", version: "1.0.0", capability: "ghost", enabled: true },
  owned: [], owners: [], onDone: () => {},
}));
check("no resources at all is called out as its own oddity",
  noOwnersAtAll.includes("no resources at all"), noOwnersAtAll.slice(0, 400));
check("and offers no button to press", nothingOwned.includes("disabled"), nothingOwned.slice(0, 300));

// "I could not read the statefile" and "this owns nothing" look
// identical from an empty list, and only one of them is safe to
// believe. The first said "nothing to remove" and sent the reason
// nowhere.
const unreadable = renderToString(h(RemoveForm, {
  pkg: { name: "ghost", version: "1.0.0", capability: "ghost", enabled: true },
  owned: [], error: "500: loading statefile", onDone: () => {},
}));
check("a failed read is not reported as an empty one", !unreadable.includes("nothing to remove"), unreadable.slice(0, 300));
check("a failed read says so, and why", unreadable.includes("could not be read") && unreadable.includes("loading statefile"));
const stillLoading = renderToString(h(RemoveForm, {
  pkg: { name: "ghost", version: "1.0.0", capability: "ghost", enabled: true },
  owned: [], loading: true, onDone: () => {},
}));
check("a list still loading is not reported as empty", !stillLoading.includes("nothing to remove"), stillLoading.slice(0, 300));

const noAddr = renderToString(h(InstalledTile, { pkg: { name: "monitoring", version: "0.4.0", capability: "monitoring", enabled: true }, onOpen: () => {} }));
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
// An agent's proposed setting says what approving changes, in words:
// there is no reconciler step to read for it (ADR-W033 E2).
const tzPlan = renderToString(h(PendingPlan, {
  plan: { ...PENDING, risk: "low", actingFor: "mika", plan: undefined,
    summary: "change the household timezone from UTC to America/Chicago (asked by mika)",
    setting: { timezone: "America/Chicago", reason: "we live in Chicago" } },
  onResolved: () => {},
}));
check("a proposed time zone says what approving sets", tzPlan.includes("America/Chicago") && tzPlan.includes("time zone"), tzPlan.slice(0, 600));
check("a proposed time zone says who asked and why", tzPlan.includes("mika") && tzPlan.includes("we live in Chicago"));
const gentle = renderToString(h(PendingPlan, { plan: { ...PENDING, risk: "low", plan: { steps: [] } }, onResolved: () => {} }));
check("a low-risk plan is not shouted at", !gentle.includes("High risk"));
check("a plan with no steps says so", gentle.includes("carries no expanded steps"));

const plans = renderToString(h(PlansView, { plans: [PENDING, RESOLVED], onResolved: () => {} }));
check("pending plans are rows, resolved ones a table", plans.includes("Resolved") && plans.includes("plan-9b2d10"));
check("a resolved plan records who resolved it", plans.includes("mika"));
// The two bands are named and counted, so the page says how much is
// waiting before you have read any of it.
check("the bands are named and counted", text(plans).includes("Waiting for you") && text(plans).includes("Resolved"), text(plans).slice(0, 400));
// Dismissing is safe because the plan is still in the history and still
// on the stream — the line under the band is what says so.
check("the page says where a dismissed failure goes", text(plans).includes("on the stream"), text(plans).slice(0, 600));

// The approval band: one row open at a time, and the summary is not
// printed twice inside the open one.
const band = renderToString(h(PendingBand, {
  plans: [PENDING, { ...PENDING, id: "plan-2", seq: 10, summary: "rotate the grafana admin secret" }],
  onResolved: () => {},
}));
check("the newest waiting plan is open by default", band.includes('aria-expanded="true"') && band.includes('aria-expanded="false"'), band.slice(0, 400));
check("only one approval row is expanded", (band.match(/aria-expanded="true"/g) ?? []).length === 1);
check("the open row keeps the decision", band.includes("Approve") && band.includes("Reject"));
check("the open row does not repeat the summary",
  (text(band).match(/prune the jellyfin transcode cache/g) ?? []).length === 1, text(band).slice(0, 600));
check("a collapsed row is still triageable",
  text(band).includes("rotate the grafana admin secret") && text(band).includes("high risk"), text(band).slice(0, 600));

// The history stops at ten rows. A household with a year of plans
// behind it is still a page that opens.
const many: PlanRecord[] = Array.from({ length: 34 }, (_, i) => ({
  ...RESOLVED, seq: i, id: `plan-${i}`, summary: `plan number ${i}`,
}));
const history = renderToString(h(ResolvedHistory, { plans: many }));
check("the history pages at ten", (history.match(/plan number /g) ?? []).length === 10 * 2, String((history.match(/plan number /g) ?? []).length));
check("the history says how much it is showing", text(history).includes("Showing 10 of 34"), text(history).slice(-200));
check("the history offers the rest", history.includes("Show more"));
const short = renderToString(h(ResolvedHistory, { plans: [RESOLVED] }));
check("a short history does not offer more", !short.includes("Show more"), short.slice(-200));
check("a short history still counts itself", text(short).includes("Showing 1 of 1"));

// A failure that has been dismissed keeps its plan in the history: the
// card is quieter, the record is not gone.
const FAILED: PlanRecord = {
  ...RESOLVED, id: "plan-bad", state: "failed", summary: "install monitoring",
};
const FAIL_EVENT: EventRow[] = [{
  seq: 1, time: "2026-09-06T22:05:00Z", type: "reconcile.failed",
  message: "run `wild secret set grafana-admin` first", attrs: { plans: "plan-bad" },
}];
const withFailure = renderToString(h(PlansView, { plans: [FAILED], events: FAIL_EVENT, onResolved: () => {} }));
check("an explained failure is raised out of the table", withFailure.includes("wild secret set grafana-admin"), text(withFailure).slice(0, 400));
check("a raised failure can be dismissed", withFailure.includes('aria-label="Dismiss"'), withFailure.slice(0, 400));
check("a raised failure is still in the history", (text(withFailure).match(/install monitoring/g) ?? []).length >= 2, text(withFailure));
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

// ------------------------------------------------------- Commitments
// The distinction this screen exists to hold: a commitment nobody can
// evaluate and a commitment that is fine are different answers, and a
// screen that renders the first as the second tells a household
// everything is in order while nobody knows.
const STANDINGS = [
  {
    name: "f150-oil", description: "F-150 oil and filter", owner: "household/mika",
    occurrence: {
      cycle: 2, state: "pending", openedAt: "2026-06-01T09:00:00Z", trigger: "",
      reason: "f150-oil: nothing to say yet — meter \"odo\" was last read 12 weeks ago",
      unresolved: ["odo"], rules: [],
    },
  },
  {
    name: "kai-checkup", description: "Kai's annual well-child visit", owner: "household/kai",
    occurrence: {
      cycle: 1, state: "overdue", openedAt: "2025-09-10T09:00:00Z", trigger: "clock",
      reason: "kai-checkup is overdue (due 10 Sep 2026)", rules: [],
    },
  },
  {
    name: "furnace-filter", description: "Furnace filter", owner: "household",
    occurrence: {
      cycle: 1, state: "pending", openedAt: "2026-06-01T09:00:00Z", trigger: "clock",
      reason: "furnace-filter is not due yet", rules: [],
    },
  },
];
check("a commitment nobody can evaluate is not reported as pending", standingLabel(STANDINGS[0] as never) === "can't tell", standingLabel(STANDINGS[0] as never));
check("and it is not coloured like one that is fine", standingKind(STANDINGS[0] as never) !== standingKind(STANDINGS[2] as never));
check("unresolved is detected from the named meter, not a state", isUnresolved(STANDINGS[0] as never) && !isUnresolved(STANDINGS[2] as never));
const byNeed = byUrgency(STANDINGS as never).map((s) => s.name);
check("overdue sorts first, then what nobody can tell", byNeed[0] === "kai-checkup" && byNeed[1] === "f150-oil", byNeed.join(","));

const METERS = [
  { name: "clock", description: "The calendar.", builtIn: true },
  { name: "odo", description: "Odometer on the F-150", unit: "miles", stale: true,
    latest: { seq: 1, meter: "odo", value: 84100, at: "2026-06-01T09:00:00Z", recordedAt: "2026-06-01T09:00:00Z", recordedBy: "mika", source: "person" } },
  { name: "furnace-hours", description: "Blower hours", unit: "hours" },
];
const commitments = renderToString(h(CommitmentsView, { standings: STANDINGS as never, meters: METERS as never, onChanged: () => {} }));
check("every commitment gets a card", text(commitments).includes("well-child visit") && text(commitments).includes("F-150 oil and filter"), text(commitments).slice(0, 400));
check("the screen asks for the reading it is missing", text(commitments).includes("Needs a reading: odo"), text(commitments).slice(0, 600));
const meterText = text(commitments);
check("a meter nobody has read says so", meterText.includes("never read"));
check("a stale meter says the number cannot be relied on", meterText.includes("too old to rely on"));
check("never read and too old are different sentences", meterText.includes("never read") && meterText.includes("too old to rely on"));
check("the calendar is marked built in and offers no box", meterText.includes("built in") && meterText.includes("declared nowhere"));
const nothingTracked = renderToString(h(CommitmentsView, { standings: [], meters: [], onChanged: () => {} }));
check("nothing tracked explains how one appears", nothingTracked.includes("propose one"));

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
// What a name MEANS. A screen of identifiers is unusable by exactly the
// person who needs one of them: the operator who does not already know.
const purposes = purposeOf([{
  name: "mail-roundcube", version: "1.0.3", capability: "mail", enabled: true,
  resources: [
    { kind: "Secret", name: "authelia-jwt-secret", purpose: "The calendar admin page — sign in as 'admin'." },
    { kind: "Container", name: "davis", purpose: "not a secret" },
  ],
}]);
check("a secret's purpose is read from its package", purposes.get("authelia-jwt-secret")?.includes("sign in as") === true);
check("only secrets carry a purpose", purposes.get("davis") === undefined);

const secrets = renderToString(h(SecretsView, { node: "n1", secrets: SECRETS, owners, purposes, reload: () => {} }));
check("the secrets table says what uses each one", secrets.includes("identity"));
check("the secrets table says what a secret is for", secrets.includes("sign in as"));
// Setting a value you choose is the only way into a service whose
// password was generated, so it is a button on the row rather than a
// name to retype into the form above.
check("each secret can be set from its own row", secrets.includes("Set\u2026") || secrets.includes("Set…"));
check("secrets explain why a generated value cannot be shown", secrets.includes("set it\nto a value you choose") || secrets.includes("to a value you choose"));
// A secret nothing uses is a fact worth stating: it is the one that is
// safe to rotate and the one that may not belong there.
check("a secret nothing uses says so", secrets.includes("nothing installed uses it"));
check("secrets say a typed value never comes back", secrets.includes("never readable again"));
const noSecrets = renderToString(h(SecretsView, { node: "n1", secrets: [], owners: new Map(), purposes: new Map(), reload: () => {} }));
check("a node with no secrets says so", noSecrets.includes("No secrets on this node yet"));

// -------------------------------------------------------------- Chat
const BINDINGS: ChatBinding[] = [
  { id: 1, connection: 1, butler: "hobson", transport: "telegram", account: "@sam", displayName: "Sam",
    identity: "sam", role:"member", createdAt: "2026-09-01T00:00:00Z", createdBy: "mika",
    lastUsedAt: "2026-09-06T00:00:00Z" },
  { id: 2, connection: 2, butler: "alfred", transport: "telegram", account: "@old", identity: "mika", role:"owner",
    createdAt: "2026-08-01T00:00:00Z", createdBy: "mika", revokedAt: "2026-09-02T00:00:00Z" },
];
const TRANSPORTS: ChatTransport[] = [
  { transport: "telegram", butler: "hobson", capability: "chat-telegram-family", installed: true, credentialed: true, online: true },
  { transport: "telegram", butler: "alfred", capability: "chat-telegram-admin", installed: true, credentialed: true, online: true },
];
const chatProps = {
  onToggleRevoked: () => {},
  reload: () => {},
};
// The bot list says whose each bot is and whether its adapter has
// checked in, and offers removal behind a second click.
const botList = renderToString(h(BotList, {
  bots: [
    { capability: "chat-telegram-hobson", platform: "telegram", label: "Telegram", butler: "hobson" },
    { capability: "chat-telegram-alfred", platform: "telegram", label: "Telegram", butler: "alfred" },
  ],
  transports: TRANSPORTS.map((t, i) => ({ ...t, capability: i === 0 ? "chat-telegram-hobson" : "chat-telegram-alfred", online: i === 0 })),
  onChanged: () => {},
}));
check("each bot names its agent", botList.includes("hobson") && botList.includes("alfred"));
check("a bot that has not checked in says so", botList.includes("not checked in") && botList.includes("online"));
check("removing a bot takes a second click", botList.includes("Remove…") && !botList.includes("Remove bot"));
const chat2 = renderToString(h(ChatView, { ...chatProps, bindings: BINDINGS, showRevoked: true }));
// The agent by NAME. A class — "Household", "Operator" — would file
// every owner agent's bindings under one label, and the whole point of a
// connection belonging to one agent is that Alfred's reach is not
// Robert's.
check("a binding names the agent it reaches", chat2.includes("hobson") && chat2.includes("alfred"), chat2.slice(0, 600));
check("a revoked binding is shown as revoked, not hidden", chat2.includes("revoked"));
check("a binding that has never been used says never", chat2.includes("never"));
const noneBound = renderToString(h(ChatView, { ...chatProps, bindings: [], showRevoked: false }));
check("no bindings explains how one is made", noneBound.includes("send their key to the bot"));


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

// Household members (ADR-W020 A3). The screen has to say the two things
// an operator would otherwise assume wrongly: adding somebody proposes a
// plan rather than creating an account, and no password is set here.
const household = renderToString(h(Household));
check("Household renders without throwing", household.length > 0);
check("household screen offers the time zone", household.includes("Time zone") && household.includes("America/Chicago"));
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

// Chat is one screen now (ADR-W033 B7.1): the bots, connecting
// somebody, the people bound and the Owner's own invites. Connecting
// asks for nothing a household has no way to know: no image, no
// package, no "family" or "admin" bot.
const connect = renderToString(h(ConnectForm, { bots: [], onDone: () => {} }));
check("Connect form renders without throwing", connect.length > 0);
check("connect form asks for no adapter image", !/adapter_image|adapter image/i.test(connect));
check("connect form names no family or admin bot", !/chat-(telegram|matrix)-(family|admin)/.test(connect));
check("connect form says the person collects the key", connect.includes("collect their one-time key"), connect.slice(0, 600));
const chatPage = renderToString(h(Chat));
check("the chat screen lists bots and connects people in one place",
  chatPage.includes("Bots") && chatPage.includes("Connect someone") && chatPage.includes("People on chat"), chatPage.slice(0, 600));


// ----------------------------------------------------------- Butlers
//
// The screen's job is to make it impossible to look at a Butler without
// seeing where its words go, so these checks are mostly about the row
// and the drawer telling the truth rather than about them rendering.

const localModel = (over: Partial<Model> = {}): Model => ({
  name: "llama3.1:8b", kind: "local", provider: "ollama", endpoint: "http://studio:11434",
  readiness: "ready", enabled: true, ...over,
});
const CLOUD = localModel({ name: "claude-sonnet", kind: "cloud", provider: "anthropic" });
const QA = localModel({ name: "gemma2:27b", readiness: "qa-only", readinessDetail: "failed tool check" });
const OFF = localModel({ name: "gpt-4o", kind: "cloud", provider: "openai", enabled: false });

const bRow = (over: Partial<ButlerRosterRow["butler"]> = {}, row: Partial<ButlerRosterRow> = {}): ButlerRosterRow => ({
  butler: {
    name: "health", directive: "keeps track of training and sleep", surface: "family",
    agentClass: "member", model: "llama3.1:8b", modelKind: "local", provider: "ollama",
    readiness: "ready", modelEnabled: true, enabled: true,
    createdAt: "2026-03-14T09:00:00Z", createdBy: "tom", updatedAt: "2026-09-09T09:00:00Z",
    ...over,
  },
  leavesTheHouse: false, carriesActions: true, reachable: true,
  people: 2, connections: 2, toolsAllowed: 3, toolsAvailable: 4, ...row,
});

// The destination is in the row, not one click away. This is A2 and A4
// rendered, and it is the single thing this screen exists to get right.
const localRow = renderToString(h(RosterRow, { row: bRow(), selected: false, onOpen: () => {}, model: localModel() }));
check("a local Butler's row says its words stay in the house", localRow.includes("stays in the house"), localRow);
const cloudRow = renderToString(h(RosterRow, {
  row: bRow({ name: "nutrition", model: "claude-sonnet", modelKind: "cloud", provider: "anthropic" }, { leavesTheHouse: true }),
  selected: false, onOpen: () => {}, model: CLOUD,
}));
check("a cloud Butler's row says so without opening it", cloudRow.includes("leaves the house"), cloudRow);

// The pill reads the SERVER's flag, never the route kind beside it. A
// client deriving the most consequential fact on the screen is a client
// that can derive it wrongly, so a row whose kind says cloud and whose
// flag says otherwise must follow the flag — and be a server bug, not a
// screen that quietly disagrees with the API.
const disagreeing = renderToString(h(RosterRow, {
  row: bRow({ model: "claude-sonnet", modelKind: "cloud" }, { leavesTheHouse: false }),
  selected: false, onOpen: () => {}, model: CLOUD,
}));
check("the destination pill follows leavesTheHouse, not the route kind", disagreeing.includes("stays in the house"), disagreeing);

// A Butler nobody can reach is flagged in the roster, because a Butler
// that answers nobody looks exactly like a working one otherwise.
const unreachable = renderToString(h(RosterRow, { row: bRow({}, { people: 0 }), selected: false, onOpen: () => {}, model: localModel() }));
check("a Butler with nobody ticked says so on the row", unreachable.includes("nobody can talk to it"), unreachable);

const retiredRow = renderToString(h(RosterRow, {
  row: bRow({ retiredAt: "2026-08-12T00:00:00Z" }), selected: false, onOpen: () => {}, model: localModel(),
}));
check("a retired Butler reads as kept, not deleted", retiredRow.includes("kept, not deleted"), retiredRow);

// healthOf is the one place readiness becomes words.
check("a working Butler is ready", butlerHealthOf(bRow(), localModel()).label === "ready");
check("a Butler that failed its tool probe answers questions only",
  butlerHealthOf(bRow({}, { carriesActions: false }), QA).label === "questions only");
check("the failure detail comes from the route, not invented",
  butlerHealthOf(bRow({}, { carriesActions: false }), QA).detail === "failed tool check");
check("a Butler on a disabled route names the route, not a generic outage",
  butlerHealthOf(bRow({ modelEnabled: false }, { reachable: false }), OFF).detail.includes("turned off in Model routes"));
check("retired outranks unreachable — it is not answering on purpose",
  butlerHealthOf(bRow({ retiredAt: "2026-08-12T00:00:00Z" }, { reachable: false }), localModel()).kind === "mute");

// The filters. Retired is the only one that inverts the predicate, and
// it is the one worth pinning: every other filter narrows the LIVE set.
const roster = [
  bRow(),
  bRow({ name: "nutrition", directive: "meal planning", model: "claude-sonnet", modelKind: "cloud" }, { leavesTheHouse: true, reachable: false }),
  bRow({ name: "ops", directive: "diagnoses services", surface: "admin", agentClass: "owner" }, { carriesActions: false }),
  bRow({ name: "old", directive: "retired job", retiredAt: "2026-08-12T00:00:00Z" }),
];
check("All hides retired Butlers", select(roster, { filter: "all", query: "", sort: "name" }).length === 3);
check("Retired shows only them", select(roster, { filter: "retired", query: "", sort: "name" }).map((r) => r.butler.name).join() === "old");
check("Leaves the house selects by the server's own flag",
  select(roster, { filter: "cloud", query: "", sort: "name" }).map((r) => r.butler.name).join() === "nutrition");
check("Needs attention catches both unreachable and questions-only",
  select(roster, { filter: "attention", query: "", sort: "name" }).map((r) => r.butler.name).join() === "nutrition,ops");
check("search reads the directive as well as the name",
  select(roster, { filter: "all", query: "sleep", sort: "name" }).map((r) => r.butler.name).join() === "health");
check("a retired Butler never leaks into a surface filter",
  select(roster, { filter: "family", query: "", sort: "name" }).every((r) => !r.butler.retiredAt));
check("needsAttention ignores retired Butlers", !needsAttention(bRow({ retiredAt: "2026-08-12T00:00:00Z" }, { reachable: false })));

// Singularisation, because "1 people" makes a reader distrust the row.
check("one person reads as a person", reachLine(bRow({}, { people: 1, connections: 1 })) === "1 person · 1 connection");
check("nobody reads as no people", reachLine(bRow({}, { people: 0, connections: 0 })).startsWith("no people"));
check("an operator Butler counts operators", reachLine(bRow({ surface: "admin" }, { people: 2 })).startsWith("2 operators"));
check("the tool denominator is what is available here", toolLine(bRow({}, { toolsAllowed: 5, toolsAvailable: 6 })) === "5 of 6 tools");

// The banner names the Butlers and what is wrong with each.
const banner = renderToString(h(AttentionBanner, {
  rows: [roster[1], roster[2]], routes: [localModel(), CLOUD, QA], onShow: () => {},
}));
check("the banner names each Butler and its fault", banner.includes("nutrition can&#x27;t reach its model") && banner.includes("ops answers questions only"), banner);

// TabRuntime (#350/H4): attaching a runtime and setting its execution
// profile, on the Butlers screen — the form RuntimeCard's popover on
// the Butler screen deliberately does not duplicate. Owner-only, like
// rotating or revoking a credential.
const runtimeNative: RuntimeRecord = {
  type: "native", credentialRef: "runtime-health-1", health: "connected",
  negotiated: { delivery: "push" }, executionProfileRef: "platform-only",
};
const trOwner = renderToString(h(TabRuntime, {
  row: bRow(), runtime: runtimeNative, loading: false, error: null, nodes: [],
  canManage: true, say: () => {}, onChanged: () => {},
}));
check("an Owner sees the runtime type and execution profile controls",
  trOwner.includes("Wildstone Native") && trOwner.includes("Host administration") && trOwner.includes("Save runtime"),
  trOwner);
check("endpoint is hidden for the native runtime", !text(trOwner).includes("Endpoint"), trOwner);

const trMember = renderToString(h(TabRuntime, {
  row: bRow(), runtime: runtimeNative, loading: false, error: null, nodes: [],
  canManage: false, say: () => {}, onChanged: () => {},
}));
check("without manage the runtime tab offers no save button and no confirm prompt",
  !trMember.includes("Save runtime") && !trMember.includes("Issue a credential"), trMember);
check("without manage the runtime record is still shown",
  text(trMember).includes("Wildstone Native"), trMember);
check("without manage every control is disabled", trMember.includes("disabled"), trMember);

const runtimeHermes: RuntimeRecord = {
  type: "hermes", endpoint: "https://hermes-health.internal/", credentialRef: "runtime-health-2",
  health: "unregistered", negotiated: { delivery: "pull", processingClass: "local_only" },
  executionProfileRef: "sandboxed",
};
const trHermes = renderToString(h(TabRuntime, {
  row: bRow(), runtime: runtimeHermes, loading: false, error: null,
  nodes: [{ id: "n1", connected: true, firstJoinedAt: "2026-01-01T00:00:00Z", lastSeenAt: "2026-09-01T00:00:00Z" }],
  canManage: true, say: () => {}, onChanged: () => {},
}));
check("the endpoint field appears for a non-native runtime",
  trHermes.includes('id="runtime-endpoint"') && trHermes.includes("hermes-health.internal"), trHermes);
check("a blank endpoint is explained as fine for pull delivery",
  text(trHermes).toLowerCase().includes("leave it blank if the runtime only pulls"),
  trHermes);

const trHostAdmin = renderToString(h(TabRuntime, {
  row: bRow(), runtime: { ...runtimeHermes, executionProfileRef: "host-admin" }, loading: false, error: null, nodes: [],
  canManage: true, say: () => {}, onChanged: () => {},
}));
check("host administration shows its warning",
  trHostAdmin.includes("Host administration") &&
    text(trHostAdmin).includes("Owner-class Agent") &&
    text(trHostAdmin).includes("opt-in tools"),
  trHostAdmin);

const trBlocked = renderToString(h(TabRuntime, {
  row: bRow({ agentClass: "member" }),
  runtime: { ...runtimeNative, executionProfileRef: "host-admin" },
  loading: false, error: null, nodes: [], canManage: true, say: () => {}, onChanged: () => {},
}));
check("host-admin on a native, member-class Agent is flagged before Save",
  text(trBlocked).includes("needs a non-native runtime or an Owner-class Agent"), trBlocked);

// RuntimesStrip (#355, H9): every live Agent's runtime at a glance,
// unhealthy first, Owner-only like the Runtime tab it links to.
const runtimeRows: RuntimeInventoryRow[] = [
  {
    butler: "health", agentClass: "member",
    runtime: { type: "native", credentialRef: "runtime-health-1", health: "connected",
      negotiated: { delivery: "push" }, executionProfileRef: "platform-only" },
  },
  {
    butler: "nutrition", agentClass: "member",
    runtime: { type: "hermes", endpoint: "https://hermes-nutrition.internal/", credentialRef: "runtime-nutrition-2",
      health: "offline", lastHeartbeat: "2026-09-20T09:00:00Z",
      negotiated: { delivery: "push", processingClass: "local_only" }, executionProfileRef: "sandboxed" },
  },
];
const stripOwner = renderToString(h(RuntimesStrip, { rows: runtimeRows, error: null, canManage: true, onOpen: () => {} }));
check("the strip lists every Agent it was given", stripOwner.includes("health") && stripOwner.includes("nutrition"), stripOwner);
check("an offline runtime says so", stripOwner.includes(">offline<"), stripOwner);
check("a connected native runtime says so", stripOwner.includes(">connected<"), stripOwner);
check("the strip shows the runtime type, profile and declared class",
  stripOwner.includes(">hermes<") && stripOwner.includes("sandboxed") && stripOwner.includes("local_only"), stripOwner);
// Unhealthy first: nutrition (offline) sorts ahead of health (connected)
// even though "health" is alphabetically first. Matched by the name
// button's own markup, not the bare word — "health" is also a column
// header, which renders earlier than either row.
check("the strip sorts unhealthy runtimes first",
  stripOwner.indexOf(">nutrition</button>") < stripOwner.indexOf(">health</button>"), stripOwner);

const stripMember = renderToString(h(RuntimesStrip, { rows: runtimeRows, error: null, canManage: false, onOpen: () => {} }));
check("without manage the strip renders nothing", stripMember === "", stripMember);

const stripEmpty = renderToString(h(RuntimesStrip, { rows: [], error: null, canManage: true, onOpen: () => {} }));
check("an Owner with no Agents sees no empty strip", stripEmpty === "", stripEmpty);

// Tool rows: three states, and only two of them are ours to undo.
const onTool = renderToString(h(Tool, {
  tool: { name: "get_household_events", summary: "Reads the household timeline.", available: true },
  busy: false, onSet: () => {},
}));
check("an available tool reads as on", onTool.includes(">on<"), onTool);
check("an available tool offers to take it away", onTool.includes('aria-pressed="true"'), onTool);

const offTool = renderToString(h(Tool, {
  tool: { name: "do_household_action", summary: "Runs one domestic action.", mutating: true, available: false, reason: "health does not have do_household_action" },
  narrowedReason: "it only ever needed to read", busy: false, onSet: () => {},
}));
check("a narrowed tool reads as off, not unavailable", offTool.includes(">off<") && !offTool.includes("unavailable"), offTool);
check("the narrowing carries the reason it was given", offTool.includes("it only ever needed to read"), offTool);
check("a mutating tool says it does something", offTool.includes("does something"), offTool);

const goneTool = renderToString(h(Tool, {
  tool: { name: "get_container_logs", summary: "Reads a container's recent log lines.", available: false, reason: "this deployment has not wired the source behind it" },
  busy: false, onSet: () => {},
}));
check("a tool nothing wired reads as unavailable", goneTool.includes("unavailable"), goneTool);

// Household actions: a family Butler reaches the calendar through one
// tool, and the Tools tab has to show what that tool reaches.
const calendarActions = [
  { tool: "calendar.get_agenda", tier: "read" },
  { tool: "calendar.create_event", tier: "domestic" },
  { tool: "calendar.delete_event", tier: "confirm" },
  { tool: "chat-telegram-family.send", tier: "domestic" },
];
const actionGate = { name: "do_household_action", summary: "Runs one domestic action.", mutating: true, available: true };
const reach = renderToString(h(HouseholdActions, { actions: calendarActions, gate: actionGate, gateOff: false }));
check("every household action is listed by name", calendarActions.every((a) => reach.includes(a.tool)), reach);
check("the actions are grouped by the package that brings them", reach.includes("2 packages") && reach.includes(">calendar<"), reach);
check("a delete says it asks first", reach.includes("asks first"), reach);
check("a read says it only looks", reach.includes("looks only"), reach);
check("reachable actions name the tool they go through", reach.includes("do_household_action") && !reach.includes("not reachable"), reach);

const actionsOff = renderToString(h(HouseholdActions, { actions: calendarActions, gate: actionGate, gateOff: true }));
check("switching off do_household_action marks every action unreachable", actionsOff.includes("not reachable") && actionsOff.includes("switched off"), actionsOff);

const none = renderToString(h(HouseholdActions, { actions: [], gate: actionGate, gateOff: false }));
check("a Butler with no household actions draws no section", none === "", none);
check("it says why rather than offering a switch", goneTool.includes("has not wired the source") && !goneTool.includes("tri"), goneTool);

// A4's crossover rule, which the store also enforces with a trigger.
const priv = {
  seq: 1, name: "gym", adapter: "withings", ownership: "personal" as const, owner: "tom",
  audience: AUDIENCE_OWNER, actionsEnabled: false, indexingEnabled: true, cloudAllowed: false,
  state: "ready" as const, leaseEpoch: 1,
};
check("a private connection is withheld from a household Butler", withheldFrom("family", priv).includes("private to tom"));
check("an operator Butler may still be given it", withheldFrom("admin", priv) === "");
check("a household connection is offered to both", withheldFrom("family", { ...priv, audience: AUDIENCE_HOUSEHOLD }) === "");
// The vocabulary is open (slice 7): an audience nobody has seen before
// is narrower than the household, so it is withheld rather than
// offered. The old two-word check could not have caught this.
check("an audience the Console does not know is withheld, not offered",
  withheldFrom("family", { ...priv, audience: "aud_9f1c" }).includes("part of the household"));

// WHAT BREAKS: counted, and every zero omitted.
const lines = retireLines({
  people: ["tom", "sam", "kids"],
  routines: [{ name: "sunday shop", state: "enabled" }, { name: "meal plan", state: "enabled" }],
  sessions: 2, memoryCount: 3,
});
check("the retire list counts people", lines[0] === "3 people lose the Butler they message");
check("it names the routines rather than counting them alone", lines[1].includes("“sunday shop” and “meal plan”"), lines.join(" | "));
check("it says memory is kept, not deleted", lines.some((l) => l.includes("kept, not deleted")), lines.join(" | "));
const nothingBreaks = retireLines({ people: [], routines: [], sessions: 0, memoryCount: 0 });
check("nothing to break renders no list at all", nothingBreaks.length === 0, nothingBreaks.join(" | "));
const disabled = retireLines({ people: [], routines: [{ name: "old", state: "disabled" }], sessions: 0, memoryCount: 0 });
check("a routine somebody already disabled is not something this breaks", disabled.length === 0, disabled.join(" | "));

// The confirmations state a consequence and name what is kept.
const cloudConfirm = renderToString(h(Confirm, {
  kind: "warn", icon: null, title: "This sends health's material outside the house",
  body: "Everything anyone tells health from now on goes to anthropic.",
  listTitle: "WHAT CHANGES", lines: ["2 people talk to this Butler"],
  confirmLabel: "Yes, switch it", cancelLabel: "Keep llama3.1:8b",
  onConfirm: () => {}, onCancel: () => {},
}));
check("the crossing confirmation names the provider", cloudConfirm.includes("goes to anthropic"), cloudConfirm);
check("cancel names what is kept, not the word Cancel", cloudConfirm.includes("Keep llama3.1:8b") && !cloudConfirm.includes(">Cancel<"), cloudConfirm);

// The route lists put the boundary in a heading, not a suffix.
const local = renderToString(h(RouteList, { title: "ON YOUR HARDWARE", routes: [localModel(), QA], current: "llama3.1:8b", busy: false, onPick: () => {} }));
check("the selected route is the checked radio", local.includes('aria-checked="true"'), local);
check("a qa-only route says questions only", local.includes("questions only"), local);
const cloudList = renderToString(h(RouteList, { title: "OUTSIDE THE HOUSE", routes: [CLOUD, OFF], current: "claude-sonnet", busy: false, onPick: () => {} }));
check("a cloud route says it leaves the house", cloudList.includes("leaves the house"), cloudList);
check("a disabled route says where to turn it back on", cloudList.includes("turned off in Model routes"), cloudList);

// A Butler on a since-disabled route still renders as being on it.
const stranded = renderToString(h(RouteList, { title: "OUTSIDE THE HOUSE", routes: [CLOUD, OFF], current: "gpt-4o", busy: false, onPick: () => {} }));
check("a Butler stranded on a disabled route still shows that route as its own",
  stranded.split('aria-checked="true"').length === 2 && stranded.includes("gpt-4o"), stranded);

// The create dialog restates the whole choice as a sentence.
const dialog = renderToString(h(NewButlerDialog, {
  routes: [localModel(), CLOUD], existing: ["health"], say: () => {}, onClose: () => {}, onCreated: () => {},
}));
check("the create dialog summarises what is being made", dialog.includes("YOU ARE MAKING"), dialog);
check("it says nobody can talk to it yet", dialog.includes("Nobody can talk to it until you give it an address"), dialog);
check("it offers both surfaces as cards, not a select", dialog.includes("surface-card"), dialog);

// The tab strip.
const tabs = renderToString(h(Tabs, {
  label: "health detail", current: "model",
  items: [{ id: "identity", label: "Identity" }, { id: "model", label: "Model" }, { id: "tools", label: "Tools", count: "5/6" }, { id: "memory", label: "Memory", count: 0 }],
  onSelect: () => {},
}));
check("the open tab is the selected one", tabs.includes('aria-selected="true"'), tabs);
check("a tab count is shown when there is one", tabs.includes("5/6"), tabs);
check("a zero count is not rendered as a broken counter", !tabs.includes(">0<"), tabs);

// The two empty states.
const noMatch = renderToString(h(NoMatches, { filter: "attention", onClear: () => {} }));
check("no matches offers a way back", noMatch.includes("Clear filters"), noMatch);
const noneRetired = renderToString(h(NoMatches, { filter: "retired", onClear: () => {} }));
check("the retired filter says nothing has been retired", noneRetired.includes("No Butler has been retired"), noneRetired);
const noRoutes = renderToString(h(NoButlers, { routes: [], onNew: () => {} }));
check("with no model routes the empty state says a Butler needs a model", noRoutes.includes("A Butler needs a model to be anything"), noRoutes);
check("and the button is disabled until there is one", noRoutes.includes("disabled"), noRoutes);
const ready = renderToString(h(NoButlers, { routes: [localModel()], onNew: () => {} }));
check("with a route the empty state offers to make one", !ready.includes("A Butler needs a model to be anything"), ready);

// The screen itself, which must never render blank while it loads.
const screen = renderToString(h(Butlers));
check("the Butlers screen renders without throwing", screen.length > 0);
check("it states the model rule before any data arrives", screen.includes("sends what it is told outside the house"), screen.slice(0, 600));

// ------------------------------------------------ Butler selector
//
// The selector's job is to make "who am I talking to" unambiguous, so
// these checks are about what it refuses to blur.

const pickerRows = [
  chatRow(),
  chatRow({ name: "kitchen", directive: "timers and the freezer" }),
  chatRow({ name: "ops", surface: "admin", agentClass: "owner" }),
  chatRow({ name: "old", retiredAt: "2026-08-12T00:00:00Z" }),
];
const picker = renderToString(h(ButlerPicker, {
  rows: pickerRows, current: pickerRows[0], onPick: () => {}, onManage: () => {},
}));
check("the picker names the selected Butler", picker.includes("health"), picker.slice(0, 400));
check("and where its words go, without opening it", text(picker).includes("stays in the house"), text(picker));
// Closed, it is one Butler and not a list.
check("a shut picker lists nobody else", !picker.includes("kitchen"), picker.slice(0, 600));

// preferred(): never opens on a Butler that cannot answer.
check("it opens on a ready operator Butler",
  preferred(pickerRows)?.butler.name === "ops");
check("it skips one that cannot answer",
  preferred([chatRow({ name: "broken", surface: "admin" }, { reachable: false }), chatRow({ name: "fine" })])?.butler.name === "fine");
check("a retired Butler is never opened on",
  preferred([chatRow({ name: "old", retiredAt: "2026-08-12T00:00:00Z" }), chatRow({ name: "live" })])?.butler.name === "live");
check("with everything unwell it still opens on one", !!preferred([chatRow({}, { reachable: false })]));

// The filter reads the directive too, which is how you find the Butler
// whose job you remember and whose name you do not.
check("the filter matches a name", matches(chatRow(), "heal"));
check("the filter matches a directive", matches(chatRow({ name: "kitchen", directive: "timers and the freezer" }), "freezer"));
check("the filter rejects a miss", !matches(chatRow(), "nutrition"));

// openingFor(): derived from the record, never a table keyed by name.
const localOpen = openingFor(chatRow());
check("the opening names the Butler", localOpen.title.includes("health"));
check("it repeats the directive its owner wrote", localOpen.blurb.includes("keeps track of training and sleep"));
check("a household Butler states its ceiling", localOpen.blurb.includes("cannot touch the home lab"));
const opsOpen = openingFor(chatRow({ name: "ops", surface: "admin" }));
check("an operator Butler states a different one", opsOpen.blurb.includes("waits for your approval"));
check("the placeholder names the Butler", opsOpen.placeholder.includes("ops"));

// The blocked strip explains a Butler that cannot answer, and names the
// reason rather than saying "unavailable".
const blockedOff = renderToString(h(BlockedStrip, {
  row: chatRow({ modelEnabled: false }, { reachable: false }), onManage: () => {},
}));
check("a turned-off model says it is turned off", text(blockedOff).includes("qwen3.5:4b is turned off"), text(blockedOff));
check("and offers the way to fix it", blockedOff.includes("Change its model"), blockedOff);
const blockedDown = renderToString(h(BlockedStrip, {
  row: chatRow({}, { reachable: false }), onManage: () => {},
}));
check("a model that is not answering says that instead", text(blockedDown).includes("is not answering"), text(blockedDown));

// ------------------------------------------------ member editing
//
// The drawer's job is to be honest about what it can and cannot do.
const memberRow = {
  username: "giulia", displayName: "Giulia", email: "g@example.com",
  groups: ["media_users"], owner: "system", node: "wildstone-homelab",
};
const editing = renderToString(h(EditMember, {
  member: memberRow,
  groups: [
    { name: "media_users", owner: "capability/media", node: "wildstone-homelab" },
    { name: "console_users", owner: "capability/console", node: "wildstone-homelab" },
  ],
  onClose: () => {}, onDone: () => {},
}));
check("a group they are in is shown as already theirs", text(editing).includes("already in it"), text(editing));
// The additive rule, said where the control is rather than discovered
// from an app that still opens.
check("it says a group cannot be taken away here", text(editing).includes("never removes one"), text(editing));
check("a group they are already in cannot be unticked", editing.includes("disabled"), editing.slice(0, 900));
// A group declared on another node is not offered: a membership is only
// real on the directory the group is declared against.
const otherNode = renderToString(h(EditMember, {
  member: memberRow,
  groups: [{ name: "elsewhere_users", owner: "capability/x", node: "other-node" }],
  onClose: () => {}, onDone: () => {},
}));
check("a group on another directory is not offered", !otherNode.includes("elsewhere_users"), otherNode.slice(0, 600));
check("and it says why there is nothing to pick", text(otherNode).includes("No groups are declared"), text(otherNode));
// Passwords. The drawer must never look like it can set one — Wildstone
// does not hold it and cannot (LLDAP stores passwords through an OPAQUE
// exchange, ADR-W020 A4) — and must say where the code comes out, since
// there is no mail relay and a reset that silently writes to a file is
// indistinguishable from one that did nothing.
const withAuth = renderToString(h(EditMember, {
  member: memberRow,
  groups: [],
  authOrigin: "https://auth.example.ts.net:7443",
  onClose: () => {}, onDone: () => {},
}));
check("the reset points at the identity provider's own page",
  withAuth.includes("https://auth.example.ts.net:7443/reset-password/step1"), withAuth.slice(0, 900));
check("and says where the one-time code comes out",
  text(withAuth).includes("Identity"), text(withAuth));
// No password field, ever: the whole point is that this screen cannot
// set one, and an input here would promise what the directory refuses.
check("the drawer offers no password field",
  !withAuth.includes('type="password"'), withAuth.slice(0, 900));
// Without identity installed there is nowhere to send anybody, and
// rendering a dead link would be worse than saying so.
const noAuth = renderToString(h(EditMember, {
  member: memberRow, groups: [], onClose: () => {}, onDone: () => {},
}));
check("with no sign-in service it says so instead of linking nowhere",
  text(noAuth).includes("No sign-in service is installed") && !noAuth.includes("reset-password"),
  text(noAuth));
// ---------------------------------------------------------- Routines
//
// The list's job is two distinctions: what state a routine is in, and
// whether Wildstone runs it at all.

function routine(over: Partial<HouseholdRoutine>): HouseholdRoutine {
  return {
    name: "evening-mail", ownership: "wildstone", author: "sam", revision: 3,
    state: "trial", document: "kind: Automation",
    createdAt: "2026-09-01T00:00:00Z", updatedAt: "2026-09-07T00:00:00Z", updatedBy: "sam",
    ...over,
  };
}

const MAIL_SPEC = {
  description: "Ask whether the mail was collected",
  trigger: { eventType: "person.arrived_home" },
  action: { tool: "notifications.notify", tier: "domestic" },
} as unknown as AutomationSpec;

function routineList(
  routines: HouseholdRoutine[],
  specs: Record<string, AutomationSpec> = {},
  loading = false,
) {
  return renderToString(
    h(RoutineList, {
      load: { data: { routines, specs }, error: null, loading, reload: () => {} } as never,
      onMode: () => {},
    }),
  );
}

const ROUTINES: HouseholdRoutine[] = [
  routine({ name: "evening-mail", state: "trial" }),
  routine({ name: "morning-lights", state: "enabled" }),
  routine({ name: "bin-night", state: "draft" }),
  routine({ name: "old-thing", state: "disabled", disabledBy: "alex" }),
  routine({
    name: "ha-porch-light", ownership: "native", state: "enabled",
    externalId: "automation.porch", connection: "home-assistant",
  }),
];

const rlist = routineList(ROUTINES, { "evening-mail": MAIL_SPEC });
// Every state carries its meaning: the sentence is what tells a
// household what trial actually costs them.
for (const state of ["draft", "trial", "enabled", "disabled"] as const) {
  check(`the list explains ${state}`, text(rlist).includes(ROUTINE_MEANING[state]));
}
check("the ownership split is structural", rlist.includes("Run by Wildstone") && rlist.includes("Native to an app"));
check("a native routine says where it really lives", text(rlist).includes("runs inside home-assistant"));
// Offering to edit something Wildstone cannot is worse than not showing
// it, so a native card is not a button.
const nativeCard = rlist.slice(rlist.indexOf("Native to an app"));
check("a native routine does not open", !nativeCard.includes('role="button"'), nativeCard.slice(0, 200));
// The trigger and the tier come parsed from the server; the browser has
// no YAML parser and must not need one.
check("a parsed spec becomes chips", rlist.includes("person.arrived_home") && text(rlist).includes("tier: domestic"));
// A document the server could not parse costs its chips, not its row.
check("a routine with no parsed spec still renders", rlist.includes("morning-lights"));

const emptyList = routineList([]);
check("an empty list still says so", text(emptyList).includes("No routines yet."));
check("an empty list is not a failed one", !text(emptyList).includes("Nothing matches"));
const loadingList = routineList([], {}, true);
check("a list that has not loaded does not claim to be empty", !text(loadingList).includes("No routines yet."));

// ------------------------------------------------- routine occurrences
//
// The stream records a decision and its consequences as separate events
// sharing a correlation id. What a household asks is "what happened on
// Tuesday", which is the group, not the events.

function ev(over: Partial<HouseholdEvent>): HouseholdEvent {
  return {
    seq: 1, eventId: "e1", occurredAt: "2026-09-07T18:00:00Z",
    recordedAt: "2026-09-07T18:00:00Z", source: "system/automation",
    type: "automation.fired", entity: "mail-check", schemaVersion: 1,
    ...over,
  };
}

// A firing that went on to act is one row, not two.
const acted = occurrences([
  ev({ seq: 1, eventId: "a", correlationId: "c1", type: "automation.fired", subject: "sam" }),
  ev({ seq: 2, eventId: "b", correlationId: "c1", type: "automation.acted", subject: "sam" }),
]);
check("a firing and its outcome are one occurrence", acted.length === 1, JSON.stringify(acted));
check("an occurrence that acted says so", acted[0]?.acted === true && acted[0]?.outcome === "acted");

// A trial routine fires and does not act. That is the whole point of
// trial, and the table has to be able to show it.
const watched = occurrences([
  ev({ seq: 3, eventId: "c", correlationId: "c2", type: "automation.fired" }),
]);
check("a firing that never acted is not marked acted", watched[0]?.acted === false);
check("a firing with no action reads as fired", watched[0]?.outcome === "fired");

// A failure outranks everything else in the group.
const failed = occurrences([
  ev({ seq: 4, eventId: "d", correlationId: "c3", type: "automation.fired" }),
  ev({ seq: 5, eventId: "e", correlationId: "c3", type: "automation.action_failed" }),
]);
check("a failed action is a failed occurrence", failed[0]?.outcome === "failed" && failed[0]?.tone === "bad");

// A failed escalation is NOT a failed action. ADR-W024 A6: the Butler
// did not answer, so the rule acted with its author's wording. Calling
// that red reports a routine as broken on the one occasion the
// deterministic backbone did exactly its job.
const fellBack = occurrences([
  ev({ seq: 10, eventId: "j", correlationId: "c5", type: "automation.fired" }),
  ev({ seq: 11, eventId: "k", correlationId: "c5", type: "automation.escalation_failed" }),
  ev({ seq: 12, eventId: "l", correlationId: "c5", type: "automation.acted" }),
]);
check("a failed escalation that still acted is not a failure", fellBack[0]?.tone === "ok", fellBack[0]?.outcome);
check("the fallback wording is still reported", fellBack[0]?.outcome === "acted · own wording");
check("a fallback occurrence counts as acted", fellBack[0]?.acted === true);
// Skipped is the same story: no escalator configured, rule acts anyway.
const skipped = occurrences([
  ev({ seq: 13, eventId: "m", correlationId: "c6", type: "automation.escalation_skipped" }),
  ev({ seq: 14, eventId: "n", correlationId: "c6", type: "automation.acted" }),
]);
check("a skipped escalation that still acted is not a failure", skipped[0]?.tone === "ok", skipped[0]?.outcome);
// The Butler answered and its wording went out: no "own wording" note.
const escalated = occurrences([
  ev({ seq: 15, eventId: "o", correlationId: "c7", type: "automation.escalated" }),
  ev({ seq: 16, eventId: "p", correlationId: "c7", type: "automation.acted" }),
]);
check("an answered escalation reads as a plain act", escalated[0]?.outcome === "acted");

// Uncorrelated events are separate stories. Grouping them under the
// empty string would collapse a week into one row.
const loose = occurrences([
  ev({ seq: 6, eventId: "f", type: "automation.suppressed" }),
  ev({ seq: 7, eventId: "g", type: "automation.suppressed" }),
]);
check("events with no correlation id do not merge", loose.length === 2, JSON.stringify(loose));
check("a suppressed decision is not an event worth alarm", loose[0]?.tone === "mute");

// The earliest event in a group dates it: an occurrence happened when it
// started, not when its last consequence landed.
const ordered = occurrences([
  ev({ seq: 8, eventId: "h", correlationId: "c4", type: "automation.acted", occurredAt: "2026-09-07T18:00:05Z" }),
  ev({ seq: 9, eventId: "i", correlationId: "c4", type: "automation.fired", occurredAt: "2026-09-07T18:00:00Z" }),
]);
check("an occurrence is dated by its first event", ordered[0]?.at === "2026-09-07T18:00:00Z", ordered[0]?.at);

// ------------------------------------------------------ routine detail

const FULL_SPEC = {
  description: "Ask whether the mail was collected on arriving home",
  trigger: { eventType: "person.arrived_home" },
  conditions: [{ kind: "timeOfDay", after: "12:00" }, { kind: "cooldown", window: "20h" }],
  context: [{ kind: "recent", eventType: "pantry.meal_logged", limit: 3 }],
  escalation: { prompt: "Is anyone still out?", into: "family" },
  action: { tool: "notifications.notify", tier: "domestic" },
} as unknown as AutomationSpec;

const does = renderToString(h(WhatItDoes, { spec: FULL_SPEC, document: "" }));
// The schema's own field names. A household that reads "when" here and
// "trigger" in the document, the CLI and every engine error is reading
// two vocabularies for one thing.
for (const label of ["trigger", "conditions", "context", "escalation", "action"]) {
  check(`the spec is labelled ${label}`, text(does).includes(label));
}
check("a condition keeps its window", text(does).includes("window 20h"));
check("the action names its tier", text(does).includes("tier domestic"));
// The tier says how much authority the action has; the params say what
// actually gets sent. This screen is where someone presses "Let it act",
// so showing the first without the second asks for a decision on a
// summary.
const withParams = renderToString(
  h(WhatItDoes, {
    spec: {
      ...FULL_SPEC,
      action: {
        tool: "notifications.notify",
        tier: "domestic",
        params: { message: "Did you get the mail?", to: "sam" },
      },
    } as unknown as AutomationSpec,
    document: "",
  }),
);
check("the action shows what it will send", text(withParams).includes("Did you get the mail?"));
check("the action shows who it will reach", text(withParams).includes("sam"));

// A routine whose document will not parse is exactly the one someone
// needs to look at.
const broken = renderToString(
  h(WhatItDoes, { spec: null, specError: "parsing rule: bad yaml", document: "not: [yaml" }),
);
check("an unreadable spec explains itself", text(broken).includes("does not parse"));
check("an unreadable spec still shows the document", text(broken).includes("not: [yaml"));

// The acted column is what makes trial legible.
const trialRuns = renderToString(
  h(Runs, {
    runs: [ev({ seq: 1, eventId: "a", correlationId: "c1", type: "automation.fired", subject: "sam" })],
    routine: routine({ state: "trial" }),
  }),
);
check("a trial run says it did not act, and why", text(trialRuns).includes("no · trial"));
const liveRuns = renderToString(
  h(Runs, {
    runs: [
      ev({ seq: 1, eventId: "a", correlationId: "c1", type: "automation.fired", subject: "sam" }),
      ev({ seq: 2, eventId: "b", correlationId: "c1", type: "automation.acted", subject: "sam" }),
    ],
    routine: routine({ state: "enabled" }),
  }),
);
check("a run that acted says yes", text(liveRuns).includes(">yes<"));
// An event with no subject is a real event that cannot fire a
// per-member rule (ADR-W024 A8), not a blank cell.
const unmappedRun = renderToString(
  h(Runs, {
    runs: [ev({ seq: 1, eventId: "a", type: "automation.suppressed" })],
    routine: routine({ state: "enabled" }),
  }),
);
check("a run with no subject reads as unmapped", text(unmappedRun).includes("unmapped"));
// An empty table and a routine that has never run are different facts.
const noRuns = renderToString(h(Runs, { runs: [], routine: routine({ state: "trial" }) }));
check("no runs says nothing triggered it", text(noRuns).includes("Nothing has triggered it"));
const stoppedRuns = renderToString(h(Runs, { runs: [], routine: routine({ state: "disabled" }) }));
check("a stopped routine says it has not been running", text(stoppedRuns).includes("has not been running"));

const revs = renderToString(
  h(Revisions, {
    revisions: [
      { revision: 2, author: "alex", at: "2026-09-07T00:00:00Z", note: "narrowed the window" },
      { revision: 1, author: "sam", at: "2026-09-01T00:00:00Z", note: "created" },
    ],
  }),
);
check("a revision names who changed it and why", text(revs).includes("alex") && text(revs).includes("narrowed the window"));
const noRevs = renderToString(h(Revisions, { revisions: [] }));
check("no history says so rather than rendering nothing", text(noRevs).includes("No recorded history"));

// -------------------------------------------------------- routine edit
//
// Every control is a choice from a closed vocabulary. A form that offers
// an event nothing publishes, or a tier policy refuses, is offering a
// rejection.

const GRAMMAR: AutomationGrammar = {
  eventTypes: [
    { type: "person.arrived_home", package: "home-automation", description: "Someone got home." },
    { type: "person.left_home", package: "home-automation" },
  ],
  actions: [
    { tool: "notifications.notify", tier: "domestic", package: "notifications" },
    { tool: "notifications.peek", tier: "read", package: "notifications" },
  ],
  conditionKinds: ["cooldown", "timeOfDay"],
  contextKinds: ["recentEvents"],
  tiers: ["domestic", "read"],
  weekdays: ["mon", "tue", "wed", "thu", "fri", "sat", "sun"],
};

function detailFor(over: Partial<HouseholdRoutine>, spec: AutomationSpec | null = MAIL_SPEC): RoutineDetail {
  return {
    routine: routine(over),
    rule: "mail-check",
    spec,
    revisions: [{ revision: 1, author: "sam", at: "2026-09-01T00:00:00Z", note: "created" }],
    runs: [],
  };
}

function editForm(over: Partial<HouseholdRoutine>, spec: AutomationSpec | null = MAIL_SPEC) {
  return renderToString(
    h(EditForm, {
      name: "evening-mail",
      detail: detailFor(over, spec),
      grammar: GRAMMAR,
      onMode: () => {},
      onSaved: () => {},
    }),
  );
}

const edit = editForm({ state: "trial" });
check("the trigger offers what packages publish", edit.includes("person.left_home"));
check("an event's description helps the choice", text(edit).includes("Someone got home."));
// A tool's declared risk IS the tier, so the form shows them together
// rather than letting someone invent a combination policy refuses.
check("an action is offered with its tier", text(edit).includes("notifications.notify · tier domestic"));
// The only free-text input is the change note. A trigger or a tool typed
// by hand is a rule that never fires or an action policy refuses.
check("the only typed field is the note", (edit.match(/type="text"/g) ?? []).length === 1);

// A rule pointing at something no installed package provides keeps its
// value visible. Silently re-pointing it at a different event would
// change what the routine does without anyone asking.
const orphaned = editForm(
  { state: "trial" },
  { ...MAIL_SPEC, trigger: { eventType: "pantry.meal_logged" } } as unknown as AutomationSpec,
);
check("an unpublished trigger is still shown", text(orphaned).includes("pantry.meal_logged"));
check("and it says nothing publishes it", text(orphaned).includes("nothing installed publishes this"));

// The warning that matters: editing a routine that is ACTING takes it
// out of action, and the form has to say so before the save rather than
// after. There is no approval step on this path, so this sentence is
// what keeps ADR-W024's "watched before it acts".
check("nothing is said before anything changes", changeWarning(false, true, 3) === null);
const actingWarning = changeWarning(true, true, 3) ?? "";
check("a change names the revision it becomes", actingWarning.includes("rev 4"));
check("an acting routine is told it goes back to trial", actingWarning.includes("back in trial"));
check("and what that costs it", actingWarning.includes("watch and report"));
// A routine that is not acting has nothing taken away, so it is not
// warned about something that will not happen.
const quietWarning = changeWarning(true, false, 3) ?? "";
check("a routine that is not acting is not warned about trial", !quietWarning.includes("trial"));
check("but still learns the revision moves", quietWarning.includes("rev 4"));

// A document that will not parse cannot be edited as a form, and the
// screen says which screen to use instead.
const unparseable = editForm({ state: "trial" }, null);
check("an unparseable document refuses the form", text(unparseable).includes("does not parse"));
check("and points somewhere useful", text(unparseable).includes("detail screen"));

// ------------------------------------------------------ routine dry-run
//
// Two replays, diffed. The summary is computed, never editorialised.

function dec(over: Partial<ReplayDecision>): ReplayDecision {
  return {
    rule: "mail-check", eventSeq: 1, eventId: "e1", eventType: "person.arrived_home",
    at: "2026-09-07T18:00:00Z", fired: true, reason: "all conditions held",
    ...over,
  };
}

function replay(decisions: ReplayDecision[]): ReplayResult {
  return {
    rule: "mail-check", from: "2026-09-01T00:00:00Z", to: "2026-09-07T00:00:00Z",
    draft: false, enabled: true, fired: decisions.filter((d) => d.fired).length,
    evaluated: decisions.length, decisions,
  };
}

// Identical replays: the summary must not read as a bug.
const same = diffReplays(
  replay([dec({ eventId: "a" }), dec({ eventId: "b", fired: false })]),
  replay([dec({ eventId: "a" }), dec({ eventId: "b", fired: false })]),
);
check("identical replays produce no differences", same.every((r) => !r.changed));
check("nothing differing says so in words", summarise(same).startsWith("Nothing differs."));
check("and never as a count of zero", !summarise(same).includes("0 of"));

// A window that held nothing is not agreement.
check(
  "an empty window says there was nothing to compare",
  summarise(diffReplays(replay([]), replay([]))).includes("nothing to compare"),
);

// A case the draft no longer catches, counted.
const lost = diffReplays(
  replay([dec({ eventId: "a" }), dec({ eventId: "b" })]),
  replay([dec({ eventId: "a" }), dec({ eventId: "b", fired: false })]),
);
check("a changed verdict is a difference", lost.filter((r) => r.changed).length === 1);
check("the summary counts what changed", summarise(lost).startsWith("1 of 2 differ."));
check("and says which direction", summarise(lost).includes("no longer catches"));

const gained = diffReplays(
  replay([dec({ eventId: "a", fired: false })]),
  replay([dec({ eventId: "a" })]),
);
check("a newly caught case is counted too", summarise(gained).includes("newly catches"));

// A changed trigger means the two rules are not asked the same
// questions. An event only one of them ever saw is a difference.
const moved = diffReplays(
  replay([dec({ eventId: "a" })]),
  replay([dec({ eventId: "z", eventType: "person.left_home" })]),
);
check("an event only one version saw is a difference", moved.length === 2 && moved.every((r) => r.changed));
check("a row missing a side keeps the other", moved.some((r) => r.current && !r.draft));

// With no current revision there is still a draft column.
const draftOnly = diffReplays(null, replay([dec({ eventId: "a" })]));
check("a draft with no running revision still has rows", draftOnly.length === 1);
check("and the missing side is absent, not false", draftOnly[0]?.current === undefined);

// Newest first, like every other table on this screen.
const replayOrder = diffReplays(
  null,
  replay([
    dec({ eventId: "old", at: "2026-09-01T00:00:00Z" }),
    dec({ eventId: "new", at: "2026-09-07T00:00:00Z" }),
  ]),
);
check("replay rows are newest first", replayOrder[0]?.eventId === "new");

// --------------------------------------------------- household stream

// The household stream's verbs, which the operational classifier had
// never seen. The failure suffix is the one that matters: the engine
// writes automation.action_failed with an UNDERSCORE, and the rule only
// matched a hyphen, so a routine whose action failed rendered grey —
// the one row on this screen that has to be red.
check("a failed action is a failure", eventKind("automation.action_failed") === "bad");
check("a failed escalation is a failure", eventKind("automation.escalation_failed") === "bad");
check("the hyphenated operational form still works", eventKind("secret.rotate-failed") === "bad");
check("a rule that acted went well", eventKind("automation.acted") === "ok");
check("an answered question went well", eventKind("chat.answered") === "ok");
check("a pending escalation wants a person", eventKind("automation.escalation_pending") === "warn");
// A rule deciding there is nothing to do is the common case, not an
// event worth colouring.
check("a suppressed decision is quiet", eventKind("automation.suppressed") === "mute");

function hev(over: Partial<HouseholdEvent>): HouseholdEvent {
  return {
    seq: 1, eventId: "h1", occurredAt: "2026-09-07T18:00:00Z",
    recordedAt: "2026-09-07T18:00:01Z", source: "home-automation",
    type: "person.arrived_home", subject: "sam", schemaVersion: 1,
    ...over,
  };
}

const HOUSEHOLD: HouseholdEvent[] = [
  hev({ seq: 1, eventId: "a", subject: "sam" }),
  hev({ seq: 2, eventId: "b", subject: undefined, type: "person.left_home" }),
  hev({ seq: 3, eventId: "c", type: "automation.fired", source: "system/automation", entity: "mail-check" }),
  hev({ seq: 4, eventId: "d", type: "pantry.meal_logged", source: "pantry", subject: "alex" }),
];

const everything = matchingEvents(HOUSEHOLD, { filter: "all", subject: "", type: "", source: "" });
check("the household stream is newest first", everything[0]?.eventId === "d");
check("nothing is dropped by default", everything.length === 4);

// An unmapped subject is a real event that cannot fire a per-member rule
// (A8). Hiding it hides the answer to "why did my routine never run".
const unmappedRows = matchingEvents(HOUSEHOLD, { filter: "unmapped", subject: "", type: "", source: "" });
check("unmapped subjects have a filter of their own", unmappedRows.length === 1 && unmappedRows[0].eventId === "b");
const byPerson = matchingEvents(HOUSEHOLD, { filter: "person", subject: "", type: "", source: "" });
check("by person excludes the ones with no person", byPerson.every((e) => !!e.subject));
check("and keeps the rest", byPerson.length === 3);

const routines = matchingEvents(HOUSEHOLD, { filter: "routines", subject: "", type: "", source: "" });
check("routine outcomes are the automation verbs", routines.length === 1 && routines[0].eventId === "c");
const bySource = matchingEvents(HOUSEHOLD, { filter: "source", subject: "", type: "", source: "pantry" });
check("by source narrows to one publisher", bySource.length === 1 && bySource[0].eventId === "d");

const stream = renderToString(
  h(HouseholdStreamView, { events: HOUSEHOLD, error: null, loading: false, now: new Date("2026-09-07T18:05:00Z") }),
);
check("an event with no subject renders unmapped", text(stream).includes("unmapped"));
// The household table carries a source column — who published this —
// which the operational one has no equivalent for.
check("the household table names the publisher", text(stream).includes("home-automation"));
// The payload is untrusted input and does not belong in a table cell.
check("payloads are not rendered in the table", !stream.includes("payload"));

const emptyStream = renderToString(
  h(HouseholdStreamView, { events: [], error: null, loading: false }),
);
check("an empty household stream says so", text(emptyStream).includes("Nothing has been published"));
const loadingStream = renderToString(
  h(HouseholdStreamView, { events: [], error: null, loading: true }),
);
check("a stream still loading does not claim to be empty", !text(loadingStream).includes("Nothing has been published"));

// ------------------------------------------- correlation and envelope

const STORY: HouseholdEvent[] = [
  hev({ seq: 10, eventId: "s1", correlationId: "c1", type: "person.arrived_home" }),
  hev({ seq: 11, eventId: "s2", correlationId: "c1", type: "automation.fired", source: "system/automation", entity: "mail-check" }),
  hev({ seq: 12, eventId: "s3", correlationId: "c1", type: "automation.acted", source: "system/automation", entity: "mail-check" }),
  hev({ seq: 13, eventId: "s4", type: "person.left_home" }),
  hev({ seq: 14, eventId: "s5", type: "pantry.meal_logged" }),
];

// Selecting a row lights up its story and nothing else.
check("a correlated row is highlighted", correlated(STORY[1], STORY[0]));
check("an unrelated row is not", !correlated(STORY[3], STORY[0]));
// Grouping on the empty string would light up every uncorrelated event
// on the stream at once — the exact opposite of the point.
check("rows with no correlation id do not correlate", !correlated(STORY[4], STORY[3]));
check("and selecting nothing highlights nothing", !correlated(STORY[0], null));

// What an event set off: the engine's records, in order.
const chain = setOff(STORY[0], STORY);
check("the chain is what followed", chain.length === 2);
check("the chain is in order", chain[0]?.eventId === "s2" && chain[1]?.eventId === "s3");
check("the event itself is not in its own chain", !chain.some((e) => e.eventId === "s1"));
check("an uncorrelated event set nothing off", setOff(STORY[3], STORY).length === 0);

check("the chain names the routine to open", routineOf(chain) === "mail-check");
// Nothing to point at means no button rather than a button pointing
// nowhere.
check("a chain naming no routine offers none", routineOf([hev({ entity: undefined })]) === undefined);

// The envelope drawer, rendered directly: under renderToString the
// drawer is closed, so asserting against the table would assert nothing.
const envelope = renderToString(
  h(Envelope, { event: STORY[0], events: STORY, onOpenRoutine: () => {} }),
);
// Both timestamps, both labelled. When a publisher batches or retries
// they differ, and that difference is why something fired late.
check("the envelope dates when it happened", envelope.includes("occurred_at"));
check("and when we heard about it", envelope.includes("recorded_at"));
for (const field of ["event_id", "subject", "source", "correlation", "schema"]) {
  check(`the envelope carries ${field}`, envelope.includes(field));
}
// ADR-W024 A4, and not decoration: the screen that shows payloads is
// where a household learns what they are.
check("the untrusted-input warning is present", text(envelope).includes("Payload text is untrusted input"));
check("the source is described as stamped, not claimed", text(envelope).includes("never read off the payload"));
check("the chain is shown", text(envelope).includes("automation.acted"));
check("and the routine can be opened", text(envelope).includes("Open routine"));

// A payload is JSON from outside the house. It renders as text: no
// markup, no auto-linking. A payload that renders as a clickable link is
// a phishing surface on the one screen whose job is to say otherwise.
const hostile = renderToString(
  h(Envelope, {
    event: hev({
      seq: 20, eventId: "x1",
      payload: { note: "<script>alert(1)</script>", link: "http://evil.example/steal" },
    }),
    events: [],
    onOpenRoutine: () => {},
  }),
);
check("a payload's markup is escaped", !hostile.includes("<script>alert(1)</script>"));
check("but its text is still shown", hostile.includes("&lt;script&gt;"));
check("a payload's url is not a link", !hostile.includes('href="http://evil.example/steal"'));
check("but the url is still readable", text(hostile).includes("http://evil.example/steal"));
// Nothing followed, so nothing is claimed to have.
check("an event that set nothing off says so", text(hostile).includes("Nothing followed"));
check("and offers no routine to open", !text(hostile).includes("Open routine"));

// ------------------------------------------------------ keyboard reach
//
// Asserted against the rendered markup, which is where these properties
// actually live. A control that answers only to a click is a control a
// keyboard cannot reach.

// Every column of every new table is scoped. A five-column stream with
// no scopes reads to a screen reader as a wall of cells.
for (const [label, html] of [
  ["the household stream", stream],
  ["the runs table", trialRuns],
] as const) {
  // `<th[ >]` rather than `<th`, which also matches <thead>.
  const headers = (html.match(/<th[ >]/g) ?? []).length;
  const scoped = (html.match(/<th scope="col"/g) ?? []).length;
  check(`${label} scopes every header`, headers > 0 && headers === scoped, `${scoped}/${headers}`);
}

// A card that opens something is reachable and operable.
check("a routine card is in the tab order", rlist.includes('role="button"') && rlist.includes('tabindex="0"'));

// A stream row selects with the keyboard, and says whether it is the
// selected one.
check("a stream row is in the tab order", stream.includes('tabindex="0"'));
check("a stream row reports its selection", stream.includes('aria-pressed='));

// The tab bar is a real tablist: one tab in the tab order, the arrow
// keys moving within the set.
const activityTabs = renderToString(
  h(ConsoleStateContext.Provider, {
    value: { error: null, snap: null },
  }, h(Activity, {})),
);
check("the activity tabs are a tablist", activityTabs.includes('role="tablist"'));
check("each tab reports selection", (activityTabs.match(/aria-selected/g) ?? []).length === 2);
// Roving tabindex: exactly one tab is tabbable.
check(
  "only the selected tab is in the tab order",
  (activityTabs.match(/tabindex="0"/g) ?? []).length === 1,
);
check("and the other is not", activityTabs.includes('tabindex="-1"'));

// ---- Entitlements (ADR-W033 design §4, §18.2; slice 8).
//
// The load-bearing check is the empty one. A service with no decision
// on record reaches NOBODY, and a screen that said nothing at all there
// would read as "everyone" — which is the opposite of what the resolver
// does and the opposite of what is safe.
import { ServiceAccessView } from "../src/views/ServiceAccess";
import { PersonGroupsView } from "../src/views/PersonGroups";
import type { PersonGroup, ServiceAccess } from "../src/api";
import { SecretInput } from "../src/ui/SecretInput";

const groupsFixture: PersonGroup[] = [
  { id: "grp_1", name: "Family", seedKey: "family", members: ["tom", "sam"] },
  { id: "grp_2", name: "Adults", seedKey: "adults", members: ["tom"] },
  { id: "grp_3", name: "Media", members: [] },
];

const undecided: ServiceAccess = {
  capability: "photos", groups: [], people: [], reaches: [],
};
const undecidedOut = renderToString(
  h(ServiceAccessView, { access: undecided, groups: groupsFixture, onChanged: () => {} }),
);
check("a service nobody decided about says it reaches nobody",
  undecidedOut.includes("Nobody"), undecidedOut);
check("and says that is the safe way round rather than a fault",
  undecidedOut.includes("waiting for you rather than broken"));
check("it offers a group to allow", undecidedOut.includes("Let a group use this"));

const decided: ServiceAccess = {
  capability: "photos",
  declared: ["family"],
  groups: [{ id: "grp_1", name: "Family", effect: "allow" }],
  people: [{ id: "psn_b", name: "sam", effect: "restrict" }],
  reaches: ["tom"],
};
const decidedOut = renderToString(
  h(ServiceAccessView, { access: decided, groups: groupsFixture, onChanged: () => {} }),
);
// The ANSWER comes before the rules: who it reaches is what an Owner
// needs, and "Adults may use this" still leaves them working out who
// that is today.
check("the resolved answer is shown", decidedOut.includes("tom"));
check("an allowed group reads as may use", decidedOut.includes("may use"));
check("a restricted person reads as refused", decidedOut.includes("refused"));
check("the answer appears before the rules",
  decidedOut.indexOf("Who may use this") < decidedOut.indexOf("Rules"));
check("the declared audience is named as where the rules came from",
  decidedOut.includes("This service says it is for"));
// Both verbs are offered, because removing a rule and refusing somebody
// are different things and a household must be able to say either.
check("a rule can be removed", decidedOut.includes("Remove"));
check("a rule can be flipped", decidedOut.includes("Refuse"));

const dormant = renderToString(
  h(ServiceAccessView, {
    access: { ...decided, dormant: true }, groups: groupsFixture, onChanged: () => {},
  }),
);
check("an uninstalled service says its rules are kept",
  dormant.includes("not installed") && dormant.includes("come back if you install it again"));

// Groups grant nothing by themselves, and the screen has to say so: a
// list of groups with toggles looks exactly like a permissions screen.
const groupsOut = renderToString(
  h(PersonGroupsView, { groups: groupsFixture, members: ["tom", "sam"], onChanged: () => {} }),
);
check("the groups screen says membership grants nothing",
  groupsOut.includes("grants nothing on its own"), groupsOut);
check("it points at where access IS decided", groupsOut.includes("under Services"));
check("a seeded group shows the role its rules reference",
  groupsOut.includes("role: adults"));
check("a household's own group shows no role",
  !groupsOut.includes("role: media"));
check("membership is a toggle per person", groupsOut.includes('aria-pressed="true"'));

// A secret field starts concealed, with a named Show control beside it,
// so a person can check what they typed before it is stored — and the
// field itself never carries spell-checking, which can send visible text
// to a spelling service.
const secretOut = renderToString(h(SecretInput, { "aria-label": "secret value", value: "typed", onChange: () => {} }));
check("a secret field starts concealed", secretOut.includes('type="password"') && !secretOut.includes('type="text"'));
check("a secret field has a named Show control", secretOut.includes('aria-label="Show secret value"') &&
  secretOut.includes('aria-pressed="false"') && secretOut.includes('type="button"'));
check("a secret field is never spell-checked", secretOut.includes('spellCheck="false"') || secretOut.includes('spellcheck="false"'));

console.log(failures === 0 ? "\nALL CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
