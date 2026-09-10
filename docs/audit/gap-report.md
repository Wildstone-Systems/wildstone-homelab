# Gap report: repository against the architecture baseline

Tracked by [#151](https://github.com/chickenrat17/wildstone-homelab/issues/151).
Written one section at a time by the audit slices; see
[`README.md`](README.md) for how to run one.

**Fill your section. Do not restructure the file.** Sections are
written in parallel and this has to merge.

Every claim carries evidence. Status is one of **implemented**,
**partial**, **missing**, **unverified** — and `implemented` means
something calls it, not that the code exists.

---

## Status

| Slice | Issue | Status | Audited |
|---|---|---|---|
| Bot routing and per-Butler chat connections | [#153](https://github.com/chickenrat17/wildstone-homelab/issues/153) | not started | — |
| Routine authoring, execution, approval integrity | [#154](https://github.com/chickenrat17/wildstone-homelab/issues/154) | not started | — |
| Identity and authorization | [#155](https://github.com/chickenrat17/wildstone-homelab/issues/155) | not started | — |
| Shared memory, audiences, semantic recall | [#156](https://github.com/chickenrat17/wildstone-homelab/issues/156) | not started | — |
| Secrets and privacy gateway | [#157](https://github.com/chickenrat17/wildstone-homelab/issues/157) | not started | — |
| Durable execution, delegation, lifecycle | [#158](https://github.com/chickenrat17/wildstone-homelab/issues/158) | not started | — |

---

## How to write a section

Copy this shape. A requirements table first, so the coverage is
countable; then the findings that need more than a row.

    ### <slice name>

    Audited <date> at <commit>. Issue #NNN.

    #### Requirements

    | # | Baseline | Requirement | Status | Evidence |
    |---|---|---|---|---|
    | 1 | §7 | One bot identity per Butler per platform | missing | `keystone/api/handlers_chat_message.go:300` — the address resolves to a Butler, but no binding stores which bot belongs to which |

    #### Acceptance criteria

    | AC | Result | Evidence |
    |---|---|---|
    | AC08 | fails | Ran two Butlers against one Telegram account; replies went out as … |

    #### Findings

    **F-NNN-01 — <one line saying what is wrong>**
    *Status* · *Baseline reference*
    What the baseline requires, what the code does, how you established
    it, and what it costs in practice. One or two paragraphs.

    #### Design gaps

    Where the code does something coherent that is not what was wanted.
    Decisions for the owner, not defects.

    #### Cross-slice

    Findings that belong to another slice. Note and point; do not chase.

    #### ADR amendments required

    Where the baseline supersedes an ADR and the ADR still says
    otherwise.

Finding IDs are `F-<issue>-<n>` — `F-153-01` — so a remediation issue
can cite one without ambiguity.

---

## Sections

### Bot routing and per-Butler chat connections

*Reserved for [#153](https://github.com/chickenrat17/wildstone-homelab/issues/153). Not yet audited.*

### Routine authoring, execution and approval integrity

*Reserved for [#154](https://github.com/chickenrat17/wildstone-homelab/issues/154). Not yet audited.*

### Identity and authorization

*Reserved for [#155](https://github.com/chickenrat17/wildstone-homelab/issues/155). Not yet audited.*

### Shared memory, audiences and semantic recall

*Reserved for [#156](https://github.com/chickenrat17/wildstone-homelab/issues/156). Not yet audited.*

Findings 1–4 on #151 were established on 2026-09-10 and are inputs to
this slice, not conclusions of it. Confirm them with evidence in the
table above rather than restating them.

### Secrets, privacy gateway and external processing

*Reserved for [#157](https://github.com/chickenrat17/wildstone-homelab/issues/157). Not yet audited.*

### Durable execution, recovery, delegation and lifecycle

*Reserved for [#158](https://github.com/chickenrat17/wildstone-homelab/issues/158). Not yet audited.*

---

## Summary

*Written once every slice is in, as the input to remediation planning.
Counts by state, the findings that block others, and the decisions
needed before any of them can be scheduled.*
