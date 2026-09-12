// End-to-end check of the Console in a real browser.
//
// The render check (render-check.tsx) proves the components render;
// this proves the SCREEN works — token gate, navigation, the catalog
// table, the generated install form, and the preview that names the
// secrets only an operator can supply.
//
// Needs a browser AND a running Keystone with a real catalog, which is
// why it is not part of `npm run build` or CI: there is no house in CI
// to point it at. It is an operator's check, run against their own
// control plane:
//
//   npx playwright install chromium     # once, for the browser itself
//   WILD_API=http://your-node:9441/ npm run ui-check
//
// `playwright` is a declared devDependency, so `npm install` is enough
// to get the library; the browser binary is a separate download and the
// preflight below says so when it is missing.
//
// It reads an operator token from ~/.wild_operator_token. It stops at
// Preview and never clicks "Propose plan": a check that installs a
// capability every time it runs is not a check.
import { chromium } from "playwright";
import { readFileSync } from "fs";

// Whose deployment this runs against is the operator's to say, not this
// script's: a hardcoded home directory and LAN address only ever matched
// one machine, and put that machine's details in a public repository.
const TOKEN_FILE = process.env.WILD_TOKEN_FILE
  ?? `${process.env.HOME ?? "."}/.wild_operator_token`;
const BASE = process.env.WILD_API ?? "http://127.0.0.1:9441/";

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

// The token, from the environment or the operator's own file. Absent, it
// used to surface as an ENOENT stack trace naming a path the reader had
// to work out the meaning of.
function readToken() {
  if (process.env.WILD_TOKEN) return process.env.WILD_TOKEN.trim();
  try {
    return readFileSync(TOKEN_FILE, "utf8").trim();
  } catch {
    return cannotRun(
      `no operator token: ${TOKEN_FILE} could not be read`,
      "Write the token to that file, or pass WILD_TOKEN=… / WILD_TOKEN_FILE=… .",
    );
  }
}

async function main() {
  const TOKEN = readToken();

  // The browser binary is downloaded separately from the library, so a
  // fresh `npm install` gets you an import that works and a launch that
  // does not.
  let browser;
  try {
    browser = await chromium.launch();
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
        "Point WILD_API at a running Keystone, or start the one on this machine.",
      );
    }
    check("token gate is shown", await page.locator("text=Wildstone Console").count() > 0);

    at("logging in with the operator token");
    await page.locator('input[type="password"]').fill(TOKEN);
    await page.getByRole("button", { name: /continue|enter|go|sign|open/i }).or(page.locator("button")).first().click();
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
