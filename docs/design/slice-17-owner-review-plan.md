# Slice 17: Owner Review Queue (#218)

The queue is populated by a database trigger for every submitted Plan. Routine
Plans snapshot the exact document, revision, output audience, and delivery
behavior. Approval checks that snapshot and an active Owner in one transaction,
activates authorization for only that version, and resolves the Plan without exposing an approved
infrastructure Plan to the reconciler. Editing invalidates pending versions.
Existing active routines are grandfathered only until their next edit.
Legacy Plans without an exact-version snapshot must be resubmitted in Console
before a review link can be issued; old links cannot approve that format.

Agents receive a narrow submission-only tool for existing routine drafts.
Proposer identity is bound outside model input. No approval or activation seam
is exposed. The Console and revision-token routes require a fresh Owner session.
Revoking a Person supersedes their pending Plans and invalidates tokens; approved
Household routine authorization remains intact.

Review notifications are optional. An Owner chooses an Owner's active connected
account and the household Console address. The worker reads the shared queue,
respects notice quiet hours and relevance, and queues only a generic notice and
link. Repeated passes deduplicate. Nothing is sent until configured. Notification
content carries neither Plan details nor an approval token.

## Household decisions

Preserve ADR-W033 A4.1's infrastructure CLI/host bearer approval exception.
Routine Plans require fresh Owner Console approval. Member-authored Meter and
Commitment edits remain immediate: an explicitly marked audit record moves from
applying to applied/failed, never proposed or approved, and enters no review queue.
This keeps household tracking simple without granting host execution authority.

## Validation

Negative cases cover Agent/Member approval, stale Console authentication,
creation of an active unreviewed routine, in-place mutation of reviewed payloads,
concurrent document changes, changed output audience, changed delivery behavior,
revoked proposers, and routine approval accidentally authorizing host repair.
Positive cases cover exact-version Owner activation, single-use Owner review
links, retained approved authorization after revocation, and generic deduplicated
notifications to the designated Owner. Legacy routine test fixtures now obtain
explicit Owner approval before testing their active behavior.

Keystone vet/full Linux race suite and Console build/render/UI/contrast/handoff
passed. Removing the real fresh-Owner API boundary and the store's Owner check
in isolated snapshots made their regression tests fail. Review regressions now
exercise full proposal loading and adapter-acknowledged notification delivery,
including retry after outbound expiry.

Remaining: integrate slice 16 after merge and complete CI/review rounds.

## Approval is distinct from execution

Issue #218 explicitly reserves routine execution for slices 20-21 (#221/#222),
and #219 forbids merging an executor before the effect fences. This slice
records the exact authorized version in the runtime DB. Its applied Plan is the
completed approval transaction; it does not claim an Automation was installed
in the Statefile or executed. Console states that automatic execution is not
yet available for these reviewed versions.

The legacy Automation engine excludes edited or review-managed Household
routines, including prior document names, and rechecks before dispatch. Thus an
old Statefile resource cannot keep running after its DB routine is edited or
reviewed. Unchanged grandfathered active routines and unrelated infrastructure
Automation resources preserve their existing behavior. The later versioned
executor must consume the approved document, Agent, audience, and delivery
snapshot directly; bypassing these contracts through the legacy engine would
violate the epic's hard gate.
