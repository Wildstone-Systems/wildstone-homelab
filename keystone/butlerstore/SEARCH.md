# Scoped search reuse (S25)

`Searches` implements #498 as dormant platform plumbing under ADR-W047 and
the v1 capability contract. It registers no native tools or public routes.
S26 must supply canonical bounded Invoke admission, current trusted callbacks,
one-attempt provider dispatch, response pagination/redaction and checkpointing.

## Required integration

- `Research.Authorize` must recheck current Person/Agent/task/surface/tool grants,
  provider policy and limits on every attempt and retained read, as in S24.
- `Resolve` returns the current trusted Person, disclosure/resource scope,
  provider connection identity, selected backend and configuration revision.
  Never copy these fields from tool arguments. Resource scope must include any
  Agent/surface/disclosure distinctions required by current policy.
- `AuthorizeSource(requestingTask, originTask, originStep)` must reauthorize the
  origin step's complete source graph and inherit its provenance into the
  requesting task. This callback is mandatory even for same-task hits. A missing
  origin graph or failed inheritance refuses; cache ownership grants no access.
- Production always calls `runtimedb.CheckResearchEgress` before dispatch and
  returned bytes, including cache hits. The private test seam cannot be wired by
  a native caller. Current provider selection is repeated around I/O and hits.
- Persist `SearchRecord.Encode()` in the canonical `ToolStepResult.Content`.
  Only a successful, non-rejected read attempt with a verified `web_search`
  step permits reuse. Encoding is a private ledger format, not the final model
  response. S26 must preserve it separately from any paginated/redacted response
  representation and must not expose this record as model-authored authority.
- Dispatch callbacks perform exactly one upstream attempt and return only the
  adapter's bounded failure classifications. Every retry/fallback invokes this
  seam again with the current selected provider/backend. Future adapters must
  return their actual retrieval dates/evidence metadata.
- Validate advertised filter/backend capabilities and acquire any bounded
  provider queue slot before invoking this seam. Its key boundary validates
  common request bounds; unsupported provider-specific options must refuse
  before S24 reservation rather than reach a dispatch callback.

## Behavior

Keys bind Person, resource scope, provider connection, backend, config revision,
exact query, exact filters and normalized limit. Keys contain a SHA-256 digest;
no query is copied into cache metadata, tracing, errors, wait text or logs. Full
results remain in the existing private task ledger; neither digests nor cache
ownership are authorization. The seam emits no logs.

TTL is exactly 15 minutes from the record's initial publication preparation;
lookup does not renew it or change retrieval/source dates. At most 128 process
entries reference verified origin steps. Every hit rereads that authoritative
receipt, rechecks current policy/source grants, inherits provenance and checks
S23. The cache spends no upstream unit and never resets usage; normal Invoke
tool-call limits still apply.

Fresh responses never seed process cache before canonical verification. A crash,
failed/cancelled request, or unverified success is not reusable permission. Every
new dispatch reserves an S24 unit. Concurrent in-flight calls remain independent;
they do not borrow reservations or cancellation contexts. On restart the process
cache is empty and only verified matching same-task ledger results can be reused.
Cross-task cache warming resumes after authorized same-task lookup. Results whose
policy changes during I/O are refused; their already committed spend remains.

## Evidence limits

Deterministic regressions cover partitioning, exact TTL, unchanged evidence dates,
denial before dispatch, current grant/source/egress revocation, Person spoofing,
same-Person cross-task hits, unverified successes, cancellation, independent
concurrent requests, actual database reopen and configuration/post-I/O policy
changes. S26 callback integration and native transport/gateway envelope behavior
are separate required evidence. No live provider, deployment or release claim is
made. No migration or person-reference inventory change is required.
