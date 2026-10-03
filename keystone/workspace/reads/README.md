# Scoped workspace read tools (S07)

`workspace_list`, `workspace_read` and `workspace_search` are individually
registered, opt-in, nonmutating tools on the canonical Owner and Member
gateways. Both manifests and dispatch apply current tool/resource grants.
Readiness reloads workspace settings; group conversations and Shared scope
remain refused. Registration does not seed tools or create workspaces.

The read coordinator requires authenticated operation identity and the full
task privacy turn. Metadata, filenames, directories and snippets pass the
same live source-policy checks as bodies. Only committed, nontrashed artifacts
appear; staging is excluded. Reads persist source identity before disclosure
and recheck authority after byte I/O. That check atomically requires the artifact
to remain committed at the admitted path/current revision; concurrent trash,
move or publication refuses stale output. Retained historical revisions remain
readable while their artifact stays committed. Bytes use the existing immutable confined
store and hash/size verification. `ReadCommittedBytes` exposes that transport
without model envelopes for independently authorized human routes; it grants
no access and requires trusted committed metadata from the caller.

Text pages target 2,500 UTF-8 bytes and shrink for JSON escaping/metadata.
The complete structured result plus the existing untrusted envelope fits the
default 4,000-byte bound. Long metadata that cannot fit is refused visibly;
the gateway never truncates a successful structured workspace response. If a
nonterminal page cannot fit even one complete UTF-8 rune, it refuses visibly
instead of returning an empty page/cursor at an unchanged offset.
Search is literal, case sensitive filename/body matching with 160-byte UTF-8
snippets and at most 20 scanned files per page. Scans can return an empty page
with a continuation. Storage/download limits remain separate.

Opaque cursor handles retain task, workspace, operation, exact request,
configuration revision, immutable source revision or namespace snapshot and
position. They expire after 15 minutes, fail after restart and recheck current
authority on every page. Changed/tampered/cross-task selectors refuse without
loading bytes. The bounded process store retains at most 1,024 cursors; capacity
failure is explicit. Workspace ledger reuse already re-enters the resource seam.

Until S09 (#482) supplies empty-directory metadata, list derives only nonempty
directories from committed child paths. `read-` directory tokens are keyed with
a process-random secret and bound to committed child identities/revisions.
Child changes or restart invalidate them. These are read snapshot tokens,
never mkdir/rmdir authority; S09 must issue its own durable generation tokens.

Focused checks: `go test ./workspace/reads ./butler ./agentgateway ./runtimedb`
with the read/workspace test filters and `go vet` for those packages. Linux
tests additionally exercise real confined byte publication/read/integrity.
Run race and full repository CI on Linux; Windows cannot exercise openat2.

Server-issued artifact, revision, directory and cursor selectors use JSON Unicode
escapes so decoding preserves their exact identities through central redaction.
Paths, snippets and file text remain ordinary JSON strings and still redact.
The escaped wire length counts toward the complete result bound.

Directory scope filters the database namespace before source policy checks and
provenance tracking. Snapshot cursors cover that selected namespace; unrelated
folder mutations and independent protected sources do not affect its pages.
