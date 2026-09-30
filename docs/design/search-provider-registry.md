# Search-provider registry (S17)

Implements the configuration boundary of #490 under ADR-W047 and
[native-agent-capability-contract-v1](native-agent-capability-contract-v1.md).
Migration 0098 creates an empty typed registry and an empty selection store.
It neither starts services nor registers, assigns, or grants research tools.

## Owner API

All routes use the public API's authentication, RBAC and redacted audit/response
path. Registry reads and research details on generic Plan reads require an active Owner. Writes and configuration Plan
approval/rejection require a fresh Owner Console session and the existing same-origin
write check. Runtime, Member and machine-only bearer credentials cannot configure
or test providers. No request accepts secret values.

- `GET /api/v1/research/providers`: `providers` and `defaultSelection`.
- `GET /api/v1/research/providers/{provider}`: current provider/config revision.
- `POST /api/v1/research/providers`: `{config: SearchProviderConfig}` creates a
  proposed Plan and server-issued provider ID; nothing is activated yet.
- `PATCH /api/v1/research/providers/{provider}`: `{config, expectedConfigRevision}`
  proposes complete replacement configuration, not a partial field merge.
- `DELETE /api/v1/research/providers/{provider}`: `{expectedConfigRevision}`
  proposes registry removal, without removing a managed service.
- Approve through `POST /api/v1/plans/{id}/approve`. The Plan view includes its
  exact `research` change. Approval compares the provider revision and atomically
  applies the registry change and completes the Plan. Registry Plans cannot mix
  with infrastructure/routine/settings changes, nor authorize a reconciler tick.
- `POST /api/v1/research/providers/{provider}/test`: `{expectedConfigRevision}`.
  Tests stored configuration only; clients cannot supply readiness verdicts.
- `PUT /api/v1/research/default-provider` and
  `PUT /api/v1/agents/{agent}/research-provider`:
  `{providerId: string | null, expectedConfigRevision: string}`. The expected
  revision is the **selection's** revision, empty only before its first write.
  `GET /api/v1/agents/{agent}/research-provider` reads that selection. The Agent
  path uses the existing Agent name lookup and persists its stable database ID.

`config` contains `name`, `kind`, optional `endpointRef` and `credentialRef`,
`backends`, `fallbackOrder`, and `limits` as defined by the v1 contract. IDs and
revisions are opaque server-issued values. Responses contain references only.
Errors from provider testers are never returned, persisted or audited verbatim.

Only `ddgs` is admitted in v1. The pinned worker currently supports only
`duckduckgo`, so omitted backends default to `["duckduckgo"]`; fallback is empty.
`auto`, unsupported engines, nonempty fallback, and reserved SearXNG/Tavily kinds
are refused. Future worker support requires a reviewed allowlist extension.

Endpoint references use `service/<name>`, credential references `secret/<name>`;
names use 1–63 lowercase letters, digits, dots, underscores or hyphens, starting
with a letter/digit. URLs, userinfo, paths, query parameters and secret literals
are not endpoint configuration. A syntactically valid service reference is not
proof of a deployed service. It cannot become ready without a tester resolving
and validating it. Missing endpoint references produce `unconfigured`.

Omitted/zero limits use the contract defaults: 10 searches, 20 fetches,
30 seconds/request, 5 MiB download, 50,000 extracted characters, 900 seconds cache.
Configurable bounds are 1–100 searches, 1–200 fetches, 1–30 seconds, 1–5 MiB,
1–50,000 characters, and 0–900 cache seconds. These values are persisted policy,
not task admission/accounting; S24 and the adapters enforce actual usage.

## Readiness and handoff

Every configuration edit produces a fresh revision and clears readiness, even
for a name-only edit. A test first invalidates prior readiness and obtains a
single-use probe generation. Completion must match both config revision and
probe generation. Later edits, deletion, tests, or dependency invalidation fence
old success. Failed tests become `unavailable`; abandoned tests stay `untested`.

`Server.ResearchTest` is an injectable adapter seam. It receives only stored
configuration and a context bounded by its configured request timeout. It must
honor cancellation, resolve service/credential references privately, and verify
the current resolved dependency revisions. Nil remains unavailable. S19/S20 own
real adapter/service wiring and desired-state provisioning Plans. Rotation,
service removal, changed resolved endpoints and failed health checks must invoke
`DB.InvalidateSearchProvider(id, configRevision)` before further use; this also
invalidates in-flight probes. These dependencies are not watched by S17.

`DB.ResolveSearchProvider(agentID)` selects the explicit override or inherits
the household default when the override is null/absent. A missing explicit
provider never falls back to another provider. Deletion deliberately retains
selectors to enforce this. Missing, untested, unconfigured and unhealthy
providers yield `provider_unavailable` reasons. Selection is never authority:
callers still enforce current tool assignments, Person/surface restrictions and
egress privacy. No live provider or native-runtime evidence is claimed here.
