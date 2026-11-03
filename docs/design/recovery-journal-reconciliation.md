# Recovery journal reconciliation contract

Status: implemented repository contract for #284; quarantine release remains
#285. This contract does not establish a backup schedule, off-host recovery,
coordinator promotion, exactly-once external effects, or HA.

## Authority and custody

The runtime database owns product authority. Its `deletion_journal` records
member offboarding, API-token and surface-link revocation, forgotten memory,
connection/routine/binding/background-grant removal, search-source removal,
and Personal or household-secret revocation/deletion. Principal disabled state,
capability/Plan state, immutable task attempts and effect receipts, uncertain
work, and coordinator holder/epoch live in their dedicated SQLite tables. The
replay code conservatively re-applies journalled removals and pauses uncertain
work; it does not claim external exactly-once execution.

Those records inside a snapshot prove only what was true at capture. Before
#284, the journal and `restore_state` were in the same SQLite file and
`POST /api/v1/restore/reconcile` trusted `journalCurrent: true`. There was no
independent post-snapshot authority, monotonic coverage proof, integrity
envelope, or exact snapshot binding. An Owner action could not repair that
evidence gap.

#284 keeps the existing journal and adds one bounded evidence envelope:
`wildstone-recovery-journal-v1`. A stopped/fenced source exports the full
ordered journal, signed by a dedicated Ed25519 journal-custodian key. Its
private half and the evidence file must be retained outside both the live
database and snapshot. Only `recovery-journal.pub` is installed on the restored
Keystone from the separately held recovery-key directory. The snapshot stores
neither its own successful verdict nor a substitute for this trust root or
external evidence file.

The distinct custodian signature prevents the restored snapshot—including its
preserved operation-signing private key—from manufacturing a successful
verdict about itself. Custody is still operational evidence: the custodian
must inspect the offline final source after fencing and retain the signed file
independently. A custodian key or evidence file available only on the lost
runtime claim is not independent.

## Verification contract

Reconciliation succeeds only when all of these checks pass:

1. `RestoreBackup` hashes the exact immutable SQLite artifact before copying it
   and stores that SHA-256 with its capture timestamp in `restore_state`.
2. Evidence names its stable journal source and independent custody, exact
   snapshot SHA-256/timestamp, base and terminal sequences, base/head chain
   hashes, terminal coverage time, source-fenced time, generation time, and
   independently pinned journal-custodian fingerprint.
3. The envelope is marked terminal, verifies under the preserved public key,
   has no unknown/trailing fields, and uses a contiguous sequence beginning at
   one. Every entry is included in the signed chain and falls within coverage.
4. The restored journal is an exact prefix of the signed journal. The declared
   base sequence/hash exactly matches the snapshot prefix. Missing, duplicate,
   conflicting, reordered, stale, future, mismatched, or tampered evidence is
   refused.
5. Post-snapshot entries are imported with their original sequence and replayed
   through the existing conservative deletion/revocation logic. If replay-time
   member fencing fires a deletion-journal trigger, the generated suffix is
   bound to this evidence ID in the same SQLite transaction as the authority
   withdrawal. After an interruption, retry accepts only that exact
   provenance-tagged suffix; an untagged or differently tagged extension is
   still source divergence and fails closed. Any replay error leaves
   quarantine active and no successful verification record.
6. A successful result is recorded in the append-only
   `recovery_journal_reconciliations` audit table and projected into
   `restore_state` with evidence ID, source, head, coverage, verifier and time.
   Both records bind a random generation created for this particular restore.
   Repeating the same evidence is idempotent only when the current generation,
   snapshot, trust root, completed replay, audit and verification projection
   all agree. An audit copied into a later backup cannot verify a new restore.
   Restart preserves a completed result and can resume an interrupted replay.
7. Reconciliation requires a fresh Console session for one currently admitted
   Owner. Administrator bearer credentials, Operators, Members and Agents are
   refused. The human authorization is necessary but is not journal freshness
   evidence.
8. Reconciliation never releases quarantine. The schema additionally refuses
   release of a restored snapshot unless signed evidence was verified and the
   journal replay completed. #285 owns the separate release decision and proof.

The source must be fenced before terminal evidence is generated. A timeout,
Owner checkbox, journal capture predating the fence, or a live source that can
still append decisions is not terminal coverage.

## Creating and presenting evidence

Provision the distinct custodian key before captures are relied on:

```sh
keystone recover journal-keygen \
  -private /independent-custody/recovery-journal.key \
  -public /offline/separately-held-bootstrap-keys/recovery-journal.pub
```

The command refuses existing targets. The private file never enters a runtime
claim, SQLite snapshot or management recovery set; only the public file is
installed from the separate bootstrap-key directory.

After the sole original writer is stopped and independently fenced, generate
the terminal file from the offline final source database and the exact snapshot
artifact:

```sh
keystone recover journal-evidence \
  -source-db /offline/final-source/keystone.db \
  -snapshot /offline/recovery-set/keystone.db \
  -snapshot-at 2026-11-03T10:00:00Z \
  -source-fenced-at 2026-11-03T11:00:00Z \
  -source household-keystone \
  -custody offline-recovery-journal-volume \
  -signing-key /independent-custody/recovery-journal.key \
  -out /independent-custody/recovery-journal.json
```

The command refuses an existing output, divergent snapshot prefix, sequence
gap, entry after the declared fence, invalid key, or inconsistent timestamps.
It does not contact Kubernetes, release quarantine, promote a coordinator, or
execute a household effect. Present the JSON object as `evidence` to
`POST /api/v1/restore/reconcile` from a fresh active Owner Console session.
The response remains `quarantined: true`.

## Fail-closed limitation and next ownership

Existing deployments did not continuously export this independently retained
terminal evidence. If the final source database is unavailable and no trusted
post-snapshot evidence file exists, Keystone cannot determine whether the
snapshot omitted a later deletion or revocation. It must remain quarantined;
the snapshot, its embedded journal, and an Owner approval cannot prove
freshness.

#163 must select how the established backup executor copies terminal evidence
to independent custody as part of a quiesced consistency group. It must not
place the only copy on the management claim. #285 may assume that a successful
#284 result is signed, snapshot-bound, continuous, terminal, replayed,
append-only audited, durable across restart, and still quarantined. #285 must
still authorize and test release separately; it cannot manufacture or waive
missing evidence.
