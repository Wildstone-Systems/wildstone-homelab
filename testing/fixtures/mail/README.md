# Hostile MIME corpus (epic #326, S04)

Raw RFC 5322 messages that the mail adapter's normalizer
(`keystone/adapter/mail/imapread/mimenorm`) and reader
(`keystone/adapter/mail/imapread`) must turn into bounded, explicit
outcomes. Mail is written by strangers: every file here is a shape a
real inbox will eventually receive.

`.gitattributes` keeps these byte-exact (CRLF line endings, non-UTF-8
bytes). Addresses use `example.internal` / `example.invalid`; nothing
here is real.

| File | Shape | Expected outcome |
|---|---|---|
| `plain.eml` | text/plain, the epic's school-night sentence | `ok` |
| `html-tracking.eml` | HTML-only newsletter: 1x1 tracking pixel, remote images, stylesheet, iframe, `utm_`/`fbclid` links, `javascript:` link, `display:none` and zero-font text aimed at a model, script | `ok`; no remote reference, hidden text or tracking parameter in the text |
| `ics-invite.eml` | multipart/mixed → alternative (plain, HTML, `text/calendar; method=REQUEST`) + `.ics` attachment + PDF with a path-traversal filename | `ok`; two raw calendar parts; PDF reported as `unsupported` with a bare name |
| `forwarded-invite.eml` | invite inside `message/rfc822` | `ok`; nested calendar found |
| `nested-deep.eml` | 20 levels of multipart (limit 16) | `too_deep` |
| `many-parts.eml` | 151 parts (limit 100) | `oversized` / `parts` |
| `malformed-no-boundary.eml` | multipart with no boundary parameter | `malformed` / `multipart_boundary` |
| `malformed-encoding.eml` | unknown `Content-Transfer-Encoding` beside a readable part | `malformed` / `transfer_encoding`; the readable part survives |
| `malformed-headers.eml` | no header section at all | `malformed` / `headers` |
| `prompt-injection.eml` | instructions to the agent, fake `X-Wildstone-*` authority headers, zero-width and bidi-override characters | `ok`; text kept verbatim as data, invisible characters and non-allowlisted headers gone |
| `iso-8859-1-qp.eml` | Latin-1 quoted-printable body, windows-1252 and Latin-1 encoded words | `ok`; decoded to UTF-8 |
| `iso-2022-jp.eml` | ISO-2022-JP body and encoded words | `ok`; decoded to UTF-8 |
| `unknown-charset.eml` | undeclared/unknown charset | `ok`; read as UTF-8, never guessed |
| `invalid-utf8.eml` | invalid UTF-8 bytes | `ok`; replaced with U+FFFD |
| `no-message-id.eml` | no Message-ID | `ok`; fingerprint still stable |

Scenarios that are about the mailbox rather than one message —
UIDVALIDITY reset, expunge between SEARCH and FETCH, duplicate FETCH
responses, a connection dropped at every body fetch, a 10 MiB+
message, oversized header sections — are driven programmatically
against the in-process fake server (`imapread/imapfake`), because
their subject is server behaviour, not bytes; see `reader_test.go`.

A new fixture is picked up automatically by
`TestEveryFixtureIsBoundedAndValidUTF8`, `TestHostileCorpusThroughTheReader`
and the `FuzzNormalize` seed corpus.
