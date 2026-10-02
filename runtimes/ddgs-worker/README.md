# Bounded DDGS worker (S18)

Dormant worker for [#491](https://github.com/chickenrat17/wildstone-homelab/issues/491),
[ADR-W047](../../docs/decisions/ADR-W047-native-agent-research-and-workspaces.md)
and the [v1 contract](../../docs/design/native-agent-capability-contract-v1.md).
This adds no tool grants, provider records, catalog activation or deployment.
Provider configuration (#490), Go adaptation (#492), managed packaging (#493)
and live conformance (#509) remain separate slices.

## Wire and limits

- `GET /health`: `{ "version": 1, "ready": true }`. A failed process reap
  makes readiness false (503). This is local worker readiness, **not** evidence
  that upstream search works or that an Owner's provider test has passed.
- `POST /v1/search`, `Content-Type: application/json`:
  `{ "version": 1, "query": "example documentation", "limit": 5, "backend": "duckduckgo" }`.
  All four fields are required. Optional `filters` accepts only `region` values
  `wt-wt` (default), `us-en`, `uk-en`. `language`, arbitrary regions and
  `recencyDays` explicitly return `unsupported`; the worker never drops or
  approximates a requested filter. Keystone's capability discovery must report
  only this supported subset.
- Success: `{ "version": 1, "data": { "items": [{ "url": "https://example.com/",
  "title": "Example", "snippet": "Documentation" }], "provider": "ddgs",
  "backend": "duckduckgo", "retrievedAt": "2026-09-30T00:00:00Z" } }`.
  Empty results are 200 with `items: []`. Titles are optional; timestamps are
  UTC RFC3339. Source dates are normalized when available, never inferred from
  snippets or replaced with the retrieval date. DDGS text currently provides none.
- Errors: `{ "version": 1, "error": { "code": "timeout", "message": "Search deadline exceeded." } }`.
  Codes/status: invalid_input/400, unsupported/422, rate_limited/429,
  provider_unavailable/503, timeout/504, internal_error/500. Disconnects cancel
  execution and emit a safe `cancelled` event; no response can reach a closed peer.
  Unknown routes/methods return 404/405 with an invalid_input envelope.
- Query: 1–1,024 UTF-8 bytes; results: 1–5; body: at most 8 KiB, no compression,
  duplicate keys, unknown properties, coercion or non-finite numbers. URLs are
  HTTP/S, at most 2,048 bytes, without userinfo. Fragments are removed; no result
  URL is fetched. Title/snippet ceilings are 512/4,096 bytes; oversized or invalid
  results fail explicitly instead of claiming an undisclosed truncation.
- Internal response ceiling: 64 KiB. **This does not increase the 4,000-byte
  Agent gateway limit.** Keystone must validate, assign source IDs and paginate
  results into bounded tool envelopes. Search strings/results remain untrusted.
- One service process per installation, with exactly two upstream slots, at most
  eight queued searches and ten admitted HTTP search bodies. No multiworker
  server or replicas: those would multiply the installation limit.
- A 30-second total deadline includes body read, queue wait and execution.
  There is one upstream attempt, no retry/fallback/cache. Upstream HTML is capped
  at 1 MiB and each socket operation at 25 seconds. Hung work runs in disposable
  processes: timeout, disconnect and shutdown kill/reap before reusing capacity.
  Reaping has a further one-second bound; if it cannot be proven, the slot stays
  occupied and the service fails closed until restarted.
- Logs contain fixed event/error labels and queue/active counts only. Request
  IDs, queries, paths, headers, result bodies and raw exception messages are not
  logged. Framework/access logs are disabled by the entrypoint; child stderr is
  discarded. Logs make admission, queueing, start, cancellation and release
  observable without retaining research content.

## Pinned upstream boundary

Python 3.12.14, DDGS 9.16.0 and aiohttp 3.14.3 are pinned. `requirements.txt`
locks transitive versions and distribution hashes (generated with uv 0.12.19).
Updates require regenerating the lock and rerunning the pinned-engine fixtures.
The container base has an exact Python version tag; managed builds require a base digest as described in [RELEASE.md](RELEASE.md).
The release image has not yet been built or verified.

The worker uses DDGS's DuckDuckGo payload builder and result parser directly.
Its generic dispatcher can silently fall back to `auto` for invalid backend
selection and collapses blocked/no-result responses. A small streaming urllib
transport therefore enforces one fixed DuckDuckGo endpoint, TLS verification,
no ambient proxies, redirects, cookies or credentials, and explicit HTTP error
classification. Only DuckDuckGo is supported; other engine names and comma
lists are refused. A changed page layout or challenge is a provider error,
never an empty success. The fixture suite executes the actual pinned DDGS parser
without contacting a search engine.

## Local checks and operation

From this directory:

```sh
python3.12 -m venv .venv
.venv/bin/pip install --require-hashes -r requirements.txt
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -B service.py
```

The standalone listener defaults to `127.0.0.1:8080`. Health and fixtures do not
perform live searches. Do not send live queries as a conformance substitute.
The service requires no privileged mode, credentials, persistent volume,
Agent workspace mount or shell capability. Its image supports a read-only
root filesystem, dropped Linux capabilities and `no-new-privileges`.

The `research-ddgs` package runs exactly one container, binds its internal
listener to `0.0.0.0`, and publishes only `127.0.0.1:<port>:8080` on Keystone's
node. It creates no public Route, credentials, volumes or sidecar mounts.
The maintainer accepted the trusted homelab-network boundary in #493 on
2026-09-30: the shared bridge and host processes can reach this unauthenticated
worker directly and bypass Keystone permissions, budgets and audit. Loopback
publication does not establish per-app or Keystone-process-only isolation.
This is a documented limitation (#191); do not expose it to the public Internet.
See [RELEASE.md](RELEASE.md) for the outstanding image/readiness evidence.

Keystone retains per-Agent opt-in grants, current Person/surface restrictions,
provider selection, privacy/provenance checks, task budgets, fallback policy
and source persistence. Shipping this directory enables none of those tools.
