import { chromium } from "playwright";
import { createServer } from "node:http";
import { readFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import assert from "node:assert/strict";
const value = randomBytes(16).toString("hex");
const pageHTML = readFileSync(
  new URL("../../keystone/portal/web/secrets.html", import.meta.url),
);
const objects = [
  { id: "hs_household", scope: "household", label: "Wi-Fi", state: "active" },
  { id: "hs_personal", scope: "personal", label: "Library", state: "active" },
];
const requests = [];
const server = createServer(async (req, res) => {
  if (req.url === "/home/secrets/") {
    res.setHeader("Content-Type", "text/html");
    res.end(pageHTML);
    return;
  }
  if (req.url.startsWith("/portal/assets/")) {
    const name = req.url.split("/").pop();
    if (
      ![
        "nocturne.css",
        "inter-latin.woff2",
        "phosphor.woff2",
        "phosphor-fill.woff2",
      ].includes(name)
    ) {
      res.writeHead(404);
      res.end();
      return;
    }
    res.setHeader(
      "Content-Type",
      name.endsWith(".css") ? "text/css" : "font/woff2",
    );
    res.end(
      readFileSync(
        new URL("../../keystone/portal/web/assets/" + name, import.meta.url),
      ),
    );
    return;
  }
  let body = "";
  for await (const part of req) body += part;
  requests.push({ path: req.url, method: req.method, body });
  res.setHeader("Content-Type", "application/json");
  if (req.method === "GET")
    res.end(
      JSON.stringify({
        secrets: objects,
        // An operator Agent that sorts first, and a household one: the
        // page must default to the household Agent and refuse a reveal
        // through the operator one without asking the server.
        agents: [
          { name: "aaa-ops", agentClass: "owner" },
          { name: "shared", agentClass: "member" },
        ],
      }),
    );
  else if (req.url.endsWith("/reveal")) res.end(JSON.stringify({ value }));
  else res.end("{}");
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const browser = await chromium.launch({
  headless: true,
  executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
});
try {
  const page = await browser.newPage({
    viewport: { width: 1280, height: 900 },
  });
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/home/secrets/`);
  await page.getByRole("heading", { name: "Wi-Fi", exact: true }).waitFor();
  assert(
    !(await page
      .locator("body")
      .innerText()
      .then((t) => t.includes(value))),
  );
  const personal = page
    .locator("article")
    .filter({ has: page.getByRole("heading", { name: "Library" }) });
  assert.equal(
    await personal.getByRole("button", { name: /Reveal/ }).count(),
    0,
  );
  // The Agent is a choice, defaulting to a household Agent.
  assert.equal(await page.locator("#agent").inputValue(), "shared");
  // An operator Agent cannot reveal on this page: refused here, with no
  // request sent.
  await page.selectOption("#agent", "aaa-ops");
  const before = requests.length;
  await page.getByRole("button", { name: "Reveal Household value" }).click();
  assert.match(await page.locator("#error").innerText(), /household Agent/);
  assert.equal(requests.length, before, "a reveal through an operator Agent was sent");
  await page.selectOption("#agent", "shared");
  await page.getByRole("button", { name: "Reveal Household value" }).click();
  await page.waitForFunction(
    () => document.querySelector("#revealed").textContent.length > 0,
  );
  assert.equal(await page.locator("#revealed").innerText(), value);
  await page.evaluate(() => window.dispatchEvent(new Event("blur")));
  assert.equal(await page.locator("#revealed").innerText(), "");
  await page.fill("#label", "Calendar");
  await page.fill("#value", value);
  // What was typed can be checked before it is stored, and is concealed
  // again when the window loses focus.
  assert.equal(await page.locator("#value").getAttribute("type"), "password");
  await page.getByRole("button", { name: "Show what I typed" }).click();
  assert.equal(await page.locator("#value").getAttribute("type"), "text");
  await page.evaluate(() => window.dispatchEvent(new Event("blur")));
  assert.equal(await page.locator("#value").getAttribute("type"), "password");
  await page.getByRole("button", { name: "Store securely" }).click();
  await page.waitForFunction(
    () => document.querySelector("#value").value === "",
  );
  assert(
    requests.some(
      (r) =>
        r.path === "/api/v1/home/secrets" &&
        r.method === "POST" &&
        JSON.parse(r.body).value === value,
    ),
  );
  assert.equal(errors.length, 0, errors.join("\n"));
  if (process.env.SECRETS_SCREENSHOT)
    await page.screenshot({
      path: process.env.SECRETS_SCREENSHOT,
      fullPage: true,
    });
  console.log(
    "PASS: protected reveal, no Personal reveal, Agent choice (household default, operator refused), typed-value reveal, blur clearing, secure intake, no browser errors",
  );
} finally {
  await browser.close();
  server.close();
}
