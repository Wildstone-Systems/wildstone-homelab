# Gap register: repository against the architecture baseline

Tracked by [#151](https://github.com/chickenrat17/wildstone-homelab/issues/151).
See [`README.md`](README.md) for how to run a slice.

**One file per slice, and no session writes into another's.** The
slices run in parallel from independent sessions, so a single shared
document would conflict on every merge — not because the sections
overlap but because the file does. This page is an index and holds no
findings of its own.

| Slice | Issue | Findings |
|---|---|---|
| Bot routing and per-Butler chat connections | [#153](https://github.com/chickenrat17/wildstone-homelab/issues/153) | [`findings/153-bot-routing.md`](findings/153-bot-routing.md) |
| Routine authoring, execution, approval integrity | [#154](https://github.com/chickenrat17/wildstone-homelab/issues/154) | [`findings/154-routines.md`](findings/154-routines.md) |
| Identity and authorization | [#155](https://github.com/chickenrat17/wildstone-homelab/issues/155) | [`findings/155-identity.md`](findings/155-identity.md) |
| Shared memory, audiences, semantic recall | [#156](https://github.com/chickenrat17/wildstone-homelab/issues/156) | [`findings/156-memory.md`](findings/156-memory.md) |
| Secrets and privacy gateway | [#157](https://github.com/chickenrat17/wildstone-homelab/issues/157) | [`findings/157-secrets.md`](findings/157-secrets.md) |
| Durable execution, delegation, lifecycle | [#158](https://github.com/chickenrat17/wildstone-homelab/issues/158) | [`findings/158-execution.md`](findings/158-execution.md) |

Each file carries its own status in its header, so progress is read
from the files rather than from a table six sessions would all want to
edit at once.

---

## Finding identifiers

`F-<issue>-<nn>` — `F-153-01`, `F-156-07`. The issue number namespaces
them, so two slices cannot collide however they are scheduled, and one
session owning one file means no collision inside it either. Numbering
is a property of the layout rather than a convention anyone has to
remember.

**Never renumber.** A remediation issue may already cite an id, and a
citation that silently comes to mean a different finding is worse than
a gap in the sequence. Numbers increase; gaps are fine; withdrawn
findings are struck through in place with a line saying why.

## What the lint checks

`testing/e2e/auditregister_test.go`, which CI runs:

- every findings file declares an issue number matching its filename,
  and a status from the closed set
- finding ids are well formed, match their file's issue, are unique,
  and only ever increase
- every finding carries a status from
  *implemented / partial / missing / unverified* and a non-empty
  evidence line
- this index links every findings file, and every findings file is
  linked from this index

It deliberately does not check prose, table contents, or whether a
finding is any good. It checks the things that break silently when six
sessions write in parallel.

Run it before committing:

```bash
cd testing/e2e && go test -run TestAuditRegister ./...
```

---

## Summary

*Written once every slice is in, as the input to remediation planning:
counts by state, the findings that block others, and the decisions
needed before any of them can be scheduled.*
