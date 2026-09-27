# Matrix federation notes for Wildstone Social

Issue #401 asks that the Matrix package be an architectural research point,
not that Wildstone Social be rebuilt on Matrix. The private homeserver package
therefore consumes none of these ideas yet.

## Useful protocol boundaries

Matrix separates a stable user namespace (`@localpart:server-name`) from the
client endpoint discovered through `/.well-known/matrix/client`. Federation
uses a separate server-to-server API and discovery path. That separation is
the useful lesson for Wildstone Social: a person's public identifier need not
be the host currently serving their client traffic.

Matrix also provides examples of:

- domain-controlled namespaces and delegation;
- signed server-to-server events and independently verifiable event graphs;
- explicit room membership and power levels rather than ambient broker trust;
- device identities and cross-signing distinct from the account name;
- incremental synchronization tokens for intermittently connected clients.

These are protocol concepts to compare before inventing a new broker or
federation layer. Reusing them does not require adopting Matrix rooms as the
Wildstone Social data model.

## Questions before a federated profile

1. Is the public identity hosted (`@name:wildstone.social`), delegated from a
   user-owned domain (`@name:example.net`), or both?
2. Who owns recovery and namespace continuity when the serving homeserver
   changes?
3. Which metadata remains visible despite end-to-end encryption?
4. What registration, moderation, rate-limit, media-retention, and abuse-report
   obligations appear once unknown servers can connect?
5. Can Wildstone's authorization evidence be carried across servers without
   turning a remote Matrix identity into household authority?
6. Which keys and database records must survive restore for identifiers and
   encrypted history to remain usable?

## Decision boundary

The `matrix-synapse` package ships with federation disabled. Enabling it is not
a boolean added to the private profile: it is a new exposure and identity
posture that should be designed and tested as a separate package profile.
