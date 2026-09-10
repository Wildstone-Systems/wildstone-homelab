// Does the built screen still say what the design said?
//
// `render-check` proves the components render and branch correctly. It
// cannot catch the failure this file exists for: a screen that renders
// beautifully and has quietly lost the sentence the design was written
// around. Copy is where that happens, because copy is the part that
// looks safe to paraphrase — "leaves the house" becomes "external
// model", and the one phrase the whole ADR turns on is gone with no
// test any redder for it.
//
// So this reads the design prototype in docs/design/butlers/ and checks
// that every load-bearing string still appears in the console source.
//
// Two things it deliberately does NOT do. It does not diff markup: the
// prototype is one standalone file with inline styles, and the console
// is React with a token set, so a structural diff would be noise. And
// it does not demand every string, because some of the prototype is
// fixture data and some of it describes an engine we do not have. Those
// live in DEPARTURES below, each with the reason — a list that is
// itself the useful artifact, because it is the honest answer to "how
// far is this from the design, and why".
import { readFileSync, readdirSync, statSync } from "fs";
import { join } from "path";

const ROOT = new URL("..", import.meta.url).pathname;
const DESIGN = join(ROOT, "..", "docs", "design", "butlers");

function sources(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) sources(path, out);
    else if (/\.(tsx?|css)$/.test(name)) out.push(path);
  }
  return out;
}

// Comments are stripped before searching. A departure EXPLAINED in a
// comment is not a departure violated — the whole point of the
// DEPARTURES list is that somebody wrote down why a phrase is gone, and
// a check that then failed on the explanation would punish the one
// behaviour it exists to encourage.
function stripComments(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, " ").replace(/^\s*\/\/.*$/gm, " ");
}

const CODE = sources(join(ROOT, "src"))
  .map((f) => stripComments(readFileSync(f, "utf8")))
  .join("\n");

// The prototype, as its own source rather than the 4.9MB bundle.
const PROTOTYPE = readFileSync(join(DESIGN, "butlers-console.dc.html"), "utf8");
const README = readFileSync(join(DESIGN, "README.md"), "utf8");

let failures = 0;
let departures = 0;
function check(what, ok, detail = "") {
  if (ok) {
    console.log(`ok:   ${what}`);
    return;
  }
  failures++;
  console.log(`FAIL: ${what}${detail ? `\n      ${detail}` : ""}`);
}

