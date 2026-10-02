# Person-facing artifacts (#485 / ADR-W047)

Console and CLI use authenticated `GET /api/v1/agents/{agent}/artifacts`.
Household SSO uses the same family under
`/api/v1/assistant/agents/{agent}/artifacts`, through the existing Assistant
portal door. The latter admits only Member Agents, including when the requesting
Person is an Owner. Console reads require the Person's own admin credential;
machine names and delegated Agent credentials cannot acquire these reads.

Each family has these suffixes:

| Suffix | Response |
| --- | --- |
| none | `{items: FileInfo[], nextCursor?}` of current committed private files |
| `/{artifact}/revisions` | Same page shape for retained committed revisions |
| `/{artifact}/revisions/{revision}` | `{file: FileInfo, text, html, truncated, nextCursor?}` |
| `/{artifact}/revisions/{revision}/download` | Original bytes as an inert attachment |

`FileInfo` contains stable `artifactId`, `revision`, scoped logical `path`,
`scope: "private"`, `format`, `mediaType`, `sizeBytes`, `updatedAt`, and canonical
same-origin `previewUrl`/`downloadUrl`. IDs and links are selectors; every access
requires current Person admission, ownership, explicit Agent access, class and
surface ceilings, and retained source authority. Owner administration does not
reveal another Person's files. Retired/disabled Agents retain human reads;
revoked grants or Person admission do not. Pending and trashed files are absent.
Shared scope remains refused until explicit sharing grants are implemented.

Pages contain at most 20 metadata items. Preview pages contain at most 32 KiB of
UTF-8 text, ending at a complete character. Markdown, text, JSON and CSV all
preview as escaped text in `<pre>`; there is no Markdown HTML/link/embed renderer.
The `html` field contains only this fixed wrapper and escaped, normally redacted
text. Clients must not run a separate Markdown/HTML renderer over `text`.
Preview/path/attachment filename values retain normal secret-shape redaction.
Preview source text is redacted in full before pagination, so a value crossing
a page boundary cannot survive as separate fragments. Preview cursor offsets
address the redacted text; attachment bytes and committed size metadata remain
the original revision's.
The narrow typed response encoder preserves authoritative selectors, canonical
links, server-issued cursors, and the already-redacted preview text/escaped HTML.
It independently redacts file metadata and paths. Authenticated downloads return
the original revision bytes outside model context.

`cursor` is a server-issued opaque handle (15-minute expiry, bounded process-local
store, invalid after restart), bound to Person, Agent, surface, route selectors
and the authorized metadata snapshot. Pages reauthorize; changed snapshots return
409 `stale_cursor`. Query keys are only `cursor` and optional `scope=private`.
Changing a handle's resource/surface does not confer authority. File names are
never host paths. Missing and hidden files both return the same 404; storage
errors are generic 503s without underlying paths or byte contents.

All responses use `private, no-store`, `nosniff`, a restrictive CSP and
`no-referrer`. Downloads use `application/octet-stream` and attachment
disposition. Production reads reload workspace readiness, use confined
`storage.Store.Read` with committed hash/size metadata, and repeat authority and
committed metadata checks after byte I/O. This small read call can later use
#480's reviewed `ReadCommittedBytes` helper; the API has no model gateway
dependency. Human mutations, sharing, full browser UI and live deployment are
outside this slice.
