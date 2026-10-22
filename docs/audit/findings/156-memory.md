# Shared memory, audiences and semantic recall

- **Issue:** #156
- **Status:** complete
- **Audited:** 2026-09-10, commit `787ad6d` (`butlers-on-the-live-path`)
- **Acceptance criteria:** AC01, AC11

Written by the audit slice for
[#156](https://github.com/chickenrat17/wildstone-homelab/issues/156).
**This file belongs to that slice alone** — no other session writes
here, which is what keeps parallel audits from colliding. See
[`../README.md`](../README.md) for how to run one and
[`../gap-report.md`](../gap-report.md) for the register.

`testing/e2e` lints this file's shape. Run
`cd testing/e2e && go test -run TestAuditRegister ./...` before you
commit.

---

## How this slice was evidenced

The live deployment described in
[`../runtime-access.md`](../runtime-access.md) **could not be reached
from this session**. This slice ran in an ephemeral cloud container,
not on the homelab host: `systemctl status keystone` fails with
*"System has not been booted with systemd as init system"*,
`/var/lib/wildstone/` does not exist (so the claim-code token could not
be read), and `curl http://127.0.0.1:9441/api/v1/butlers` returns HTTP
`000`. No `wild` binary is installed. Everything in `runtime-access.md`
that depends on the running service is therefore out of reach, and the
two checks that genuinely need it are recorded `unverified` below
(F-156-14, F-156-17) rather than guessed at.

What replaced it, and what it is worth: the store and the seams were
**executed** against a real SQLite database built by the repository's
own migrations, driven through the production code path — a scratch
`main` outside the repository, importing `keystone/runtimedb`,
`keystone/butlerstore` and `keystone/butler` with a `replace` directive.
No repository file was changed. That is enough to settle what the code
does; it is not evidence about the household's actual data, and nothing
below claims it is. Where a finding rests on executed behaviour the
evidence line says **executed**, and the transcript is quoted inline.

Three existing tests were run unmodified and are cited where relevant.

**Housekeeping:** every probe wrote to a throwaway `os.MkdirTemp`
database, discarded with the container. Nothing was written to the
household's memory, because nothing in this session could reach it.

---

## Requirements

| # | Baseline | Requirement | Status | Evidence |
|---|---|---|---|---|
| 1 | §6, §7, §12 | One shared memory service; every authorized Butler retrieves the same permitted facts; no agent-exclusive silo | **missing** | F-156-01, F-156-03 |
| 2 | §6 | Audiences: Personal / Household / Selected people / Private | **missing** | F-156-02 |
| 3 | §6 | The subject of a memory is not automatically in its audience | **missing** | F-156-02 — no mechanism; the gift-list denial is vacuous |
| 4 | §6 | Default audience follows the source | **missing** | F-156-03 — the audience is a constant, not derived |
| 5 | §6, §13 | Semantic recall is required; the product is not prescribed | **missing** | F-156-04, F-156-05 |
| 6 | §6 | Vector index finds related records when wording is fuzzy | **missing** | F-156-04 |
| 7 | §6 | Record shape: stable ID, household, subject, audience, source, contributor, creation + event dates, version, status, sensitivity/retention | **partial** | F-156-06 |
| 8 | §6 | Status: current, superseded, disputed, expired, deleted | **partial** | F-156-06 |
| 9 | §6 | Automatic memory: remember useful stated preferences, decisions, commitments | **missing** | F-156-16 |
| 10 | §6 | Avoid indiscriminate transcript or inbox ingestion | **implemented** | Nothing ingests transcripts today — see *Design gaps* for the parked direction |
| 11 | §6 | Visible memory activity log | **missing** | F-156-09 |
| 12 | §6 | Editor, "remember this", "forget this" | **implemented** | F-156-12 |
| 13 | §6 | Controls for source scope, retention and sharing | **missing** | F-156-10 |
| 14 | §6 | Corrections preserve attribution | **implemented** | F-156-07 — attribution on the surviving record is correct |
| 15 | §6 | Version checks prevent two Butlers silently overwriting each other | **partial** | F-156-07 |
| 16 | §6 | Conflicting household claims remain attributed and unresolved until clarified | **missing** | F-156-07 — the losing claim is discarded |
| 17 | §6 | Invalidate cached context after corrections or permission changes | **partial** | F-156-18 |
| 18 | §6 | Deletion removes material from active memory, derived indexes, summaries and caches | **partial** | F-156-12 (store and index) + F-156-18 (the replayed transcript is not a cache this clears) |
| 19 | §6 | Suppress automatic re-ingestion from the same source | **implemented** | F-156-12 |
| 20 | §6 | Disclose backup retention limits | **unverified** | F-156-14 |
| 25 | §6 | Restore reapplies deletion records before serving restored data | **missing** | F-156-19 |
| 21 | §6 | No useful semantic match means no supported recollection | **partial** | F-156-15 |
| 22 | §6 | Retrieval authorization filters by person, household, agent grant **and response audience** | **partial** | F-156-08 — response audience is not a filter |
| 23 | §6 | Personal export, provenance included, no other person's private data | **implemented** | F-156-13 |
| 24 | §6 | Retrieval quality is measured | **partial** | F-156-11 |

## Acceptance criteria

| AC | Result | Evidence |
|---|---|---|
| **AC01** | **fails** — the successful-retrieval half is impossible and the denied-disclosure half is vacuous | F-156-01, F-156-02, F-156-03 |
| **AC11** | **fails in part** — correction, index invalidation and re-ingestion suppression all hold; version handling, disputed-claim retention, cache invalidation and restore-replay of ordinary forgets do not; backup retention unverified | F-156-07, F-156-12, F-156-14, F-156-18, F-156-19 |

### AC01, run end to end

Driven through `butlerstore.MemorySeams` — the only write path a
conversation has — then read back six ways. Executed:

```
== AC01: gift idea saved by <member-a> through Butler A ==
  remember via Butler A: err=<nil>
  stored seq=1 owner="<member-a>" audience="private" butler="alfred" label="alfred" subject="jude"
  <member-a> via Butler A (author, same butler)  retrieve=1 recall=1
  <owner> via Butler B  <-- AC01 REQUIRES a hit retrieve=0 recall=0
  <owner> via Butler A                         retrieve=0 recall=0
  <member-a> via Butler B <-- same person, other butler retrieve=0 recall=0
  Jude via Butler B    <-- AC01 requires NO hit retrieve=0 recall=0
  Jude via Butler A    <-- AC01 requires NO hit retrieve=0 recall=0
```

Jude is denied, and that is not a pass. Jude is denied because
**everybody** is denied except the person who spoke, through the one
Butler they spoke to. AC01 asks for a boundary that admits <owner> and
refuses Jude; what exists is a wall with one door. Recording the
denial as satisfied would be recording the absence of a mechanism as
the presence of one.

### AC11, run end to end

```
== AC11: concurrent conflicting writes + correction ==
  base seq=4
  correction A (<owner>): seq=5 err=<nil>
  correction B (<member-a>, same base): seq=0 err=runtimedb: memory 4 was already corrected or forgotten
  base record now: superseded_by=5 envelope_version=1

== AC11b: forget a source-derived entry ==
  before forget: retrieve=1 recall=1
  forget: err=<nil>
  after forget:  retrieve=0 recall=0
  re-remember, SAME source: err=butler: this was forgotten and will not be stored again
  re-remember, DIFFERENT source: err=<nil> (nil = it came back)
```

## Findings

### F-156-01 — Memory is siloed per Butler, and per person; switching agents loses access

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/memoryscopes.go`; 11d82f0; `TestPersonalMemoryFollowsItsPersonAcrossAgents`.

- **Audit status:** missing
- **Audit evidence:** executed (above) + `keystone/runtimedb/butlermemory.go:600-609` (`RetrieveMemory` ANDs `scopeClause("butler", …)` into the candidate SQL), `keystone/runtimedb/butlerscope.go:56-65`, `keystone/butlerstore/memory.go:140-151`; test `TestARememberedNoteBelongsToTheSpeakerAndToOneButler` (`keystone/butlerstore`, run, PASS).

Confirms established finding 1 in #151, and it is worse than reported
there. #151 says Butler B cannot read Butler A's material. It is also
true that **the same person cannot reach their own note through a
different Butler**: `<member-a> via Butler B` returns zero. §12 says
"switching agents preserves access to permitted facts", and switching
agents currently loses all of them.

Two independent ANDed conditions produce this, and both have to change:

- the `butler` scope, `(butler = ? OR butler = '')`, and
- the audience, which the only write path pins to `private` with the
  owner set to the speaker (F-156-03).

The `Butler` column is not a bug — ADR-W031 A4 asks for exactly it, on
purpose, for material like a health Butler's notes. The baseline
supersedes that as the **default** for ordinary durable facts. See
*ADR amendments required*.

### F-156-02 — The audience vocabulary is two values where the baseline needs four

- **Current status:** superseded
- **Current evidence:** The old four-audience vocabulary was replaced by Agent-local, Personal, Household and session context, each intersected with dynamic audience rows.
- **Superseded by:** docs/agent-system-design.md §11 memory scopes

- **Audit status:** missing
- **Audit evidence:** executed — `RecordMemory` with each candidate audience; `keystone/runtimedb/migrations/0023_butler_memory.sql:42` `CHECK (audience IN ('private','shared'))`; `keystone/runtimedb/butlermemory.go:130-132`.

```
  audience="private"   accepted=true
  audience="shared"    accepted=true
  audience="adults"    accepted=false  ... needs audience private|shared, got "adults"
  audience="household" accepted=false
  audience="selected"  accepted=false
```

Confirms established finding 2 in #151. AC01's adults-only gift list
has no representation, and neither does §6's *Selected people*.
`private` conflates §6's **Personal** and **Private** — they differ in
who may be assisted and through which destination, and one column
value cannot carry both.

The subject-is-not-in-the-audience rule (§6, the gift-list case) has
no mechanism at all: `subject` narrows retrieval when a caller asks for
it and never excludes anyone. It is only unreachable to Jude today
because it is unreachable to everyone.

### F-156-03 — Nothing in production can write a shared or pooled record; the sharing mechanism is unwired

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/memoryscopes.go`; 11d82f0; `TestPersonalMemoryFollowsItsPersonAcrossAgents`.

- **Audit status:** missing
- **Audit evidence:** `keystone/butlerstore/memory.go:140-151` (audience and Butler are constants in the only production writer); `grep` for callers outside `runtimedb` — `PromoteMemoryToSharedPool` and `RecordButlerMemory` have **no non-test callers** (`keystone/runtimedb/butlerscope.go:283,305`); no route for either in `keystone/api/api.go:352-358`.

This is the audit-README trap, and it caught the sharing half of the
subsystem. `PromoteMemoryToSharedPool` is complete, transactional, and
computes a disclosure preview naming the cloud Butlers that would gain
access. Reading it says sharing is implemented. Nothing calls it —
no handler, no route, no Console control — so **the mechanism exists
and is unwired**.

The consequences compound:

- `Remember` hardcodes `Audience: private`, `Owner: member`,
  `Butler: butlerName`. There is no branch that produces anything else.
- `CorrectMemory` inherits the audience and can only narrow it
  (`butlermemory.go:339-345`).
- The API exposes retrieve, export, correct, forget and digest — there
  is no record-creating or promoting endpoint.

So on this deployment every memory record is private-to-one-person and
held-by-one-Butler, permanently, with no path to any other state. §6's
"Default audience follows the source" is not a defaulting rule here;
it is the only outcome.

### F-156-04 — Semantic recall cannot retrieve: candidates come only from the FTS match

- **Current status:** open
- **Current evidence:** Partially delivered in `9537d63be6fd995eee821170a00f2e01ab143fc4` (#575); retained as open until the remaining semantic integration is verified. `SearchIndex` in `keystone/runtimedb/butlerindex.go` now unions a vector candidate query (same permission predicate, cosine floor `minSemanticSimilarity`) with the FTS candidates, so a vocabulary-mismatched chunk is retrieved when its embedding is close; covered by `TestSemanticCandidates*`. Still open: no embedder produces embeddings (F-156-05), and the `recall` callers pass none.
- **Current owner:** chickenrat17
- **Tracking:** #311

- **Audit status:** missing
- **Audit evidence:** executed; `keystone/runtimedb/butlerindex.go:225-241` — the candidate query is `FROM butler_index_fts … WHERE butler_index_fts MATCH ?`, and the cosine term at line 283 only adjusts `h.Score` on rows FTS already returned.

Confirms established finding 3 in #151, by execution rather than by
reading. A document was indexed with an embedding, then queried with an
**identical** embedding and non-overlapping words:

```
keyword overlap, no embedding              -> 1 hits
NO keyword overlap, identical embedding    -> 0 hits
NO keyword overlap, no embedding           -> 0 hits
```

A perfect vector match retrieves nothing. The embedding column can only
reorder what bm25 already found, so §6's "find related records when
wording is fuzzy" is exactly the case it cannot serve. `ftsQuery`
ORs the terms (`butlerindex.go:305-320`), which widens the keyword half
usefully — and does nothing for a query that shares no term at all.

The fix shape #151 gives is right and worth restating because it is the
part an embedder alone does not deliver: **a union candidate query —
keyword and nearest-neighbour, both under one permission clause**, so
the vector half never becomes a path that ranks before it filters
(`butlerindex.go:198-206` is explicit that no such path may exist).

### F-156-05 — There is no embedding client; the index job queue has neither producer nor consumer

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlermemory.go` has no embedding client producer or index-job consumer; the semantic path is absent.
- **Current owner:** chickenrat17
- **Tracking:** #311

- **Audit status:** missing
- **Audit evidence:** `grep -rn 'QueueIndexJob\|ClaimIndexJob\|FinishIndexJob\|RecordIndexMeasurement' --include=*.go` returns **no callers outside `keystone/runtimedb`**, tests included for the first three; `IndexDocument.Embedding` is set at exactly one non-test site — `keystone/butlerstore/memory.go:185-189` — where it is left nil.

Confirms established finding 4 in #151. `butler_index_jobs`, its
priority scheme and its claim/finish protocol are complete and dead.
`butlerstore/memory.go:172-181` says so in its own comment: *"there is
no embedder yet"*. Every row in `butler_index` therefore has
`embedding IS NULL`, and the hybrid path in `SearchIndex` has never
run against real data.

### F-156-06 — The record shape is missing household, version, and sensitivity/retention; two statuses do not exist

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlermemory.go` records scope and provenance but does not yet expose a complete sensitivity/retention and disputed-status contract.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** partial
- **Audit evidence:** `keystone/runtimedb/migrations/0023_butler_memory.sql:28-80`; `keystone/runtimedb/butlermemory.go:67-97`; `grep -rni 'disputed' keystone/runtimedb/` returns nothing.

Against §6's field list:

| §6 field | State |
|---|---|
| stable ID | present — `seq` |
| household | **absent** — no column; a single household is assumed throughout |
| subject | present |
| audience | present but two-valued (F-156-02) |
| source reference | present — `source_kind` + `source_id` |
| contributor | present — `author` |
| creation date | present — `created_at` |
| relevant event date | present — `observed_at` |
| version | **absent** — `envelope_version` is the task-envelope schema constant `TaskEnvelopeVersion`, identical on every row (observed `1`), not a per-record version |
| status | **partial** — *current*, *superseded* (`superseded_by`) and *deleted* (`forgotten_at`) are derivable; **disputed** and **expired** do not exist |
| sensitivity / retention | **absent** — `safety_relevant` is a digest-urgency flag, `permission_label` is an access label; neither is a retention policy, and no record carries one |

*disputed* is the one whose absence changes behaviour rather than
metadata: see F-156-07.

### F-156-07 — Corrections have no version check, and a conflicting claim is discarded rather than kept as disputed

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlermemory.go:CorrectMemory` supersedes an eligible row but has no caller-supplied version precondition or disputed-claim state.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** partial
- **Audit evidence:** executed (AC11 transcript above, plus five concurrent trials); `keystone/runtimedb/butlermemory.go:336-341` reads `GetMemory` and tests `Eligible()` **outside** the transaction opened at line 381; the write at line 391 is `UPDATE butler_memory SET superseded_by = ? WHERE seq = ?` with no `AND superseded_by IS NULL`.

Two halves, and they need separating.

**What holds.** Attribution on the surviving record is correct: the new
row carries the correcting person as `author`, the old row is kept and
stamped `superseded_by`, derivatives are invalidated, and the stale
text is deleted from the index in the same transaction. Five runs of
two genuinely concurrent `CorrectMemory` calls on one record accepted
exactly one each time; no double-supersede was observed.

**What does not.** §6 asks for two distinct things and neither is
present:

- *"Use version checks to prevent two Butlers silently overwriting each
  other."* There is no version to check. The protection observed is a
  read-then-write eligibility test plus whatever serialization the
  store happens to provide, and the `UPDATE` carries no guard of its
  own. It was not observed to fail — and "was not observed to fail"
  is not the same claim as "cannot", which is what a version check
  would let the code assert. Marked partial on that distinction, not on
  a reproduced defect.
- *"Conflicting household claims remain attributed and unresolved until
  clarified."* The losing correction is **destroyed**. <member-a>'s
  competing claim came back as `err=... already corrected or forgotten`
  and left no trace: no record, no digest entry, no *disputed* status
  for the household to clarify later. A household disagreement is
  currently resolved by whichever Butler committed first, silently.

### F-156-08 — `recall` reads private memory into a group conversation; response audience is not part of retrieval authorization

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/memorydisclosure.go`; 0562893; `TestDisclosurePolicyAppliesOnTopOfAudience`.

- **Audit status:** missing
- **Audit evidence:** executed, through the real gateways; `keystone/butler/household.go:223-227` (`NewHouseholdGroupGateway` sets `group = true` and registers the same tool map), `household.go:248-259` (`recall` registered unconditionally on the seam), `keystone/butlerstore/memory.go:85-88` (the seam closes over `member` alone); `runtimedb.IndexQuery` has no audience or destination field (`butlerindex.go:164-181`).

<member-a> states something private in a direct conversation with Alfred,
then a room asks Alfred about it:

```
direct remember: rejected=false
  kept as a fact, about <member-a>: Seeing the doctor about the back pain on Tuesday.

group gateway tools: [describe_capabilities recall remember]
recall IN A ROOM: rejected=false
  - Seeing the doctor about the back pain on Tuesday. [about: <member-a>]
```

§6's retrieval-authorization sentence lists four filters — person,
household, agent grant, **response audience**. The first three are in
the SQL. The fourth is absent, and there is no field on `IndexQuery` or
`MemoryQuery` through which a caller could supply it.

The gap is specifically memory's. The group gateway was deliberately
built as a separate constructor so `get_household_events` would refuse
another member's events structurally rather than by prompt
(`household.go:208-222`) — and memory never received the equivalent
treatment. `MemorySeams` is handed the same `member` either way, so the
room inherits the speaker's whole private store.

Cross-slice pointer: this also bears on AC09 (#155). It is recorded
here because the missing filter is §6's and lives on the memory query.

### F-156-09 — The memory activity log records only shared changes, and no production path creates a shared record

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/memoryscopes.go`; 11d82f0; `TestPersonalMemoryFollowsItsPersonAcrossAgents`.

- **Audit status:** missing
- **Audit evidence:** executed — `PendingMemoryDigest` returned `0` after a private write and `1` after a shared one; `keystone/runtimedb/butlermemory.go:180-186, 431-437, 474-480` (every `digestTx` call is inside `if audience == shared`); `MarkMemoryDigestDelivered` has **no callers outside `runtimedb`**; F-156-03 establishes that nothing in production writes a shared record.

Composing those: `butler_memory_digest` can never receive a row on this
deployment. `GET /api/v1/butler/memory/digest` and the Console Memory
page (`console/src/views/Memory.tsx:26`) will show an empty list
forever, whatever the household does.

Two further problems sit behind that one, and both survive fixing
F-156-03:

- **It is a shared-changes digest, not an activity log.** §6 asks for a
  *visible memory activity log*. A person's own corrections and
  forgets of their own records are deliberately excluded — reasonably,
  for a digest other adults read — so after F-156-03 there would still
  be no record anyone can inspect of what happened to their own memory.
- **Nothing delivers it.** `delivered_at` is never written, so the
  endpoint is a growing list of everything that ever changed rather
  than a digest, and no Butler or routine sends one.

### F-156-10 — Retention and source-scope controls do not exist; migration 0038's columns have no reader

- **Current status:** open
- **Current evidence:** Migration 0079 removes unused 0038 transcript settings; `keystone/runtimedb/butlermemory.go` still lacks a full source-scope/retention control contract.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** missing
- **Audit evidence:** `grep -rn 'index_conversations\|transcript_retention_days' --include=*.go .` returns **nothing**; `keystone/runtimedb/butlersettings.go:44-56` has neither field and `Validate` (line 64) checks neither; nothing schedules `CompactIndex` — its only caller is the manual `POST /api/v1/butler/index/compact` handler (`keystone/api/api.go:353`).

§6 asks for controls over source scope, retention and sharing. None of
the three is reachable:

- **Source scope** — no per-source or per-connection memory setting
  exists.
- **Retention** — migration 0038 added `index_conversations` and
  `transcript_retention_days` to `butler_runtime_settings`. Neither is
  in the `ButlerSettings` struct, neither is selected by any query, and
  neither is exposed by the API or Console. #151 already records the
  0038 work as parked; what this adds is that the migration's own
  comment asserts *"the bounds for these two live in
  `ButlerSettings.Validate` alone"* — and `Validate` does not mention
  them. The schema's stated backstop does not exist on either side.
- **Sharing** — F-156-03.

Nothing ages memory out on a clock either: no timer, ticker or routine
calls `CompactIndex`, so retention today is "forever, unless an
administrator posts to the compact endpoint".

### F-156-11 — The retrieval-quality fixture cannot detect the defect it is meant to measure

- **Current status:** open
- **Current evidence:** Partially delivered in `9537d63be6fd995eee821170a00f2e01ab143fc4` (#575); retained as open until the remaining semantic integration is verified. `TestRetrievalQualityIsMeasuredAndRecorded` in `keystone/runtimedb/butlerindex_test.go` now includes queries sharing no word with their target and asserts hybrid recall is strictly above keyword recall. The memory-level fixture under `keystone/butler/memory_test.go` still has no semantic case, pending the embedder (F-156-05).
- **Current owner:** chickenrat17
- **Tracking:** #311

- **Audit status:** partial
- **Audit evidence:** `keystone/runtimedb/butlerindex_test.go:257-345` — all six fixture queries share a content word with their target document (`"when is bin day"` → `"bin day is Wednesday morning"`), so FTS returns candidates for every one; the recorded `hybrid` strategy row is produced from the same keyword-derived candidate set as `keyword`.

The measurement table exists and is populated, and the Console reads
it — that part works. What it measures is reranking quality over a
corpus where the keyword half never misses. Run against a query with no
lexical overlap it would record 0% for *both* strategies, which is the
number F-156-04 is about. As written, a `hybrid` row reporting high
recall is evidence that reordering works, and is not evidence that
semantic retrieval does — and it is the row a reader would reach for
to conclude the opposite.

### F-156-12 — Forgetting, index invalidation and re-ingestion suppression work

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/memoryscopes.go`; 11d82f0; `TestForgetSurvivesBackupRestore`.

- **Audit status:** implemented
- **Audit evidence:** executed (AC11b transcript above); `keystone/runtimedb/butlermemory.go:441-487`; test `TestForgottenMemoryLeavesTheIndex` (`keystone/runtimedb/butlerindex_test.go:151`).

Recorded because AC11 asks for it and because most of this report is
about what is absent. Forgetting a record removes it from retrieval and
deletes its searchable chunks **in the same transaction**; correcting
does the same for the superseded text; derivatives of both go with
them; and the source is suppressed by a hash of the normalized content
so the same claim cannot re-enter from the same conversation. Saying
the sentence again through the same source is refused with
`butler: this was forgotten and will not be stored again`.

The one boundary worth stating plainly, because it is a decision rather
than a defect: suppression is keyed on *(source_kind, source_id,
content_key)*, so the same claim from a **different** conversation is
stored again. The forget handler says so in its response, which is the
right behaviour — a new statement is a new decision — but it means
"forget this" is not "never store this".

Scoped deliberately: this finding covers the **record store, the index
and re-ingestion**, which is what it claims and what was executed.
§6's deletion sentence also says *caches*, and it is not clean there —
the replayed conversation transcript still carries the text
(F-156-18) — nor across a restore (F-156-19). Requirement row 18 is
`partial` for that reason; this finding stays `implemented` for the
three mechanisms it names.

### F-156-13 — Personal export honours current permissions

- **Current status:** implemented
- **Current evidence:** `keystone/api/handlers_portal_memory.go`; 11d82f0; `TestMemoryShowsYoursAndTheHouseholdsOnly`.

- **Audit status:** implemented
- **Audit evidence:** `keystone/runtimedb/butlermemory.go:625-660` — the query returns the reader's own records plus live shared ones, and other people's forgotten or superseded records are excluded; `keystone/api/handlers_butler_memory.go:88-95` passes the acting member rather than a requested one.

### F-156-14 — Backup retention behaviour for forgotten memory is unverified

- **Current status:** open
- **Current evidence:** `TestForgetSurvivesBackupRestore` proves journal replay for an explicit forget, but backup retention of all historical derived material remains unverified.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** unverified
- **Audit evidence:** not established. `keystone/runtimedb/restore.go:216-266` replays `deletion_journal` entries over restored state, which is the mechanism AC11 asks about — but whether this deployment takes backups containing memory rows, what they retain, and whether the household is told, needs the live host.

Narrowed after review. This finding is now **only** the host-dependent
half: what the backups contain and whether their retention is
disclosed. Whether the replay mechanism has anything to replay for an
ordinary forget is establishable from the code, and it does not — that
is F-156-19, recorded `missing`, not hidden here.

**What would settle it:** on the homelab node, inspect the backup
artefacts for `keystone.db` (`GET /api/v1/backups`, and the run records
under `/var/lib/wildstone/keystone`), and confirm whether the household
is shown a retention period for material it has forgotten. None of that
is reachable from this container — see *How this slice was evidenced*.

### F-156-15 — "No useful semantic match" is answered correctly, for the wrong reason

- **Current status:** open
- **Current evidence:** `keystone/butler/memory.go:handleRecall` can report no useful result, but it cannot prove semantic miss quality while retrieval is lexical-only.
- **Current owner:** chickenrat17
- **Tracking:** #311

- **Audit status:** partial
- **Audit evidence:** `keystone/butler/memory.go:257-262` — an empty result returns `"nothing remembered about that"` with an explicit comment against hedging; `keystone/butler/memory.go:220-224` — the `recall` tool description tells the model to search *"ALWAYS after a household lookup comes back empty"*.

The behaviour §6 asks for is present at the prompt boundary: no match
produces a plain "nothing remembered", not an invented recollection,
and the model is steered towards live tools. Marked partial because the
sentence is *"No **useful semantic match** means no supported
recollection"* — and with F-156-04 unfixed, every semantic-only miss is
indistinguishable from a fact that was never stated. The Butler is
truthful about having found nothing and wrong about there being nothing
to find.

### F-156-16 — Automatic memory does not exist; capture is model-initiated only

- **Current status:** open
- **Current evidence:** `keystone/butler/memory.go` offers model-initiated remember; automatic capture and retention policy for ordinary conversation history is still undefined.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** missing
- **Audit evidence:** `keystone/butler/memory.go:355-378` (`handleRemember` is a tool the model must call), `keystone/butler/household.go:261-277` (the only registration); no extraction pass over sessions exists — `QueueIndexJob` has no producer (F-156-05) and nothing reads `index_conversations` (F-156-10).

§6: *"Remember useful stated preferences, decisions, and ongoing
commitments."* Today a preference is kept only when the model decides,
mid-turn, to spend a tool call on it — which the `remember` description
scopes to *"when they say to remember, note or keep something"*. A
stated preference nobody asks to be kept is lost.

This is the requirement most in tension with §6's next sentence
(*"Avoid indiscriminate transcript or inbox ingestion"*), and the two
together are the decision noted under *Design gaps* rather than a
straightforward fix.

### F-156-17 — Whether the live deployment matches this report is unverified

- **Current status:** open
- **Current evidence:** The household host is unreachable from this worktree, so `docs/audit/slice-24-rollout.md` records no live memory inspection.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** unverified
- **Audit evidence:** not established. `systemctl status keystone` → *"System has not been booted with systemd as init system"*; `/var/lib/wildstone/` does not exist; `curl http://127.0.0.1:9441/api/v1/butlers` → HTTP `000`; no `wild` binary on `PATH`.

Everything above is established against commit `787ad6d`, which #151
records as the code deployed to the live keystone. This finding exists
so that is a stated assumption rather than a silent one.

**What would settle it** — corrected after review, because the check
first written here could not have answered the question.
`GET /api/v1/butler/memory` with no `butler` parameter widens only the
**Butler** scope to `AllButlers`; the handler still passes
`actingMember(id)` (`keystone/api/handlers_butler_memory.go:74-79`) and
`RetrieveMemory` still applies `audience = 'shared' OR owner = ?`
(`keystone/runtimedb/butlermemory.go:594-596`). Other people's private
records stay invisible to it, so it cannot establish anything about
*every* live record.

What would actually settle it, on the homelab host: copy the database
out per `runtime-access.md` (`sudo cp … /tmp/audit.db`) and query it
directly —
`SELECT butler, audience, COUNT(*) FROM butler_memory WHERE forgotten_at = '' GROUP BY 1, 2;`
to confirm no live record sits in the shared pool or carries a
non-`private` audience, and `SELECT COUNT(*) FROM butler_memory_digest;`
to confirm the activity log is empty. Then, for behaviour rather than
schema, `wild butler ask -surface family` / `-surface admin` with the
same recall question, to reproduce AC01's cross-Butler miss on the real
models. A copy answers the schema question; only the CLI answers the
behavioural one.

### F-156-18 — A corrected or forgotten fact survives in the conversation it came from

- **Current status:** open
- **Current evidence:** `keystone/runtimedb/butlermemory.go:ForgetMemory` invalidates memory, but the source conversation text needs a separate retention/redaction decision and test.
- **Current owner:** chickenrat17
- **Tracking:** #317

- **Audit status:** partial
- **Audit evidence:** `keystone/butlerlive/serve.go:89-91,198-202` — `systemFor` runs per turn and calls `MemoryPreamble`, which reads live SQL (`keystone/butlerstore/memory.go:68-83`), so the injected block is never stale; but `serve.go:106` replays `t.History` into the same turn, and nothing filters it against corrections.

§6: *"Invalidate cached context after corrections or permission
changes."* The durable half is right — the preamble and every `recall`
result are rebuilt from the store on each turn, so a correction or a
forget takes effect on the next turn with no cache to purge, and a
revoked record cannot be served from a warm copy.

What is not invalidated is the conversation transcript. A fact stated,
corrected, and then asked about again inside one ongoing conversation
is still present verbatim in the replayed history, and the model may
answer from it. Prompt caching aside, that is cached context in the
sense §6 means. Marked partial rather than missing because the
mechanism that matters for cross-conversation continuity is sound.

**This is forgetting as well as correction.** Widened after review, and
the forget case is the worse of the two: §6's deletion sentence asks
that forgotten material be removed from "active memory, derived
indexes, summaries, **and caches**". `ForgetMemory` clears the record
and the index in one transaction (F-156-12) and does not touch the
session history, so a record forgotten through the Console mid-
conversation is still in front of the model for the rest of that
conversation. A person who has just been told "forgotten" can be told
it back on the next turn.

### F-156-19 — An ordinary forget writes no deletion-journal row, so a restore brings it back

- **Current status:** implemented
- **Current evidence:** `keystone/runtimedb/memoryscopes.go`; 11d82f0; `TestForgetSurvivesBackupRestore`.

- **Audit status:** missing
- **Audit evidence:** `keystone/runtimedb/butlermemory.go:441-487` — `ForgetMemory` marks the record, suppresses the source and deletes the index chunks, and never calls `journal`. The only memory journal write in the tree is `keystone/runtimedb/offboarding.go:194-196`, inside `OffboardMember` and gated at line 186 on `Audience == private && Owner == member`. The exported `Journal` (`offboarding.go:255`) has **no callers anywhere**. `ReconcileRestore` (`keystone/runtimedb/restore.go:216-266`) replays `deletion_journal` rows and re-applies `ForgetMemory` for `subject_kind = "memory"` at line 261 — over a journal that ordinary forgets never wrote to.

Raised by the Codex review on this PR, verified against the code, and
it is right — including about the audit mistake. I had folded this
into F-156-14 as `unverified`, and it needs no live host at all: three
greps settle it.

§6: *"Restore procedures must reapply deletion and revocation records
before serving restored data."* The replay machinery exists and is
correct. What it replays is a journal that only ever receives rows from
offboarding, and only for the offboarded member's own private records.
So:

1. <member-a> forgets a record through the Console. It is marked, its index
   chunks are deleted, its source is suppressed. No journal row.
2. The deployment is restored from a snapshot taken before step 1.
3. `ReconcileRestore` finds nothing to replay for that record. It comes
   back live, retrievable, and searchable.

The suppression row is restored away with it, so the source is not even
suppressed any more. This is the deletion guarantee failing across
exactly the event it was written for, and it sits alongside F-156-12 —
forgetting is airtight right up until a restore.

The fix shape, for whoever picks this up: journal inside
`ForgetMemory`'s existing transaction rather than at the call sites.
Doing it in the API handler would leave `InvalidateMemoryFromSource`
and `OffboardMember` writing their own rows, and a journal with three
writers is a journal that will be missing one.


## Design gaps

**Transcript ingestion: the parked direction contradicts §6.**
Migration 0038 adds `index_conversations` defaulting to **1**, and its
comment argues the case at length: *"Memory that holds only what you
dictate is barely memory."* §6 says *"Avoid indiscriminate transcript
or inbox ingestion"* and ADR-W030 A3 says *"Query permission does not
imply bulk indexing permission."* Default-on whole-conversation
indexing is the thing both sentences name.

The tension is real, not a drafting slip: F-156-16 is a genuine gap,
and indexing transcripts is the obvious way to close it. The middle
path §6 points at is **extraction** rather than ingestion — a pass that
proposes stated preferences, decisions and commitments as records with
provenance, which a person can see and reject — rather than putting
every sentence in a retrieval index. That is a decision for the owner,
and it decides whether 0038 is finished or reverted. #151 says #156
decides this; this slice's recommendation is extraction, and the
decision is recorded as open.

**Single-household assumption.** No `household` column exists anywhere
in the memory schema (F-156-06). For one household this costs nothing
today, and adding it later means a migration across every memory and
index row plus every retrieval clause. Worth deciding deliberately now
rather than discovering it.

**`private` carries two of §6's four audiences.** Personal ("authorized
Butlers assisting that person") and Private ("Butlers assisting the
specified person, through a suitable response destination") differ in
whether a suitable destination is required. Collapsing them is a
defensible simplification, but it should be a recorded choice, because
F-156-08 is exactly the failure the distinction exists to prevent.

**`CompactIndex` ages out explicitly remembered notes.** Migration 0038
states the rule — *"Explicitly remembered notes are NOT covered by it —
those were asked for, and they last until somebody forgets them"* — and
`butlerindex.go:378-388` deletes from `butler_index` by `observed_at`
with no exclusion for memory-backed chunks. Executed:

```
before compact: memory records=1, searchable chunks=2
CompactIndex(180d): pruned=0 aged=1 err=<nil>
after  compact: memory records=1, searchable chunks=1
  -> the explicit note still EXISTS but is no longer findable by recall
```

Filed here rather than as a finding because it is not reachable by
accident today — the only caller is the manual admin endpoint, and a
note's chunk is stamped `observed_at = now` at write time, so it takes
a year plus a deliberate `maxAgeDays` to bite. It becomes a live defect
the moment F-156-10's scheduled retention is wired, which is when the
code will do precisely what 0038 says it must not. Whoever implements
0038 should read this first.

## Cross-slice

- **#155 (identity and authorization) / AC09.** F-156-08 discloses one
  person's private memory into a group conversation. The missing filter
  is §6's, so the finding is here; the group-boundary half is #155's.
  Noted, not chased.
- **#157 (secrets) / AC04.** `remember` refuses any note naming a
  credential (`keystone/butler/memory.go:428-437`, `redact.NamesACredential`)
  — established finding 5 in #151. Confirmed present in the memory
  write path; the remediation shape (safe label in shared memory,
  opaque reference to the model) is #157's. Noted, not chased.
- **#158 (durable execution and lifecycle).** `butler_index_jobs` is a
  complete queue with no producer or consumer (F-156-05). Whatever
  runs background work for #158 is where its worker belongs; a second
  scheduler for indexing alone would be the wrong answer.
- **#153 (bot routing) / AC08.** AC08's *"switching Butlers preserves
  permitted shared knowledge"* is the same requirement F-156-01 fails.
  One fix serves both; the finding is here.

## ADR amendments required

**ADR-W031 A4 — "One Butler's material does not reach another Butler's
model"** (`docs/decisions/ADR-W031-butlers-are-user-created-specialists.md:97-110`).

A4 currently reads *"Memory recorded by a Butler is owned by that
Butler … There is no implicit sharing between Butlers, at any point,
for any reason."* Baseline §6 and §7 say the opposite for ordinary
durable facts: *"Every authorized Butler can retrieve the same
permitted facts"*, *"no agent-exclusive silo for ordinary durable
facts"*. The baseline supersedes it, so **A4 changes.**

The amendment should invert the default, not delete the isolation. A4's
worked case — the health Butler's material not reaching the nutrition
Butler's cloud model — is a real requirement the baseline still wants,
and §6 expresses it as *audience* and as the Model row's *privacy
constraints*, not as a per-agent owner. Suggested shape: durable memory
belongs to a person or the household and is retrievable by any Butler
whose grant and the record's audience both permit it; a per-Butler hold
becomes an **opt-in restriction** on a record, justified by its
sensitivity, rather than the state every record is written in. A5's
promotion flow then inverts too: the disclosure decision moves from
"promote out of a silo" to "restrict into one", and its
cloud-Butler-naming preview (`PreviewPoolPromotion`) is worth keeping
either way.

**ADR-W030 A3 — "A vector database product is not selected here"**
(`docs/decisions/ADR-W030-butler-continuity-durable-work-and-scoped-integrations.md:59`).

Baseline §6 accepted change: *"semantic recall is required in the agent
baseline. Vector-backed retrieval is no longer deferred."* §13: *"Semantic
retrieval is required, but the database product is not prescribed."*
A3's sentence is not wrong about the product and is being read as
deferring the capability. **Amend it** to say the capability is
required and the product is not prescribed — which also settles that
the existing choice (SQLite FTS5 plus an in-row embedding, both local)
remains acceptable, and that what has to change is the candidate query
(F-156-04) and the embedder (F-156-05), not the datastore.

**ADR-W030 A3 — audience vocabulary.** A3 and A3.1 are written
throughout in terms of private-versus-household-shared. §6 needs four
audiences. The two-value vocabulary should be recorded as superseded
in the same amendment, so F-156-02's schema change does not read as
contradicting the ADR it is implemented under.

**Not an amendment: ADR-W030 A3's digest.** A3's *"the digest includes
another adult's corrections and forgets of household-shared memory"* is
consistent with §6's activity log and narrower than it. F-156-09 is a
wiring gap and a scope gap against the baseline, not an ADR conflict.
