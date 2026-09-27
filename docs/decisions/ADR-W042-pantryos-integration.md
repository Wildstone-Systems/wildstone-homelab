# ADR-W042: PantryOS integration

- **Status:** proposed. The PantryOS half is implemented (PantryOS ADR 0026).
  Wildstone: A2 implemented (#380 — `keystone/adapter/pantry/http.go`); A4/A5
  implemented as `packages/pantry-pantryos` and `keystone/domestic/pantry.go`
  (#382), on the road in A1.1; the rest is tracked in #379.
- **Builds on:** ADR-W024 (household events, A8 subject mapping, A10 shims),
  ADR-W025 / `docs/catalog-integration-contract.md` (the `integrates:` block),
  ADR-W030 A5/A5.1/A8 (connections, adapters, action classes, P12's PantryOS
  fake).
- **Counterpart:** PantryOS `docs/decisions/0026-wildstone-home-lab-integration.md`.

## Context

P12 built the PantryOS adapter against a versioned fake (`fake-1`) because
PantryOS had no integration API. PantryOS now has one: contract `pantry-1`,
an event relay that posts to the household intake, and server-side cook
sessions an agent can drive. This ADR settles what Wildstone does with it.

## Decisions

### A1. PantryOS may run anywhere Wildstone can reach

Both shapes are supported and speak the same contract:

- **Managed:** Wildstone installs PantryOS from the catalog.
- **Connection-only** (W030 A5): PantryOS runs on its own machine or VM.
  Wildstone connects and never owns it; removing the connection never
  removes PantryOS.

In both, the PantryOS adapter runs as a package-managed container on a
Wildstone node that can reach PantryOS (W030 A5.1). Keystone makes no
PantryOS-specific calls. Two network paths, both HTTPS on the household
network or tailnet:

- adapter → PantryOS `…/api/integrations/wildstone/v1/*` with a PantryOS
  **agent key** (a node secret, never in a work item or model context);
- PantryOS → Keystone `POST /api/v1/household/events` with the `pantry`
  **publisher token** (W024 A10, *native* shape: no shim).

### A2. The real adapter is a thin HTTP Upstream

`pantry-1` mirrors `adapter.Upstream` one-to-one:

| Upstream | PantryOS |
|---|---|
| `Capabilities()` | `GET /v1/capabilities` |
| `Query(cap, req)` | `GET /v1/query/{cap}?<req as query string>` |
| `Act(cap, req)` | `POST /v1/act/{cap}` with `req` as the body |

Mapping onto the runtime's result types:

| PantryOS answers | Upstream returns |
|---|---|
| 200, `status: ok` | `Answer{Data}` |
| 200, `status: empty` | `Answer{Empty: true, Data}` |
| 404 whose detail is PantryOS naming the capability | `ErrNoSuchCapability` |
| any other 404 (a wrong address, a proxy page) | `ErrUnavailable` |
| 422 | `pantry.ErrRejected` with PantryOS's message (a wrong request, not an outage) |
| network error, 5xx, 401/403 | `ErrUnavailable` |

One honest limit: the runtime's result set is closed at four (`ok`, `empty`,
`stale`, `unavailable`), so the runner reports an `ErrRejected` as
`unavailable` carrying PantryOS's refusal text. The adapter keeps the two
apart; the work record does not. A fifth result type is a W030 change and
is not made here.

The adapter records `Version: "pantry-1"` from the capability list, so "which
contract is this" keeps its answer. `keystone/adapter/pantry` keeps the fake
beside a new `HTTP` upstream; the fake moves to `pantry-1` semantics (below)
so the contract suite keeps meaning the same thing for both.

**Contract-test change.** The fake treated an unknown list (`hardware`) as
empty. `pantry-1` refuses it (422), because PantryOS keeps only the grocery
list and a routing mistake must not read as "nothing on it". The suite's
empty case for PantryOS becomes `{"list":"groceries"}` against an empty
pantry, and the real deployment is enrolled with
`WILDSTONE_TEST_PANTRY_URL` + `WILDSTONE_TEST_PANTRY_KEY`, like HA and CalDAV.

### A3. PantryOS owns the grocery list when it is connected

- **Connected:** grocery items go to PantryOS (`pantry.add` / `pantry.check`).
  `lists-wildstone` keeps every other list.
- **Not installed:** the `lists-wildstone` `groceries` list is used.
- **Connected but not answering:** no silent fallback to `lists-wildstone`
  — that creates two half-lists. The tool reports PantryOS unavailable and
  the Butler says so.
- The routing decision is "is a PantryOS connection **installed**", never
  "did the last call work". It lives in `keystone/domestic` beside
  `ListsTools`, and a test pins the unavailable case.
- On first connect, the Plan offers a one-time import of the existing
  `groceries` list into PantryOS (each item through `pantry.add`, which is
  repeat-safe).

### A4. Events

Declared in the package's `publishes:` block, shape `native`:

| Type | Subject | Payload (bounded, no nutrition) |
|---|---|---|
| `pantry.meal_logged` | member (when linked) | `food, meal_type, quantity, unit, profile_id` |
| `pantry.purchase_recorded` | — | `item_count, items[≤25]` |
| `pantry.recipe_cooked` | — | `recipe, recipe_id, servings_made \| batches, leftover_servings` |
| `pantry.stock_low` | — | `item, item_id, on_hand, threshold, unit` |
| `pantry.item_expiring` | — | `item, item_id, expires_on, days_left, quantity, unit` |

- **Subject mapping (W024 A8):** PantryOS links each household profile to a
  member name. Unlinked profiles publish with no subject — a valid event that
  cannot fire a per-member rule.
- **Event ids are deterministic** over their PantryOS source, and PantryOS
  queues them in an outbox; the intake's existing de-duplication makes
  retries free.
- `pantry.connection_tested` is sent by PantryOS's "test connection" button.
  It is not declared as a routine trigger.

### A5. Agent tools

Declared in the package's `agentTools:` with these tiers. Tiers are
PantryOS's proposal (`proposedRisk` in `pantry-1` capabilities); Wildstone
policy validates them and an Owner may make them stricter, never weaker
(W030 A8).

| Tool | Capability | Risk |
|---|---|---|
| `pantry_summary` | `pantry.summary` | read |
| `check_stock` | `pantry.inventory` | read |
| `whats_expiring` | `pantry.expiring` | read |
| `show_grocery_list` | `pantry.list` | read |
| `suggest_recipes` | `pantry.recipes` | read |
| `get_recipe` | `pantry.recipe` | read |
| `start_cooking` | `pantry.cook.start` | read |
| `current_step` | `pantry.cook.current` | read |
| `cooking_step` (next / previous / repeat / goto) | `pantry.cook.step` | read |
| `stop_cooking` | `pantry.cook.abandon` | read |
| `add_to_grocery_list` | `pantry.add` | domestic |
| `check_grocery_item` | `pantry.check` | domestic |
| `finish_cooking` | `pantry.cook.finish` | confirm |

- Moving through a cook-along is `read`: it changes no stock. Only
  `finish_cooking` removes ingredients, so it is `confirm` — a person is
  asked, and a routine cannot run it alone.
- **Not in v1:** `log_meal` and `delete_meal` (they need a food-name →
  catalog-item resolution step PantryOS does not expose yet), fetching
  product URLs (outbound fetches), receipt/cloud AI, and anything admin.

### A1.1 Amendment: v1 takes the calendar's road

A1's adapter container needs two things that do not exist yet: a program
that runs an `adapter.Runner` in a container, and a production consumer
that asks the integration work queue and waits for the answer (the
`crossapp.Integrations` seam is exercised only by tests). Rather than build
both before the household can use its pantry, v1 takes the road ADR-W034
A5 already accepted for the calendar:

- Keystone holds the upstream itself: `-pantry-url` names PantryOS and
  `PANTRY_AGENT_KEY` carries its agent key (never a flag). With either
  missing the `pantry.*` executors are not registered, so no Agent is
  offered a pantry this deployment has not got.
- Each call is also gated on `pantry-pantryos` being installed, as the
  lists are, and the tools are left out of an Agent's vocabulary until it
  is.
- The upstream is the same `pantry.HTTP`, checked by the same contract
  suite, so moving to the adapter container later changes where it runs,
  not what it does.

This is an operator's decision on a different road, as W034 A5 puts it —
not a weaker one: the declaration check, tiers, confirmation and the
execution authority all apply exactly as on the queue path.

### A6. Connecting

**Wildstone → PantryOS** needs nothing new: the Owner mints a PantryOS agent
key (read + write) and enters it, with PantryOS's address, in the connection
Plan. It becomes a node secret for the adapter.

**PantryOS → Wildstone has a gap today.** `POST
/api/v1/household/publishers/{capability}` mints the publisher token and
places it on a Wildstone node as a secret; by design it never appears in the
response (W024 A10). That serves the *managed* shape — PantryOS's container
reads it from its environment — but a *connection-only* PantryOS on another
machine has no way to receive it. So:

- **Managed:** works as-is; the package wires `<capability>-publisher` into
  PantryOS's environment and PantryOS reads it (a PantryOS env seed for the
  `wildstone` connector config).
