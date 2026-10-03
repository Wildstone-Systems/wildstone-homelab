# Research provider adapter (S19)

Implements #492 under ADR-W047 and the native-agent-capability-contract-v1.
This is a dormant Keystone adapter, with no tool activation, migrations, service
provisioning, live searches, task counters, cache or public-page retrieval.

`Factory.NewSearch` takes the selected `research.Provider` from the S17 registry
and a trusted `EndpointResolver`. It checks readiness against the configuration
revision, validates configuration, and resolves only the stored service reference.
The resolver must honor its context and validate the managed service identity
and dependency revision. S20 supplies that integration; no default resolver or
ambient endpoint exists. Gateway callers must re-resolve current selection and
check current grants/provenance and reserve an attempt before calling Search.
Do not retain a constructed adapter across configuration/authority changes.

`Factory.Test` matches `api.Server.ResearchTest`: an explicitly requested Owner
probe uses a fixed synthetic query against stored configuration. It does not
require prior readiness or mutate it; S17's probe/config CAS does that. The total
probe deadline includes resolution and search. Health alone does not establish
upstream readiness. This slice does not install the callback in the server:
managed dependency validation and invalidation belong to S20.

The pinned registry and worker support only DuckDuckGo. Unsupported kinds,
backends and any nonempty fallback order are refused before resolution/I/O.
There are no automatic retries, other engines or paid routes. Each dispatched
search returns one `Attempt`, including failures/cancellation, with actual
provider/backend and typed code; preflight refusal returns no attempts. Empty
search is successful. Future fallback support requires a reviewed worker and
registry allowlist extension plus per-attempt admission; this slice cannot
silently broaden policy. Test-only SearXNG/Tavily shapes exercise `SearchAdapter`
without enabling those providers.

Supported filters are exactly region `wt-wt`, `us-en`, `uk-en`. Language and
recency constraints are explicitly unsupported, never approximated. Query and
filter strings are at most 1,024 UTF-8 bytes; limit defaults to five and is 1–5.
The internal request/response limits match S18: 8 KiB / 64 KiB, URL 2,048 bytes,
title 512 bytes and snippet 4,096 bytes. Malformed, unknown, duplicate, null,
oversized, wrong-version/provider/backend or status-inconsistent responses
fail closed without returning partial hits. Raw upstream errors are discarded.
Worker retrieval and source dates are preserved as UTC times; missing source
dates remain absent. Search never visits result URLs.

HTTP uses a separate configured-service transport with no environment proxy,
cookie jar, credentials, redirect, compression or retry. Configured credential
references are explicitly unsupported by the unauthenticated S18 wire. Only a
bare HTTP(S) service origin is accepted. One configured deadline (at most 30s)
bounds each search including headers/body; caller cancellation closes the worker
request. Use the publicfetch package for arbitrary public URLs, never this
internal-service transport.

`SearchAdapter` and `ExtractionAdapter` are independent interfaces. Results are
untrusted normalized evidence, not Agent tool responses or persistent source
references. The later source/evidence layer must retain metadata, assign opaque
IDs/revisions and page complete JSON into the existing 4,000-byte gateway bound.
No extraction implementation, source persistence, grants or budgets are added.

Verification: `go test -race ./research/...` and `go vet ./...` in Keystone.
Fixtures use local HTTP servers only and are not live-provider evidence.

## External egress privacy (S23)

Model-authored search queries and fetch URLs must enter through
`egress.Search` / `egress.Fetch`. These call `runtimedb.CheckResearchEgress`
before invoking any adapter. The gateway supplies a trusted `PrivacyTurn`
bound to its current task (the existing `beginDisclosureTurn` seam); query,
URL, model location and the local DDGS service address supply no authority.
Missing context, failed policy reads, protected source history and revoked
source access refuse both operations with no adapter request. Current task,
session and workspace ancestry are checked on each call, including resumes.
An active Person and explicit Agent access are required even on Console Owner
tasks; the legacy memory-only Console administration exemption cannot authorize
external research disclosure.
In-flight memory reads are committed to task provenance before dispatch and
retained on policy denial. All errors are static and omit input/source data.

This is a dormant boundary: canonical tool registration (#499), cache checks
(#498), budgets and execution fences must compose it with their own admission
checks. Low-level transport APIs are not Agent tool entry points. The fixed
synthetic Owner provider test remains a separate configuration probe.
