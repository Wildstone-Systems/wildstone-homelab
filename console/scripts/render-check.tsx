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
import { Chat, IssuedCode, Prerequisites } from "../src/views/Chat";
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

// An invite needs a person to invite and a bot to send the code to. An
// empty dropdown is not an answer, so the screen says which is missing
// and where it is fixed.
const missing = renderToString(
  h(Prerequisites, {
    surface: "family", usable: [], chosen: undefined, allTransports: [
      { transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: false, credentialed: false },
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
    usable: [{ transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: true, credentialed: false }],
    chosen: { transport: "telegram", surface: "family", capability: "chat-telegram-family", installed: true, credentialed: false },
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
