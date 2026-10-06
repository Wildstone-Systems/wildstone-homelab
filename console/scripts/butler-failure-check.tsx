import assert from "node:assert/strict";
import {createElement as h} from "react";
import {renderToString} from "react-dom/server";

// Node has no sessionStorage; call() and some screens read it.
const store = new Map<string, string>();
(globalThis as unknown as { sessionStorage: Storage }).sessionStorage = {
  getItem: (k: string) => store.get(k) ?? null,
  setItem: (k: string, v: string) => void store.set(k, v),
  removeItem: (k: string) => void store.delete(k),
  clear: () => store.clear(),
  key: () => null,
  length: 0,
} as Storage;

import {api, ApiError} from "../src/api";
import {failedAsk, Turn} from "../src/views/Butler";

// The browser half of a failed Butler ask (#233): the rejected request,
// what the screen adopts and says, and the failed turn's trace.
async function main() {
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({error: "model failed", sessionId: "bs-7", proposalErrors: ["400: refused by policy"]}), {
      status: 502,
      headers: {"Content-Type": "application/json"},
    })) as typeof fetch;
  try {
    for (const ask of [() => api.askOneButler("alfred", "hi"), () => api.continueButlerSession("bs-6", "hi")]) {
      const err = await ask().then(() => null, (e: unknown) => e);
      assert.ok(err instanceof ApiError, "a 502 rejects with an ApiError");
      assert.equal(err.status, 502);
      assert.equal(err.sessionId, "bs-7");
      assert.deepEqual(err.proposalErrors, ["400: refused by policy"]);
      const failed = failedAsk(err);
      assert.equal(failed.adopt, "bs-7", "the preserved session is adopted");
      for (const text of ["model failed", "refused", "400: refused by policy", "new conversation"]) {
        assert.ok(failed.message.includes(text), `error text omits ${text}`);
      }
    }
  } finally {
    globalThis.fetch = realFetch;
  }

  const bare = failedAsk(new ApiError(502, "model failed"));
  assert.equal(bare.adopt, null);
  assert.equal(bare.message, "model failed");
  const plain = failedAsk(new Error("offline"));
  assert.equal(plain.adopt, null);
  assert.equal(plain.message, "offline");

  const html = renderToString(
    h(Turn, {
      turn: {
        ordinal: 1,
        role: "user",
        text: "unlock the door",
        createdAt: "",
        planIds: ["plan-9"],
        transcriptJson: JSON.stringify({provider: "ollama", calls: [{name: "unlock_door", input: {}, result: "ok"}]}),
      },
      proposals: [],
      onResolved: () => {},
    }),
  ).replace(/<!--.*?-->/g, "");
  for (const text of ["unlock_door", "plan-9", "did not finish"]) {
    assert.ok(html.includes(text), `failed user turn omits ${text}`);
  }
  console.log("Butler failure path checks passed.");
}
main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
