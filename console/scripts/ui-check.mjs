// End-to-end check of the Console in a real browser.
//
// The render check (render-check.tsx) proves the components render;
// this proves the SCREEN works — token gate, navigation, the catalog
// table, the generated install form, and the preview that names the
// secrets only an operator can supply.
//
// Needs a browser and a running Keystone, so it is deliberately NOT part
// of `npm run build` or CI: install with `npx playwright install
// --with-deps chromium` and `npm i -D playwright`, point BASE at a live
// control plane, and run `npm run ui-check`.
//
// It reads an operator token from ~/.wild_operator_token. It stops at
// Preview and never clicks "Propose plan": a check that installs a
// capability every time it runs is not a check.
import { chromium } from "playwright";
import { readFileSync } from "fs";

const TOKEN = readFileSync("/home/user/.wild_operator_token", "utf8").trim();
const BASE = "http://192.168.1.52:9441/";
let failures = 0;
const check = (label, cond, detail = "") => {
  if (!cond) { failures++; console.log("FAIL:", label, detail); }
  else console.log("ok:", label);
};

const browser = await chromium.launch();
const page = await browser.newPage();
const consoleErrors = [];
page.on("pageerror", (e) => consoleErrors.push(String(e)));

await page.goto(BASE, { waitUntil: "networkidle" });
check("token gate is shown", await page.locator("text=Wildstone Console").count() > 0);

await page.locator('input[type="password"]').fill(TOKEN);
await page.getByRole("button", { name: /continue|enter|go|sign|open/i }).or(page.locator("button")).first().click();
await page.waitForTimeout(1500);

check("logged in (nav appears)", await page.locator("nav.nav").count() > 0);
check("Services tab exists", await page.getByRole("button", { name: "Services" }).count() > 0);

await page.getByRole("button", { name: "Services" }).click();
await page.waitForTimeout(1500);

const rows = await page.locator("table tbody tr").count();
check("catalog table has rows", rows >= 9, `got ${rows}`);
check("documents-paperless listed", await page.locator("text=documents-paperless").count() > 0);
check("identity shows as enabled", await page.locator("tr", { hasText: "identity" }).locator("text=enabled").count() > 0);

// Install flow
const row = page.locator("tr", { hasText: "documents-paperless" });
await row.getByRole("button", { name: /Install|Reconfigure/ }).click();
await page.waitForTimeout(800);

const inputs = await page.locator(".panel input").count();
check("form generated from the package", inputs === 5, `got ${inputs} inputs`);
check("required marked", (await page.locator(".panel label").allTextContents()).join("|").includes("public_domain *"));
check("default used as placeholder",
  await page.locator('.panel input[placeholder="paperless"]').count() > 0);

const gated = await page.getByRole("button", { name: /fill in/ }).count();
check("Preview gated until required filled", gated > 0);

await page.locator(".panel input").nth(0).fill("192.168.1.52.nip.io");   // public_domain
await page.locator(".panel input").nth(2).fill("wildstone-homelab");      // node
await page.waitForTimeout(300);
check("Preview enabled once filled", await page.getByRole("button", { name: "Preview" }).count() > 0);

await page.getByRole("button", { name: "Preview" }).click();
await page.waitForTimeout(2500);

const body = await page.locator(".panel").innerText();
check("preview reports step count", /step\(s\)/.test(body), body.slice(0, 120));
check("names paperless-oauth-providers", body.includes("paperless-oauth-providers"));
check("names paperless-oidc", body.includes("paperless-oidc"));
check("tells you to supply them", /must supply/i.test(body));
check("offers Propose plan", await page.getByRole("button", { name: "Propose plan" }).count() > 0);
check("no uncaught page errors", consoleErrors.length === 0, consoleErrors.join("; "));

await page.screenshot({ path: "/home/user/console-services.png", fullPage: true });
await browser.close();
console.log(failures === 0 ? "\nALL UI CHECKS PASSED" : `\n${failures} UI CHECK(S) FAILED`);
process.exit(failures ? 1 : 0);
