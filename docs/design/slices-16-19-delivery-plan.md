# Slices 16–19 delivery plan

Authorized sequence: #217, #218, #219, #220, each branching from and targeting main in a separate PR. Merge after green required CI, resolved review comments, and a fresh Codex review with no P1 findings. Fix actionable findings rather than treating lower priority as permission to ignore them.

## Household defaults

Preserve existing content classifications and disclosure restrictions; no bulk sensitivity inference or reclassification. People control Personal content restrictions; Owners control Household policy. Use plain household language for controls. Existing Personal-secret ownership and trusted reveal rules remain unchanged.

## Slice 16: processing and delivery privacy

Add explicit local-only processing and trusted-surface delivery restrictions to memory and conversation context, with forward-only schema changes. Keep destination disclosure separate from model locality. Preserve and intersect source restrictions across derivations, corrections, promotions, remembered conversation content and resumed history. Unknown destinations remain denied by existing policy.

Enforce before provider calls, including recall results and retries, and recheck before delivery. A privacy failure is visible, not an empty-memory answer; no cloud fallback can widen policy. Ordinary unrestricted family chat retains its existing behavior. Expose small controls through the existing memory/session surfaces and shared public API.

Tests start with unavailable local routing and zero cloud calls, local inference with prohibited external delivery, per-destination outcomes, inherited restrictions, resumed sessions, privacy changes during a turn, and external secret warning without echo. Mutation checks target live callers as well as evaluators.

## Slice 17: Plans and Owner Review Queue

Every submitted Plan enters a persistent queue visible to all Owners. Agents propose and never activate. Fresh Owner approval binds the exact version, including output audience and delivery behavior. Edits create new versions. Revocation invalidates pending approvals without withdrawing already-approved Household routine authority.

Use a configurable designated Owner and notification destination. Until configured, the queue remains visible in Console without external notifications. Notices carry only a generic message and review link; approval occurs in Console. Keep pending Plans indefinitely, as the design requires.

## Slice 18: household execution fencing

Inventory every effectful domestic, notification and chat dispatch. Enforce current coordinator authority and household stop at the shared store/runtime boundary, including legacy callers. Preserve permitted reads on non-coordinating processes. Reproduce lease loss and stop races. Include coordinator heartbeat remediation if still missing, or explicitly track any remaining work.

## Slice 19: objective-aware completion evidence

Preserve informational completion on delivery evidence. Require successful tool, scheduler or executor receipts for effectful objectives, with unknown objectives treated as effectful. Enforce in both store and schema, retaining atomic evidence/state writes, permission labels and audiences. Document classification at trusted task creation and how assertions of effects are prevented from substituting for receipts.

## Validation and review

For each slice, record acceptance tests and at least one failing negative test before implementation. Run required Go vet/race, Console build and applicable UI checks. Demonstrate mutation failures at enforcement call sites. Open the PR with scope, behavior, validation and limitations; request Codex review, fix findings and repeat. Rebase or merge current main and rerun affected checks when necessary before merging. Do not deploy or modify live household configuration as part of this sequence.

### Privacy upgrades

Sessions and interrupted tasks created before privacy provenance tracking receive
conservative source links during migration: existing Household records and
Personal/Agent-local records their Person/Agent could have recalled by then.
This preserves default behavior until someone tightens a relevant record's
policy. Because old recall provenance was not recorded, some old histories and
their derivatives may inherit a restriction even when the exact read is unknown.
New work records exact reads. The upgrade regression verifies both protected
historical replay and isolation from another Person's unrelated private record.

Pre-0070 pending proactive messages have no reliable generating-context
provenance. The upgrade expires that pending push queue and retains its rows
for audit; otherwise a later privacy restriction could be bypassed by an old
message. Fresh notifications are unchanged. Pending historical replies retain
conservative Person/session provenance, and new replies, session choices, and
task-authored pushes carry explicit sources through the adapter poll.

### Final disclosure boundaries

Memory retrieval/export/digest and session/task inspection apply the same live
trusted-screen restriction. Human Console sessions remain trusted; service and
operator bearer credentials cannot claim that destination. Existing unflagged
inspection remains available. Stored task evaluation and debug views retain the
source-task check.

Notification and calendar HTTP attempts recheck disclosure immediately before
transport, including redirects. File mutations recheck after preparation at the
store's write callback. A privacy change during preparation therefore withholds
the effect rather than relying only on the gateway's earlier check.
