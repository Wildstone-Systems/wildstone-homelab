# Slice 22: Owner to Member demotion (#223)

Base: `main`, after the reviewed routine executor. This slice is a role change, not offboarding.

## Existing seams

- `SetButlerMemberRole` serializes changes and enforces the Last-Owner invariant in both code and schema.
- Console authentication, Agent admission, and Household secret management recheck the live Person role.
- The Owner review destination already clears on role loss. Unused legacy review tokens do not.
- Household routines approved by a Person are owned by the Household after approval.

## Implementation

1. Write preservation tests first: the Person remains active; Homepage, service entitlements, Member-Agent admission, Personal memory and secrets, and existing Agents remain. Demotion writes no offboarding or deletion record.
2. Add a forward-only migration that consumes unused Owner-bound review tokens when an Owner becomes a Member. Do not change the Person generation or invalidate Member resources.
3. Expose a demotion action for a freshly authenticated human Owner in Console. Reuse the consequential-operation step-up gate, then end the target's elevated Console sessions after the role transaction commits.
4. Show the action on active Owner rows in Console with a brief explanation of what is lost and kept.

## Authorization and tests

The live role narrows general execution authority. The token trigger withdraws a resource-specific pending approval. Existing audience and disclosure policy remain unchanged; neither Owner approval history nor Household routine grants are revoked. Tests cover each revoked and preserved capability, last-Owner refusal at the API call site, stale sessions and bearer denial, self-demotion, and a previously approved routine that remains authorized.
