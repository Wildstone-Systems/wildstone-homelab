// End-to-end check of the Console in a real browser.
//
// The render check (render-check.tsx) proves the components render;
// this proves the SCREEN works — Owner sign-in, navigation, the catalog
// table, the generated install form, and the preview that names the
// secrets only an operator can supply.
//
// Needs a browser AND a running Keystone with a real catalog behind the
// household's identity provider, which is why it is not part of
// `npm run build` or CI: there is no house in CI to point it at. It is
// an operator's check, run against their own control plane, as an Owner:
//
//   npx playwright install chromium     # once, for the browser itself
//   WILD_CONSOLE=https://console.your.house/ WILD_USER=you npm run ui-check
//
// `playwright` is a declared devDependency, so `npm install` is enough
// to get the library; the browser binary is a separate download and the
// preflight below says so when it is missing.
//
// It signs in the way an Owner does (ADR-W033 A4, A4.1): through the
// household address, round the identity provider's OpenID Connect
// sign-in, with the Owner's own username and password — read from
// WILD_PASSWORD or ~/.wild_owner_password. There is no token path into
// the Console to use instead: bearer tokens are for `wild`, the
// installer and the host. It stops at Preview and never clicks "Propose
// plan": a check that installs a capability every time it runs is not a
// check.
import { chromium } from "playwright";
import { readFileSync } from "fs";

// Whose deployment this runs against is the operator's to say, not this
// script's: a hardcoded home directory and LAN address only ever matched
// one machine, and put that machine's details in a public repository.
const PASSWORD_FILE = process.env.WILD_PASSWORD_FILE
  ?? `${process.env.HOME ?? "."}/.wild_owner_password`;
const BASE = process.env.WILD_CONSOLE ?? "";
const USER = process.env.WILD_USER ?? "";

// Two different exits, because they are two different problems. A check
// that failed (1) is a regression worth reading the diff over; a check
// that could not run at all (2) is a missing browser or a control plane
// that is not up, and reporting that as a test failure sends whoever
// reads it looking for a bug that is not there.
const EXIT_FAILED = 1;
const EXIT_CANNOT_RUN = 2;

// Thrown rather than exited, so that a setup problem found after the
// browser is open still unwinds through the cleanup below. `process.exit`
// does not run pending finally blocks, and a headless chromium left
// behind by every aborted run is how a check eats a machine.
class CannotRun extends Error {
  constructor(what, remedy) {
    super(what);
    this.remedy = remedy;
  }
}
const cannotRun = (what, remedy) => {
  throw new CannotRun(what, remedy);
};

let failures = 0;
const check = (label, cond, detail = "") => {
  if (!cond) { failures++; console.log("FAIL:", label, detail); }
  else console.log("ok:", label);
};

// What the script was doing when something threw. A driven browser fails
// by timing out on a locator, and "Timeout 30000ms exceeded" on its own
// does not say which screen was on the other side of it.
let stage = "starting up";
const at = (s) => { stage = s; };

// Long enough for a control plane on a busy node, short enough that a
// run which is going to fail does not spend half an hour proving it: the
// steps below are sequential, so a 30s default is 30s of nothing per
// step once the first one has gone wrong.
const STEP_TIMEOUT = Number(process.env.WILD_TIMEOUT_MS ?? 10000);

// The Owner's sign-in, from the environment or their own file. Absent,
// it says so rather than surfacing as an ENOENT stack trace.
function readSignIn() {
  if (!BASE) {
    cannotRun(
      "no Console address: WILD_CONSOLE is not set",
      "Pass the household's Console address, e.g. WILD_CONSOLE=https://console.your.house/ . " +
        "The Console is entered through the identity provider, so Keystone's own port will not do.",
    );
  }
  if (!USER) {
    cannotRun("no Owner to sign in as: WILD_USER is not set", "Pass WILD_USER=<an active Owner's username> .");
  }
  if (process.env.WILD_PASSWORD) return process.env.WILD_PASSWORD;
  try {
    return readFileSync(PASSWORD_FILE, "utf8").trim();
  } catch {
    return cannotRun(
      `no Owner password: ${PASSWORD_FILE} could not be read`,
      "Write it to that file, or pass WILD_PASSWORD=… / WILD_PASSWORD_FILE=… .",
    );
  }
}

