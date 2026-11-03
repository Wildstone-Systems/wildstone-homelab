# Restore quarantine release contract

Status: implemented repository contract for #285. Journal reconciliation is
#284; coordinator promotion and post-promotion restart remain #286 and #287.
Release is a data-access decision, not execution authority.

## Required release state

`POST /api/v1/restore/release` accepts only `{"release":true}` from a fresh
Console session for one currently admitted, active Owner. Administrator bearer
credentials, Operators, Members, Agents, stale sessions, disabled Owners and
revoked credentials are not release authority. The request is recorded in the
ordinary request audit, while `restore_state.released_at` and
`released_by` retain the successful decision.

The runtime database revalidates all of the following in one write transaction
before clearing quarantine:

1. The current restore has a nonempty random restore generation and exact
   snapshot SHA-256/timestamp.
2. `restore_state` names a completed #284 verification and replay.
3. The immutable `recovery_journal_reconciliations` row exists and binds the
   same restore generation, snapshot, source, terminal head, coverage,
   verifier, verification time and replay count.
4. The retained canonical evidence still has a valid Ed25519 signature under
   the currently installed `recovery-journal.pub`; its recorded fingerprint
   also matches that key. Changing or losing the custodian trust root fails
   closed.
5. The envelope is final, names independent custody, contains a contiguous
   hash chain and snapshot prefix, and covers through a nonempty source fence.
   The original writer must have been independently stopped/fenced before this
   terminal evidence was generated. An Owner checkbox or timeout is not that
   evidence.
6. No local journal suffix exists beyond the signed head except rows carrying
   this evidence ID's replay provenance.
7. The release update still matches the same generation and evidence ID when
   committed. Missing, stale, mismatched, copied or incomplete state preserves
   quarantine.

The schema independently prevents clearing `quarantined` or setting a release
timestamp unless the current projection has a matching generation-bound
immutable audit and replay checkpoint. A caller-provided `journalCurrent`
value is never accepted. Owner authorization cannot substitute for signed
journal verification.

Repeating release is idempotent only if the same current restore generation,
snapshot, evidence, trust root, replay and original durable release still
validate. A different or unaudited unquarantined state is refused. The decision
survives process restart.

## Execution remains fenced

Entering restore quarantine revokes the process-local coordinator handle.
`AcquireCoordinator` refuses every database with a restore generation, before
and after quarantine release. Therefore release does not change the preserved
coordinator holder or epoch, wire a scheduler, resume a mailbox reconciler, or
make queued Agent/tool/automation work dispatchable.

The post-restore promotion marker and authenticated promotion transition are
owned by #286. Until that separately reviewed transition and the documented
restart, `CheckHouseholdExecution` fails closed. #287 then validates singleton
restart, stale-epoch refusal, completed receipt continuity and uncertain-effect
handling. #285 neither promotes nor executes work.

## API sequence

```text
POST /api/v1/restore/reconcile  {"evidence": <signed terminal evidence>}
                                -> still quarantined
POST /api/v1/restore/release    {"release": true}
                                -> data quarantine released; execution fenced
POST /api/v1/coordinator/promote
                                -> separate #286 operation, not implemented here
restart singleton              -> separate #287 validation
```

No repository test in #285 authorizes a live Kubernetes change or release of
the retained #277 installation.