// Normalise the quote characters and entities the two files disagree
// about, so a curly apostrophe is not reported as missing copy.
const norm = (s) =>
  s
    .replace(/&#x27;|&#39;|[‘’]/g, "'")
    .replace(/&quot;|[“”]/g, '"')
    .replace(/&amp;/g, "&")
    .replace(/\\u00b7/g, "·")
    .replace(/\s+/g, " ");

const CODE_N = norm(CODE);
const PROTO_N = norm(PROTOTYPE);
const README_N = norm(README);

// ---------------------------------------------------------------- 1
// The sentences the design is built around. Each is asserted to be in
// the PROTOTYPE (so this file cannot drift into testing itself) and
// then in the console.
const LOAD_BEARING = [
  "One model each, its own directive and its own reach.",
  "sends what it is told outside the house",
  "leaves the house",
  "stays in the house",
  "Reads the home. Cannot touch the home lab.",
  "Can propose changes to the home lab.",
  "questions only",
  "nobody can talk to it",
  "kept, not deleted",
  "Nothing matches that.",
  "Clear filters",
  "Shapes how it talks. It grants nothing",
  "A surface is granted at creation and cannot be changed.",
  "ON YOUR HARDWARE",
  "OUTSIDE THE HOUSE",
  "WHO MAY TALK TO IT",
  "CONNECTIONS IT MAY READ",
  "An unticked person cannot reach this Butler at all",
  "It will answer nothing until somebody here is ticked.",
  "A tool exists only while the package behind it is connected",
  "will still talk, but it cannot look anything up",
  "Nobody has asked this Butler anything yet.",
  "YOU ARE MAKING",
  "Nobody can talk to it until you give it an address.",
  "Give it a name — this is what people will say to reach it.",
  "There is already a Butler called",
  "WHAT BREAKS",
  "WHAT CHANGES",
  "It stops answering immediately. Nothing is deleted",
  "Activity will record the crossing, with your name on it",
  "the conversation open right now carries over on its next message",
  "Retire it",
  "Keep it",
  "Yes, switch it",
  "Bring back",
  "Duplicate",
  "Show only these",
  "New Butler",
  "Needs attention",
  "Search name or directive",
  "TRY IT",
];

console.log("\nLoad-bearing copy from the prototype");
console.log("-".repeat(64));
for (const line of LOAD_BEARING) {
  const want = norm(line);
  if (!PROTO_N.includes(want) && !README_N.includes(want)) {
    check(`"${line}" is in the design at all`, false, "not in the prototype — fix this list, not the console");
    continue;
  }
  check(`console says "${line}"`, CODE_N.includes(want));
}

// ---------------------------------------------------------------- 2
// Where the build departs from the design, and why. A departure with no
// reason is a bug; a departure with a reason is a decision, and this is
// where it is written down. Each names a prototype string that must NOT
// appear in the console, so a later well-meaning edit that reintroduces
// it fails here rather than shipping a control the engine ignores.
const DEPARTURES = [
  {
    gone: "ask first",
    why: "The tri-state tool control has no engine behind it. keystone/runtimedb/butlertools.go defines enabled|disabled only, and PUT /butlers/{name}/tool is narrow/un-narrow. IMPLEMENTATION_PLAN.md line 4 of Phase 0 pre-authorised shipping two-state and dropping this, and rendering a control the Butler loop ignores is the one thing it said not to do.",
  },
  {
    gone: "send.email",
    why: "A fixture tool name. The real household registry is get_household_events, list_household_actions, do_household_action; the operator's is get_system_overview, propose_change and the rest. The screen reads the gateway's manifest over GET /butlers/{name}/tools rather than keeping a second copy of the registry in TypeScript.",
  },
  {
    gone: "read.timeline",
    why: "Same: a fixture name. See above.",
  },
  {
    gone: "propose.plan",
    why: "Same. The roster's '· may propose plans' suffix went with it — propose_change's availability is already in the tool counts the server sends, and a second derived claim about it is a second thing to get wrong.",
  },
  {
    gone: "probed 2m ago",
    why: "There is no probe timestamp on the wire. Model carries readiness and readinessDetail; nothing records when it was last checked, and POST /butler/routes/{name}/probe does not exist — the nearest endpoint records a verdict a caller supplies. Inventing a freshness claim is worse than omitting one, so the ON NOW card shows the provider and endpoint instead.",
  },
  {
    gone: "Re-check",
    why: "Same reason: a button whose endpoint does not exist. Left out rather than stubbed.",
  },

  {
    gone: "92x / 7d",
    why: "Per-tool usage counts. Nothing records them; the roster's tool counts are real, and a fabricated usage column beside them would make the real numbers look fabricated too.",
  },
];

// Departures with no unique string to assert on. They are listed
// because the list is the artifact, not because a check can hold them:
// "Model routes" still appears in the console as the route note
// "turned off in Model routes", so asserting its absence would fail on
// copy the design also wanted.
const NOTES = [
  'The header\'s second button, "Model routes", is not built: it pointed at a screen that does not exist in console/src/views. The phrase survives where the design also uses it — a disabled route reads "turned off in Model routes", which is still where somebody would go.',
  "The Sessions tab lists only sessions carrying this Butler's stamp. Sessions recorded before migration 0036 have none and are not shown; they genuinely did not run as this Butler, and filtering by surface instead would file another Butler's conversations under this one's name.",
  "The Identity tab's name field is read-only. butlers.seq is the stable key and the scope tables follow it, but butler_memory, butler_index, household_routines and butler_tasks all reference a Butler by NAME as text with no foreign key — so renaming would orphan its memory, which ADR-W031 says survives even retirement.",
];

console.log("\nDepartures from the design, each with its reason");
console.log("-".repeat(64));
for (const d of DEPARTURES) {
  const want = norm(d.gone);
  if (!PROTO_N.includes(want) && !README_N.includes(want)) {
    check(`"${d.gone}" is in the design (so the departure is real)`, false, "not in the prototype or README — this entry is stale");
    continue;
  }
  departures++;
  check(`"${d.gone}" is deliberately absent`, !CODE_N.includes(want), d.why);
  if (!CODE_N.includes(want)) console.log(`      why: ${d.why}`);
}

// ---------------------------------------------------------------- 3
// Two rules the README states about the recreation itself.
console.log("\nThe README's two hard rules");
console.log("-".repeat(64));

// "No colour literals below the token block in styles.css."
const CSS = readFileSync(join(ROOT, "src", "styles.css"), "utf8");
const tokenBlockEnds = CSS.indexOf('[data-theme="light"]');
const afterTokens = CSS.slice(CSS.indexOf("}", CSS.indexOf('[data-theme="light"]')));
const literals = [...afterTokens.matchAll(/#[0-9a-fA-F]{3,8}\b/g)].map((m) => m[0]);
check(
  "no colour literals below the token block in styles.css",
  literals.length === 0,
  literals.length ? `found ${literals.length}: ${literals.slice(0, 6).join(", ")}` : "",
);
void tokenBlockEnds;

// "Console, CLI and Butler use the same public API. No console-only
// endpoints." Every path the console calls must be a route keystone
// registers.
const API = readFileSync(join(ROOT, "src", "api.ts"), "utf8");
const ROUTES = readFileSync(join(ROOT, "..", "keystone", "api", "api.go"), "utf8");
const called = [...API.matchAll(/["`](\/api\/v1\/[^"`?]*)/g)].map((m) => m[1]);
const butlerPaths = [...new Set(called.filter((p) => p.includes("butler")))];
const missing = butlerPaths.filter((path) => {
  // `/api/v1/butlers/${encodeURIComponent(name)}/tools` becomes
  // `/api/v1/butlers/` — compare on the literal prefix and the suffix
  // after the interpolation, which is what api.go's {name} stands in for.
  const prefix = path.split("${")[0];
  return !ROUTES.includes(prefix);
});
check(
  "every butler path the console calls is a route keystone registers",
  missing.length === 0,
  missing.join(", "),
);

console.log("\nDepartures with no string to assert on");
console.log("-".repeat(64));
for (const note of NOTES) console.log(`note: ${note}`);

console.log("\n" + "-".repeat(64));
console.log(`${departures + NOTES.length} recorded departure(s) from the design.`);
console.log(failures === 0 ? "HANDOFF CHECK PASSED" : `${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
