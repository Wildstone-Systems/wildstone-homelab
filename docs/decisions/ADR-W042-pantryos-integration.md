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

### A3. PantryOS owns the grocery list when it is installed

- **Installed:** grocery items go to PantryOS (`pantry.add` / `pantry.check`).
  `lists-wildstone` keeps every other list.
- **Not installed:** the `lists-wildstone` `groceries` list is used.
- **Installed but not answering:** no silent fallback to `lists-wildstone`
  — that creates two half-lists. The tool reports PantryOS unavailable and
  the Butler says so.
- The routing decision is "is PantryOS **installed**", never "did the last
  call work". It lives in `keystone/domestic` (`Lists.Groceries`), and
  `TestAnInstalledPantryOSThatIsDownIsNeverFallenBackFrom` pins the
  unavailable case — and installed-but-not-connected, which is also
  PantryOS's list and says why it cannot answer. *(Implemented, #383.)*
- The grocery list is any list called `groceries`, `grocery` or `shopping`
  (with or without "list"). `show_list`, `add_items` and `check_items` go to
  PantryOS; `remove_items`, `clear_checked` and `delete_list` have no
  PantryOS match and are refused, saying where the list lives — a delete is
  refused before anyone is asked to confirm it. The lists overview notes
  that the grocery list is kept in PantryOS.

#### A3.1. Moving the old grocery list *(Implemented, #387)*

A household that kept groceries in `lists-wildstone` has an old list that
nobody's requests reach once PantryOS is installed. It is moved across,
offered and never automatic:

- **Where:** the pantry service's Connection panel, once PantryOS is paired
  (`GET|POST /api/v1/pantry/grocery-move`). Not the install Plan: at
  install time there is no connected PantryOS to move to.
- **What:** the open items of every grocery-named list (A3's names). Ticked
  items are the old list's history and are not sent.
- **One item at a time, through `pantry.add`** (repeat-safe). An item
  leaves the old list only once PantryOS has it
  (`runtimedb.RetireMovedListItems`), and the old list is removed, ticked
  history with it, once nothing open is left on it. That emptiness is
  judged in the same transaction, so an item added while the move ran
  keeps the list and waits for the next move.
- **Part-way failure loses nothing:** the move stops, says how far it got,
  and leaves the rest on the old list; moving again finishes it.
- It is consequential (A4.1 step-up), because it removes a list.

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
- **Connection-only:** a **pairing exchange** *(implemented, #384 / PantryOS
  #77)*:
  1. PantryOS shows a one-time code (five minutes, single use).
  2. The Owner sends it with PantryOS's address and this Keystone's address
     as PantryOS reaches it: `POST /api/v1/pantry/pairing`
     `{address, code, keystoneUrl}` (manage, consequential step-up).
  3. Keystone mints a fresh `publisher-pantry` token — without revoking the
     current one yet — and calls PantryOS's
     `/api/integrations/wildstone/pairing/exchange` once, never following a
     redirect. PantryOS stores the connection and answers with an agent key.
  4. Keystone checks that key against PantryOS's capability list, seals it
     in `integration_pairings` (migration 0089: the household secret key,
     bound to capability and endpoint, refused at startup with a missing or
     wrong key), and only then revokes the publisher tokens it replaces.

  Any failure revokes the token just minted and leaves an existing pairing
  exactly as it was. No route returns either secret: `GET` reports the
  endpoint and when, `DELETE` forgets the key and revokes the publisher
  tokens (PantryOS itself is untouched). Re-pairing is rotation.

The pantry tools resolve PantryOS on every call: a pairing wins over
`-pantry-url`/`PANTRY_AGENT_KEY`, so pairing, re-pairing and unpairing take
effect without a restart. An installed PantryOS with neither says it is not
connected yet.

## Work (Wildstone side)

1. `keystone/adapter/pantry`: `HTTP` upstream; fake moved to `pantry-1`
   semantics; contract-suite row for the real deployment.
2. `keystone/integration`: add the `pantry` domain (queries, actions,
   well-known events) to the closed vocabulary.
3. `packages/pantry-pantryos/package.yaml`: `integrates:` (managed and
   connection-only), `publishes:` (A4), `agentTools:` (A5); a test pinning
   the tools to the domestic gateway, as `TestListsToolsAreDeclaredByThePackage`
   does for lists.
4. `keystone/domestic`: grocery routing (A3) with the unavailable case pinned;
   moving an old grocery list across (A3.1, #387).
5. Connection Plan + pairing exchange (A6) — the exchange is implemented
   (#384), with its Console screen in the pantry service's detail panel
   (Services → PantryOS → Connection).

## Consequences

- PantryOS never has to move onto the Wildstone host.
- The adapter stays small: PantryOS does the pantry logic and answers in
  Wildstone's own result types.
- One list rule to explain to people: groceries live in PantryOS when it is
  there; everything else lives in Wildstone.
