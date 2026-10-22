# Household secret storage operations

Household and Personal values use a separate Keystone store from Wardstone's infrastructure credentials. The runtime DB contains encrypted values and metadata. Keystone loads a dedicated 32-byte key from `<DataDir>/household-secrets.key`, created with owner-only file permissions on first use. Keep this directory out of managed application mounts and repositories.

Back up the key through the operator's protected backup process as well as the runtime DB and deletion journal. A database-only backup cannot recover values without the original key. Do not generate a replacement key for an existing store. Startup refuses missing, malformed, wrong keys or invalid ciphertext. Restore the matching key and investigate corruption; do not delete the database to bypass the failure.

Replicas sharing the same runtime database must receive the same key through protected operator provisioning before starting. Do not independently initialize keys on replicas. Automatic key distribution and encryption-master-key rotation are not implemented. Value rotation through Console is supported and creates a fresh authenticated ciphertext version.

Restore uses the existing quarantine/reconciliation procedure. Bring the deletion journal current before release. Secret revocations and rotations replay conservatively as quarantine; deleted values are erased. A quarantined object is not automatically reactivated. Store a replacement and explicitly grant access when needed.

Use Home → Personal and Household secrets for Personal intake, explicit Household submission and protected Household reveal. Use Console → Secrets for Household management, caller/Agent grants, submission review and Personal administrative lifecycle actions. Personal values have no reveal route, including for their owning Person. To share an existing Personal item, its owning Person can choose “Submit Household copy for Owner review”; this copies the value internally without revealing or modifying the Personal original. Administrative metadata shows another Person's object as “Personal item,” without its label or value.

For chat intake, the explicit formats are:

- `/secret personal Safe label = value`: store a Personal value owned by the authenticated sender.
- `/secret household Safe label = value`: submit a separate Household value; an Owner may inspect it for accuracy before approval.

Prefer the protected form. Sending either command through an external messenger has already disclosed the value to that provider. Wildstone warns and records this provenance. Labels must be safe descriptions, never the credential itself. Free-text detection is a guard, not a guarantee that every unlabelled value will be recognized.

The initial trusted consumer is calendar agenda reading. Configure the calendar endpoint and username through the existing deployment settings. Bind a compatible credential in the secret UI and grant the Agent `calendar.get_agenda` use. Household secrets additionally need an explicit Person or Group `use` grant. Binding does not install or provision a calendar. A Personal binding is considered only for that Person's active interactive call. The selected binding denies all other calendar operations and has no environment-credential fallback on failure. Do not bind the production calendar until this read-only boundary matches the intended use.

## Mailbox app passwords (Personal background credentials)

A Person's mailbox monitor (epic #326) uses a different store from the
household secrets above: the app password is entered once through
*Watch this mailbox* (Home or Console), sent over mTLS to each placement
node's write-only secret store as `pc.<connection>.v<n>`, and kept nowhere
in Keystone — no column can hold it, and it never appears in a response,
event, log or the audit chain. A per-connection fingerprint key
(`pc.<connection>.fk.v<n>`) travels the same way. The adapter receives
both only as secret references materialized into its container.

- **Not in any backup or recovery set.** Wardstone leaves every `pc.`
  name out of the node recovery bundle. After a whole-site restore each
  Person re-enters their own app password; nobody else can.
- **Revocation fences, then erases with evidence.** Revoking (by the
  Person, an Owner administering, offboarding, restore replay or removing
  the `mail-roundcube` package) moves the lease epoch, withdraws queued
  and claimed reads, revokes the authorization, removes the adapter
  container and erases every copy; each erasure is proven by the node's
  `EraseSecret` evidence. Anything unproven leaves the connection
  `degraded` with reason `erasure_unverified` until
  `POST /api/v1/integrations/{name}/credential/erase` proves it.
- **Prefer a separate, revocable app password.** Where the provider
  cannot issue a read-only credential, Wildstone still only reads, but the
  password itself has wider power upstream; revoke it at the provider as
  well when you stop monitoring.
- **Offboarding erases what the monitor learned.** Candidates, reviews,
  excerpts, effect records, dedupe state and checkpoints are deleted after
  the fence and journalled, so a restore deletes them again. Events
  already on a calendar stay there.
