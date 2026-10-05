# Native staged workspace tools (S08)

`workspace_write_begin`, `workspace_write_chunk`, `workspace_write_commit`
and `workspace_patch` are individually opt-in, mutating tools on canonical
Owner and Member gateways. Their schemas advertise only private workspaces.
QA-only sessions withhold and refuse every write. Neither tool registration
nor gateway wiring creates workspaces or seeds upgrade grants.

Trusted operation context carries the originating task, canonical tool and
Invoke idempotency key. JSON arguments carry only narrowing selectors and
bounded text. Keys are required, at most 128 UTF-8 bytes. Source authority,
current explicit Person/Agent grant, tool assignment, original class/surface,
current model/session QA state and task lifecycle are checked on every
coordinator boundary. Cancellation stops effects; retries restore persistent
sessions only under current authority. Task-owned sessions cannot cross tasks.

The existing publication coordinator owns quota-accounted scratch, consecutive
1,024-byte UTF-8 chunks, exclusive immutable byte publication, expected-base
comparison, execution fences, recovery and original durable receipts. This
adapter neither publishes directly nor deletes staging. Begin retries return
current consecutive durable `nextIndex`; incomplete staging remains invisible.
Create collisions and stale edits leave committed history unchanged.

Patch replaces exactly one occurrence, including overlap checks. Its old/new
text combined is at most 1,024 bytes. Full edits use staged replacement writes.
Additional patch arguments are bound by a trusted content hash in Begin's
request fingerprint; no plaintext patch body is stored in session metadata.
An unchanged retry reuses the original session/receipt, even after the current
revision advanced. Changed arguments conflict. Patch bytes use confined,
hash/size-verified committed storage and the coordinator's chunk/commit path.

Current task/base source flags are frozen again at sealing, capturing reads
made after begin. Existing immutable capture triggers retain exact sources.
A changed processing destination/source permission refuses before publication.
The complete versioned JSON result stays within 3,600 bytes before the existing
untrusted envelope; paths whose receipt cannot fit are refused before begin.
Only verified server-issued selectors use JSON Unicode escapes to preserve
opaque identity through central secret redaction. Paths/text still redact.

S07 and S08 share one typed failure mapping. Readiness enumerates both read
and write tool names, each using its own configured seam and current settings.
Generic transport limits remain unchanged.

Windows evidence covers real database authority, session keys, cancellation,
policy inheritance and canonical dispatch. Linux tests cover confined report
publication, restart/cancellation retries, patch replay, unique match rules,
stale edits and execution-fence revocation. Cross-compilation verifies their
build; execute them and race/full CI on Linux before treating this as complete
platform evidence. No migration, move/trash, sharing or lifecycle cleanup is
added by this slice.
