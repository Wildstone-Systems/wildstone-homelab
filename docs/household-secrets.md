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
