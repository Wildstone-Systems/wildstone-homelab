# keystone/provider/cloudflare

Cloudflare DNS `Provider` implementation for `resource.KindDnsRecord`
(ADR-W004 A5, work package S2c).

## Test coverage

All automated tests in this package (`cloudflare_test.go`, `fake_cf_test.go`)
run against an in-memory `httptest` fake of the Cloudflare API v4
(`fakeCF`). No test in this package, or anywhere else in the `keystone`
module, makes a real network call to Cloudflare.

## Live-Cloudflare verification (deferred, manual, owner-run only)

Spike S2's scope explicitly defers the single "real token, real zone"
end-to-end check to a manual step run by the platform owner:

1. Create a Cloudflare API token scoped to **exactly one zone** and to the
   `Zone.DNS` permission (Edit), nothing broader.
2. Export it out-of-band (e.g. `export CF_TEST_TOKEN=...`, never committed
   or logged) and point a throwaway `cloudflare.Provider{APIToken: ...,
   BaseURL: cloudflare.DefaultBaseURL}` at a real test record in that zone.
3. Run `Observe`/`Create`/`Update`/`Delete` by hand (or a short local
   `go run` scratch program, not a checked-in test) against that one
   record, then delete it.

This MUST NOT be automated into a Go test that runs under `go test ./...`
or in CI: CI has no Cloudflare credentials, and even a scoped token
must never be shared with, or exercised by, a CI environment. There is
deliberately no `_test.go` file in this package that dials a real host —
if one is ever added, it must be built behind a manual-only tag (e.g.
`//go:build cloudflare_live`) that CI never passes, and must never read a
token from a committed file.