- **Connection-only:** needs a **pairing exchange**. PantryOS shows a
  one-time, 5-minute code (its ADR 0018 §8 device-pairing pattern). The Owner
  enters the code and PantryOS's address in Wildstone's connection Plan.
  Keystone calls PantryOS once, with the code, to (a) receive a fresh agent
  key for the adapter and (b) deliver the publisher token — the token goes
  machine to machine over TLS, is never shown to a person, and is still
  stored only as a hash in Keystone. Re-pairing is rotation, exactly like
  re-minting today.

Until pairing lands, a connection-only PantryOS can answer queries and
actions but cannot publish events. The PantryOS side already accepts the
token (`PUT /api/integrations/wildstone/connection`), so pairing only has to
deliver it.

## Work (Wildstone side)

1. `keystone/adapter/pantry`: `HTTP` upstream; fake moved to `pantry-1`
   semantics; contract-suite row for the real deployment.
2. `keystone/integration`: add the `pantry` domain (queries, actions,
   well-known events) to the closed vocabulary.
3. `packages/pantry-pantryos/package.yaml`: `integrates:` (managed and
   connection-only), `publishes:` (A4), `agentTools:` (A5); a test pinning
   the tools to the domestic gateway, as `TestListsToolsAreDeclaredByThePackage`
   does for lists.
4. `keystone/domestic`: grocery routing (A3) with the unavailable case pinned.
5. Connection Plan + pairing exchange (A6), including the one Keystone call
   that delivers a publisher token to a connection-only app.

## Consequences

- PantryOS never has to move onto the Wildstone host.
- The adapter stays small: PantryOS does the pantry logic and answers in
  Wildstone's own result types.
- One list rule to explain to people: groceries live in PantryOS when it is
  there; everything else lives in Wildstone.
