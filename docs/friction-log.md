# Friction log

Every surprise hit while dogfooding the migration
([migration.md](migration.md)) gets a row **when it happens**, not
from memory later. A row is closed by a fix (link the commit/PR) or an
explicit deferral with a reason — silent deferral is not a state.
Alpha exit requires this table empty or all-deferred-with-reasons
(ADR-W018 A4).

Severity: **blocker** (migration step cannot proceed) · **papercut**
(worked around, but wrong) · **polish** (confusing, cosmetic).

| # | Date | Phase/step | Severity | What happened | Status |
|---|------|-----------|----------|---------------|--------|
| — | — | — | — | *(no entries yet — the log opens with Phase 0)* | — |

## Template

```
| n | YYYY-MM-DD | e.g. Phase 1 / photos step 4 | blocker | What you expected, what actually happened, exact error text | open / fixed <commit> / deferred: <reason> |
```
