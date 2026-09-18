# Butler memory: what exists, and the four seams that would finish it

> **Notes, not a source.** This file was generated on 2026-09-10 by
> reading the existing code. It is an accurate inventory of what
> `keystone/runtimedb` already contains, and that part is still useful.
>
> Its "order of work" is not. That ordering was invented here, not
> derived from any accepted design, and it is what pushed semantic
> recall to step 6 — leaving a memory feature that cannot answer the
> design brief's own cilantro example. It also encoded a per-Butler
> memory scope that the baseline explicitly rejects.
>
> The accepted statement of intent is
> [the agent architecture baseline](agent-architecture-baseline.md).
> Read that first, and treat anything below as observation about the
> code rather than as a plan. Remaining work is tracked from
> [#151](https://github.com/chickenrat17/wildstone-homelab/issues/151).

- **Status:** steps 1 and 2 of the ordering below were built and
  deployed. Step 3 was started and parked. Steps 4 to 7 are superseded
  by the baseline audit.
- **Describes:** ADR-W030 A2 (permission-first retrieval), A3 (owned data
  with derived retrieval indexes), A3.1 (acceptance boundaries), A6
  (freshness); ADR-W031 A4 (a Butler's scope), A5 (the shared pool).
  A3's deferral of vector retrieval and W031 A4's scope are both
  superseded by baseline §6/§7.

---

## The finding

Almost all of it is already written. What is missing is that nothing
calls it.

`keystone/runtimedb/butlerindex.go` is a complete retrieval layer, and
the implementation choice was made by measurement rather than by taste:
SQLite FTS5 with bm25 for the keyword half, an optional caller-supplied
embedding for the vector half, hybrid ranking across both. No service,
no second datastore, no new dependency. It holds permission-first
filtering *in the candidate SQL* — its own comment is the reason why:

> A match on material the reader may not see must never exist, not even
> briefly, because "briefly" is how it reaches a model's context.

It also carries freshness (a volatile source comes back flagged for live
verification rather than answered from the index), deletion that
propagates to the FTS triggers, compaction, measurement recording, and a
job queue.

`keystone/runtimedb/butlermemory.go` is the owned-data half: record,
derive, correct, forget, invalidate-from-source, export, digest. Records
carry kind, subject, audience, permission label, owner, Butler scope,
source, observed-at, confidence, safety-relevance and supersession.

`keystone/runtimedb/butlerscope.go` has A5's shared pool, and
`PromoteMemoryToSharedPool` already returns which Butlers gain access
and whether any of them runs on a cloud model — the disclosure sentence
A5 asks for is a return value waiting for a screen.

**And none of it ran.** Outside `runtimedb`, when this was written —
with what steps 1 and 2 changed:

| Function | Callers then | Callers now |
|---|---|---|
| `RetrieveMemory` | the Console's Memory tab | and the injected preamble |
| `RecordMemory` | none | the `remember` tool |
| `IndexDocumentInto` | none | the `remember` tool, inline |
| `SearchIndex` | none | the `recall` tool |
| `ClaimIndexJob` | none | none — step 3 |

Zero records were stored on the live deployment, and the household
gateway's system prompt said, in as many words, *"you have no memory
between conversations"* — which was true, and was the thing a person hit
first.

---

## The four seams

### 1. Retrieval — memory reaches the model

Two paths, and the ADR wants both because it describes two different
things.

**Always-on, for preferences and procedures.** A3 says explicit
preferences persist until corrected. A preference only shapes an answer
if it is present before the model speaks, so this is an injection: query
`RetrieveMemory{Reader: member, Butler: b.Name, Kind: "preference"}`
plus procedures, render as a short block, prepend to the system prompt.
The runner assembles that at `keystone/butler/runner.go:543`
(`System: r.Gateway.SystemPrompt()`), which is the one place to change.

**On demand, for everything else.** A `recall` tool over `SearchIndex`,
for "what did I say about the boiler in March". It is safe as a tool
rather than an injection because the filter is in the SQL: `IndexQuery`
takes `Reader` and `Butler` and ANDs them onto every candidate, so a
model choosing the query text cannot widen the scope. What it must NOT
do is take the reader or the Butler from the model — those come from
the session, exactly as the gateway's other seams do.

Both must respect `MustVerifyLive`. A hit from a volatile source is what
was true when indexed; answering from it without a live read is the
failure A6 names.

**Budget.** The injected block needs a hard cap in tokens, and the cap
belongs in the code rather than in a prompt asking the model to be
brief. An unbounded memory block is a context window that shrinks as a
household uses the system.

### 2. Capture — something writes

This is the half with teeth, and the only half that is a design decision
rather than plumbing.

**Explicit statements** are a tool: the person says remember this, the
Butler calls `remember`, the record is `kind: "fact"` or `"preference"`
with `confidence: "stated"`. Cheap, and the boundary cases barely arise.

**Inferences** are what A3 actually asks for — "Butler may learn
preferences and reusable procedures automatically" — and they come with
A3.1's whole test surface. Shape:

- a second pass after the turn completes, extracting candidates;
- stored as `kind: "inference"`, `confidence: "tentative"`;
- `Author` is the Butler, `Owner` is the person who spoke;
- unused inferences lose influence over time (A3), which means
  retrieval ranking must know the difference — an inference is not a
  fact that happens to be less certain.

Five rules A3.1 states that the implementation has to hold, each of
which is a test:

1. **Redact before storing.** `redact.String` on the way in, always,
   including credentials and password-reset material.
2. **Restrictions travel.** A derivative satisfies the union of its
   sources' restrictions. `DeriveMemory` takes source seqs for exactly
   this; use it rather than `RecordMemory` for anything inferred.
3. **Missing permission metadata fails closed.** No audience, no
   permission label, no write.
4. **An inference is never authorization.** A learned procedure cannot
   widen a tool, reach a connection, or stand in for an approval. The
   tool resolver already refuses independently; the test is that a
   procedure claiming otherwise changes nothing.
5. **A group statement belongs to its speaker.** "I dislike cilantro" in
   a shared room is that person's preference with the room recorded as
   the source audience — not the household's.

**Butler scope on write is the Butler, never the pool.** `Butler:
b.Name`. Promotion to shared is A5's separate, explicit act, and doing
it silently is the failure A5 calls the worst kind: the property still
looks true and the material has already left.

### 3. The job runner — something drains the queue

`QueueIndexJob` / `ClaimIndexJob` / `FinishIndexJob` exist and nothing
claims. A worker belongs beside the reconcile service in
`cmd/keystone`, and it must be **coordinator-gated**: two processes
draining one queue would index the same chunk twice and race the
compaction. The lease is already there — `AcquireCoordinator` — and
butlerlive already takes it.

Indexing is a job rather than an inline write because embedding is slow
and a person waiting on an answer should not wait on it.

### 4. Embeddings — the one genuine gap

There is no embedding client. `IndexDocument.Embedding` is optional and
its comment is explicit that nil "is a working configuration and not a
degraded one", so **bm25 alone works on day one** and A3's "hybrid
meaning/keyword" is reachable afterwards.

Ollama serves `/api/embeddings`. It wants a `provider.Embedder` seam
alongside the chat provider, wired the same way and from the same
assignment, so a Butler's memory is embedded by a model the household
chose. `butler_index_measurements` exists to compare keyword against
hybrid on real data, which makes the upgrade a measurement rather than
an opinion.

---

## Order of work

1. **Retrieval, keyword-only, read-only.** *Done.* The `recall` tool and
   the injected preference block, wired against records that did not yet
   exist. It sounds backwards and is not: it proved the permission
   filter and the budget with nothing at stake, and every later step
   writes into a path already known to be safe.
2. **Explicit capture.** *Done.* The `remember` tool, redaction,
   fail-closed metadata. Memory works end to end at this point, for
   anything a person asks it to hold. Three things the plan did not
   anticipate:
   - The shared redaction path does not catch a credential named in
     prose. Its rules are about shape, and "the router password is
     hunter2" has none — a short ordinary word, and "is" is not a
     key/value separator. A3.1 asks for exclusion of credentials *on top
     of* the redaction path, so `redact.NamesACredential` is a predicate
     and the tool refuses rather than storing the wrong half.
   - A note is indexed inline rather than queued. Step 3's queue is for
     documents, where the slow part is the embedding; a note is one row
     and there is no embedder, so queueing it would mean telling
     somebody it is remembered while `recall` cannot find it.
   - The seams moved from `cmd/keystone` to `butlerstore`, beside the
     ledger. The read path could live in a closure in a main function;
     the write path makes claims — the speaker owns a statement made in
     a room, the scope is the Butler and never the pool — and a claim
     that is not tested is a comment.

   The live run found the thing the unit tests could not. Asked "who
   serviced the boiler", the Butler looked at the timeline, found
   nothing, and said so — it never reached for `recall`, because a
   boiler service *sounds* like a household event. Forcing the tool by
   name returned the note immediately, so the write and the index were
   right and the prompt was wrong. Two rounds to fix it: saying the two
   lookups are different only made it announce that it should check and
   then ask the person instead. What worked was making the rule about
   the ACT — call recall in the same turn, do not say you are about to,
   and never ask whether somebody told you something, because they told
   you so they would not have to remember.
3. **The job runner and indexing**, coordinator-gated.
4. **Inference capture**, with A3.1's five rules as five tests written
   first.
5. **The digest**, which A3 requires and which already has a store and
   an endpoint — it is the control surface for step 4, and step 4 should
   not ship without it.
6. **Embeddings**, measured against keyword on this household's own
   data.
7. **The shared pool's screen** — A5's naming of which Butlers and which
   models gain access, which is a return value with nowhere to appear.

## Two things to change with them, not before

- `keystone/butler/household.go:253` and `:658` tell the model it has no
  memory between conversations. True today. It becomes a lie the moment
  step 1 lands, and a model told it cannot remember will contradict
  material sitting in its own context.
- ~~The Console's Memory tab currently shows an empty cabinet with no
  explanation.~~ Records exist now, so "has not been told anything worth
  remembering yet" is true when it says it. The tab was also asking for
  every Butler's material while claiming to show one Butler's; it now
  asks for the one it names.

## What is most likely to go wrong

**The cloud crossing.** The shared pool is the one place a local
Butler's material can reach a cloud Butler's model. A4 holds today
because retrieval is scoped by Butler; the moment promotion exists, A5's
disclosure sentence is the only thing standing between "we are
vegetarian" and a hosted provider. It needs the acceptance test W031
already has for the assignment path, extended to the pool.

**Inference poisoning.** A household event is attacker-influenceable by
definition — a sensor, an app, another person's phone wrote it. An
inference drawn from one and stored as a preference is a durable,
retrievable statement that a later session will act on. A3.1 names this;
the untrusted envelope the household gateway already applies has to
survive into extraction.

**Unbounded growth.** Compaction exists and nothing schedules it.
