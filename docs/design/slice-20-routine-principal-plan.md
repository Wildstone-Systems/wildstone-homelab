# Slice 20: exact routine-version authority (#221)

The approved Plan snapshot is the routine principal. Its Plan ID and revision
identify one immutable authorization; the proposer, approving Owner and
assigned Agent are not substitutes for it. Slice 21 will schedule execution
using this principal, while this slice supplies the checks the executor must
call.

1. Extend routine submissions with explicit Household secret IDs and memory
   record IDs. Snapshot those requests alongside document, assigned Agent and
   output audience. A changed request set increments the routine revision and
   requires a new Owner review. Approval grants only the reviewed version.
2. Resolve a routine principal from current reviewed, enabled state on every
   use. Intersect that state with assigned Agent capability, the resource's
   exact-version grant, current resource state and audience/privacy policy.
   A prior version's grant cannot authorize a new version.
3. Route Household secret use through the existing general principal/object
   grant check, extended to recognize the exact routine principal. Keep raw
   reveal and all unattended Personal secret use denied.
4. Filter unattended memory candidates before loading content. Permit only
   explicit Household or Agent-local grants whose record and inherited
   restrictions include every current member of the approved output audience.
   Personal memory remains unavailable until its owner can grant it.
5. Add negative tests for proposer, approver, Agent and old-version grants;
   reveal and Personal secrets; adults-only content to a Kids audience;
   Personal and inherited Agent-local memory; and grant edits requiring review.

This changes all three §10 layers: the routine principal supplies general
execution authority, exact resource grants supply the resource layer, and
the approved output audience plus current privacy policy narrow disclosure.
No later layer can turn an earlier denial into permission.
