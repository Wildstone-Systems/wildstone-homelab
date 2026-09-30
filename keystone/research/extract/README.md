# Public documentation extraction (S22)

Dormant extraction and immutable text pagination for #495, [ADR-W047](../../../docs/decisions/ADR-W047-native-agent-research-and-workspaces.md)
and the [v1 contract](../../../docs/design/native-agent-capability-contract-v1.md).
`Extract(publicfetch.Page)` performs no I/O. It accepts HTML, Markdown and
plain text, retains final URL, media type and UTC retrieval/source timestamps,
normalizes line endings and caps text at 50,000 Unicode code points with an
explicit truncation flag. Last-Modified is optional upstream evidence, not a
verified publication date. Missing/invalid source dates remain absent.

Declared supported encodings decode to UTF-8. HTML without a declared charset
uses HTML encoding sniffing (including meta/BOM); other text defaults to UTF-8.
Unknown encodings, invalid UTF-8, binary controls, PDF and unsupported media
are refused. Transport 401/403/407 and WWW-Authenticate challenges, plus HTML
active HTML-namespace password inputs (including foreign integration points
and object fallback), produce a specific `permission_denied` challenge error.
Template/noscript contents and literal Markdown examples remain inert.
This is conservative detection, not a promise to recognize every CAPTCHA or
script-only login page. No authentication, browser, script or external asset
request is attempted.

HTML uses the tolerant HTML tree parser and caps retained output during construction,
so repeated links cannot amplify a bounded response into unbounded text.
Parsing still inspects the complete bounded response to detect challenges and
controls beyond the retained-text cap; the cap is not an early parsing exit.
Normalization follows HTML tree semantics, not CSS layout or script execution. Executable/embedded subtrees, comments and
attributes are discarded; headings retain line boundaries and HTTP/S links
are resolved against the final URL as inert text. Markdown uses pinned Goldmark v1.7.13 to identify actual HTML spans. It replaces
Markdown-only text with collision-checked literal markers, restored directly
as data after parsing one continuous HTML stream. Tree construction handles
implicit closures and foreign-content exits while preserving HTML
raw-text/RCDATA state across nodes; autolinks, escaped angle brackets, code
spans and fenced/indented examples remain literal data. Plain text is
literal data. Prompt-injection prose remains untrusted data: extraction never
interprets instructions. Consumers must not render output as active HTML or
infer authority from it; the canonical untrusted envelope remains required.

`NewStore` requires a current-policy authorization callback. `Put`, `Read` and
cursor-only `Continue` require trusted Person, Agent, task, resource and policy
revision context, and recheck the callback before cache access. The callback
must enforce current grants/provenance and revocation; this dormant package
is not itself the canonical authority resolver. Every insertion has a new
server-issued source ID and immutable revision over text, metadata and scope.
Cursors authenticate source/revision/byte position with a random per-store HMAC
key. They cannot switch scope, URL or revision. Reads never refetch upstream.

The process-local cache holds at most 128 normalized sources for 15 minutes;
reads do not renew expiry or retrieval timestamps. Expired sources are removed
on access/insertion. Restart invalidates all refs/cursors and returns
`stale_cursor`; this is not durable task-resume or artifact storage. No raw
response files, database migrations or search-query cache are introduced.

Text pages target 2,500 UTF-8 bytes without splitting code points. JSON escaping
and metadata shrink the page to a 3,500-byte encoded `ToolBody`, reserving 500
bytes for the gateway envelope and future budget fields. Metadata that cannot
fit fails explicitly, never emits partial JSON. Activation must measure the
complete final response after redaction and budget metadata; it must not rely
on gateway truncation. No transport limits are increased.

S24/S26 still own per-hop budget hooks, canonical tool registration, readiness,
grants and durable provenance wiring. Public retrieval remains dormant and
its intentionally absent per-hop budget hooks are unchanged. No fixture is
live DDGS/native model or deployment evidence.

```sh
cd keystone
go test -race -cover ./research/...
go vet ./research/...
```