async function main() {
  const PASSWORD = readSignIn();

  // The browser binary is downloaded separately from the library, so a
  // fresh `npm install` gets you an import that works and a launch that
  // does not.
  let browser;
  try {
    browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined });
  } catch (e) {
    cannotRun(
      `chromium would not launch: ${e.message.split("\n")[0]}`,
      "Install the browser with `npx playwright install chromium` (add --with-deps on a bare Linux box).",
    );
  }

  const page = await browser.newPage();
  page.setDefaultTimeout(STEP_TIMEOUT);
  const consoleErrors = [];
  page.on("pageerror", (e) => consoleErrors.push(String(e)));

  try {
    at("opening the Console");
    try {
      await page.goto(BASE, { waitUntil: "networkidle" });
    } catch (e) {
      cannotRun(
        `${BASE} did not answer: ${e.message.split("\n")[0]}`,
        "Point WILD_CONSOLE at the household's Console address.",
      );
    }
    // With no session the Console sends the browser to sign in; it never
    // offers a box to paste a token into (ADR-W033 A4.1).
    check("the Console offers no token box", await page.locator('[aria-label="API token"]').count() === 0);
    check("no session sends the browser to the identity provider",
      new URL(page.url()).origin !== new URL(BASE).origin || await page.locator('input[type="password"]').count() > 0,
      page.url());

    at("signing in as the Owner through the identity provider");
    await page.locator('#username-textfield, input[name="username"], input[autocomplete="username"]').first().fill(USER);
    await page.locator('#password-textfield, input[type="password"]').first().fill(PASSWORD);
    await page.keyboard.press("Enter");
    // The OpenID Connect client may ask for consent the first time.
    const consent = page.getByRole("button", { name: /accept|consent|allow/i });
    await consent.first().waitFor({ timeout: 3000 }).then(() => consent.first().click()).catch(() => {});
    await page.waitForURL((u) => u.origin === new URL(BASE).origin, { timeout: STEP_TIMEOUT }).catch(() => {});
    await page.waitForTimeout(1500);

    check("logged in (rail appears)", await page.locator("nav.rail").count() > 0);
    check("Services tab exists", await page.getByRole("button", { name: "Services" }).count() > 0);

    at("opening Services");
    await page.getByRole("button", { name: "Services" }).click();
    await page.waitForTimeout(1500);

    // Since ADR-W027 the catalog is two halves: what is installed is a
    // board of tiles, what could be is a table of rows.
    const rows = await page.locator("table tbody tr").count();
    const tiles = await page.locator(".tile").count();
    check("catalog lists the packages", rows + tiles >= 9, `${rows} available rows + ${tiles} installed tiles`);
    check("documents-paperless listed", await page.locator("text=documents-paperless").count() > 0);
    check("identity shows as installed", await page.locator(".tile", { hasText: "identity" }).count() > 0);
    check("an installed capability says whether it answers",
      await page.locator(".tile", { hasText: "identity" }).locator("text=/answering|no address/").count() > 0);

    // Install flow
    at("opening the install form");
    const row = page.locator("tr", { hasText: "documents-paperless" });
    await row.getByRole("button", { name: /Install/ }).click();
    await page.waitForTimeout(800);

    const inputs = await page.locator(".panel input").count();
    check("form generated from the package", inputs === 5, `got ${inputs} inputs`);
    check("required marked", (await page.locator(".panel label").allTextContents()).join("|").includes("public_domain *"));
    check("default used as placeholder",
      await page.locator('.panel input[placeholder="paperless"]').count() > 0);

    const gated = await page.getByRole("button", { name: /fill in/ }).count();
    check("Preview gated until required filled", gated > 0);

    await page.locator(".panel input").nth(0).fill("192.168.1.10.nip.io");   // public_domain
    await page.locator(".panel input").nth(2).fill("wildstone-homelab");      // node
    await page.waitForTimeout(300);
    check("Preview enabled once filled", await page.getByRole("button", { name: "Preview" }).count() > 0);

    at("previewing the plan");
    await page.getByRole("button", { name: "Preview" }).click();
    await page.waitForTimeout(2500);

    const body = await page.locator(".panel").innerText();
    check("preview reports step count", /step\(s\)/.test(body), body.slice(0, 120));
    check("names paperless-oauth-providers", body.includes("paperless-oauth-providers"));
    check("names paperless-oidc", body.includes("paperless-oidc"));
    check("tells you to supply them", /must supply/i.test(body));
    check("offers Propose plan", await page.getByRole("button", { name: "Propose plan" }).count() > 0);
    check("no uncaught page errors", consoleErrors.length === 0, consoleErrors.join("; "));

  } catch (e) {
    // A driven browser reports a broken screen by throwing, and an
    // uncaught throw loses both the checks that already ran and the
    // summary. One failed step is not a reason to discard the report.
    if (e instanceof CannotRun) throw e;
    failures++;
    console.log(`FAIL: stopped during ${stage} \u2014 ${e.message.split("\n")[0]}`);
  } finally {
    // Best effort, and deliberately also on the failing path: a picture
    // of the screen as it actually was is the first thing anyone wants
    // when a check that drove a browser says no.
    await page
      .screenshot({ path: process.env.WILD_SHOT ?? "console-services.png", fullPage: true })
      .catch(() => {});
    await browser.close();
  }
}

try {
  await main();
} catch (e) {
  if (!(e instanceof CannotRun)) throw e;
  console.log(`CANNOT RUN: ${e.message}`);
  console.log(`  ${e.remedy}`);
  process.exit(EXIT_CANNOT_RUN);
}

console.log(failures === 0 ? "\nALL UI CHECKS PASSED" : `\n${failures} UI CHECK(S) FAILED`);
process.exit(failures ? EXIT_FAILED : 0);
