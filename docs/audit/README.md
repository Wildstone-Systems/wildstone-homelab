# Auditing the repository against the architecture baseline

The six findings files retain their September 10 observations under **Audit
status** and **Audit evidence**. Slice 24 added **Current status** and
**Current evidence** for each stable finding ID. Use the current fields for
implementation planning; the original prose, tables and runtime notes remain
historical evidence. The [gap report](gap-report.md) counts both sets.

How to run one audit slice of
[#151](https://github.com/chickenrat17/wildstone-homelab/issues/151).

Each slice is a separate GitHub issue and is meant to be run from a
**fresh session** with no memory of the others. That is deliberate.
A session that built something is the worst auditor of it: it knows
what the code was meant to do and reads that into what it does. The
cost of a fresh session is that everything it needs has to be written
down, which is what this directory is.

---

## Before you start

**For current remediation, work from `main`.** The original six audit sessions
used `butlers-on-the-live-path` on September 10, when it was ahead of `main`.
That branch instruction is historical; slices 1–23 have since landed on
`main`. Keep the original audit commit in each findings file when interpreting
its old evidence.

**Read, in this order:**

1. Your slice's issue — it carries the requirements verbatim, the
   acceptance criteria, starting points in the code, and the
   deliverable.
2. [`docs/agent-system-design.md`](../agent-system-design.md) and
   [`ADR-W033`](../decisions/ADR-W033-the-agent-design.md) — **the current
   authority.** Audit against these.
3. [`docs/agent-architecture-baseline.md`](../agent-architecture-baseline.md)
   — **historical input, not the current source of truth.** It triggered
   this audit and its section numbers are what the issues cite, so read
   the sections your issue names. But where it differs from the design
   contract, the contract wins.

   > **The baseline no longer supersedes ADR language.** That instruction
   > stood here while the baseline was the accepted statement of intent,
   > and it is now wrong in a way that changes findings. The clearest
   > example: baseline §8 lets an Agent activate a sufficiently specified
   > routine on its own, while the design contract requires human Owner
   > approval for activation. An auditor following the old order would
   > record conformance to a superseded architecture as a pass, and the
   > contract's requirement as a gap. See
   > [`docs/README.md`](../README.md) for the full precedence.
4. [#151](https://github.com/chickenrat17/wildstone-homelab/issues/151)
   — the ground rules and the findings already established, so you do
   not spend the session rediscovering them.
5. [`CONTRIBUTING.md`](../../CONTRIBUTING.md) — the checks CI runs and
   the ground rules that are not negotiable.

**Do not read `docs/butler-memory.md` as a source.** It was generated
by reading the code, and its plan was invented rather than derived. Its
inventory of `runtimedb` is accurate; nothing else in it is normative.

---

## The four states

Every requirement gets exactly one, and the difference between the last
two is the one that matters.

| State | Means |
|---|---|
| **implemented** | The behaviour exists and you have evidence of it. |
| **partial** | Some of the requirement holds. Say which part, and which part does not. |
| **missing** | The behaviour does not exist. |
| **unverified** | You could not establish either way. Say what would settle it. |

**`missing` is a finding. `unverified` is a gap in the audit.** Do not
use `unverified` to be polite about something you actually established,
and do not use `missing` for something you simply could not reach.

### Evidence

A claim without evidence is an opinion, and this whole exercise exists
because opinions about this code have been wrong twice.

- **Code evidence:** file and line, on a named commit.
- **Runtime evidence:** the command, and what came back. See
  [`runtime-access.md`](runtime-access.md).
- **Test evidence:** the test name, and whether you ran it.

Not evidence: a UI label, a function existing, a comment describing
intent, this document, the baseline, or the issue text.

### The trap that has already caught us twice

**Code that exists but nothing calls.** The entire Butler memory
subsystem — retrieval, indexing, records, correction, forgetting — was
complete in `runtimedb` and had **zero callers** outside its own
package. Reading it would tell you the feature was implemented. It did
nothing.

So for anything you mark `implemented`, check that something reaches
it. `grep` for the function outside its own package. If the only
callers are tests, the state is `missing`, and say why: the mechanism
exists and is not wired.

---

## Scope discipline

**Audit. Do not fix.** Even a one-line obvious fix. A finding with a
fix already applied cannot be prioritized against the others, and §13
is explicit that identity, authorization, memory visibility and privacy
come before anything that increases tool reach.

**Note cross-slice findings, do not chase them.** Findings ignore slice
boundaries — memory scope surfaces in the lifecycle slice, routine
grants surface in the authorization slice. Record it under
`Cross-slice` in your section with a pointer, and move on. Chasing it
is how one slice becomes six.

**Design gaps are not bugs.** Where the code does something coherent
that simply is not what was wanted, say so under its own heading. That
is a decision to put to the owner, not a defect to file.

---

## The deliverable

**Your own file: `findings/<issue>-<slug>.md`.** It already exists with
the right header and headings. No other session writes into it, and you
write into no other — that is what lets six slices run at once without
a merge conflict on every one of them.
[`gap-report.md`](gap-report.md) is an index and holds no findings.

Finding ids are `F-<issue>-<nn>`, numbered from 01 and **only ever
increasing**. Never renumber, even to close a gap: a remediation issue
may already cite an id, and a citation that quietly comes to mean a
different finding is worse than a missing number. Withdraw one by
striking it through in place with a line saying why.

`testing/e2e` lints the shape of all this and CI runs it. Before you
commit:

```bash
cd testing/e2e && go test -run TestAuditRegister ./...
```

It checks ids, statuses, evidence lines and the index — not prose, and
not whether a finding is any good.

Then comment on your issue with a summary — counts by state, the two or
three findings that matter most, and anything that needs a decision
rather than a fix. Close it. Remediation issues get created from the
report afterwards, in one pass across all six, so they can be
prioritized against each other rather than in the order the audits
happened.

---

## Housekeeping

- **Clean up test data.** An audit that leaves records in the live
  household's memory has changed the thing it was measuring. Note what
  you created and confirm it is gone.
- **Synthetic secrets only** (§17). Never put a real household
  credential in a report, an issue, or a commit.
- **Do not deploy during an audit.** Building and installing a binary
  restarts the control plane the other slices may be reading. If you
  genuinely need a code change to observe something, say so on the
  issue first.
