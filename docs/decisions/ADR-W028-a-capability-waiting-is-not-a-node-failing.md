# ADR-W028: A capability waiting is not a node failing

- **Status:** accepted
- **Relates to:** ADR-W008 (reconcile loop), F37 (a missing secret must
  be named before approval, not met one per tick), F60 (a repeatedly
  failing resource is set aside so the rest converges), ADR-W016 A2 /
  ground rule 4 (every change is a plan a human approves)
- **Audience:** decision record.

---

# Part A — Decisions (binding)

### A1. A missing operator-supplied secret blocks its capability, not the node

`ensureNodeSecrets` gathered every `provided: true` Secret in a node's
desired state, and if any had no value it aborted the whole reconcile
pass. One capability enabled without its secrets therefore stopped every
other capability on that machine from converging.

The consequence was not theoretical and not proportionate. On a live
deployment a household chat adapter was enabled without its bot token.
Forty-five minutes later an unrelated `ai-open-webui` install was
approved; it failed, reporting two secrets it had never heard of. So did
a household-member plan. So did twenty-odd scheduled backup runs. The
operator reasonably concluded the packages were broken.

The scope of the problem is the capability that declared the secret. The
scope of the consequence now matches it: steps belonging to a capability
awaiting a supplied secret are removed from the plan, the rest of the
node converges, and the capability is reported as blocked with the
commands that unblock it.

This is not a new mechanism. F60 already established that a resource
which cannot converge is set aside "so the rest of the plan proceeds
without them, and the operator is told which resource is stuck and why
rather than watching everything else fail behind it." That principle
was right; it simply had not been applied to this cause.

### A2. A delete is never blocked

The way out of a capability that cannot converge is to remove it, and a
removal is a delete. Blocking deletes would make the state inescapable
from the Console — the exact corner this ADR exists to open up.

A delete also cannot need the secret it is deleting. A step whose
resource is absent from desired state is a delete by definition, is
attributable to no owner, and is left alone.

### A3. Not being able to ASK is still a node-wide fault

If the agent cannot be reached to list the secrets it holds, the pass
fails as it always did. Converging on a guess about which secrets exist
is worse than not converging: the guess that a secret is present is the
one that generates a random value where a real credential was required,
which is the failure F37 exists to prevent.

### A4. Waiting and failing get different words

A resource set aside under F60 has been attempted and failed, repeatedly.
One set aside under A1 has never been attempted and should not be. The
summary for the second says so, and carries the reconciler's own
explanation — which already names the exact `wild secret set` commands —
rather than wrapping it in "has failed 3 times", which would be false.

---

# Part B — Considered and rejected

## Automatic rollback of a plan that will not converge

The question that prompted this ADR was whether a failed install should
revert itself so later installs are not blocked. It should not, for four
reasons, and the fourth is the one that settles it.

**It is a change nobody approved.** Ground rule 4 says every change to
desired state goes through a plan a human approves. An automatic
deletion is a change. Making the control plane the one principal that
can alter state unasked is a side door around the gate the whole
architecture funnels through.

**It is often wrong.** A node offline for twenty minutes fails to
converge too. Auto-revert would uninstall working capabilities because a
machine rebooted.

**It destroys data.** Removing a capability removes its volumes. An
automatic rollback would delete what was stored, with no plan, no
approval, and no way back.

**It treats the symptom.** The pain was never that the bad install
persisted; it was that the bad install stopped everything else. Once the
consequence is scoped to the capability, a half-finished install is one
blocked tile on Home that says what it wants — which is a state an
operator can read, act on, and leave alone if they are not ready. There
is nothing left for a rollback to save them from.

## Refusing to propose an install whose secrets are unset

Tempting, and wrong. The gap between proposing and approving is exactly
when an operator would go and set the secret; the preview already names
what to set, and a plan waiting for approval is a reasonable place to
pause. Refusing at propose time would break a legitimate flow to prevent
a state that A1 has already made harmless.
