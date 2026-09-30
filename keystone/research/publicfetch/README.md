# Public retrieval transport (S21)

`Fetcher.Fetch(ctx, url)` implements the dormant transport portion of
[ADR-W047](../../../docs/decisions/ADR-W047-native-agent-research-and-workspaces.md)
and its [v1 contract](../../../docs/design/native-agent-capability-contract-v1.md).
It returns untrusted HTML, Markdown or plain-text bytes for S22 extraction.
It does not register `web_fetch`, write files, configure providers or grant tools.

Each request accepts only a bounded HTTP/S URL without userinfo. All DNS answers
must be public before one literal IP is dialed; Host and TLS validation retain
the original hostname. Every redirect resolves and validates again. There are
at most five redirects, no retries or connection reuse, and one 30-second context
deadline across resolution, dialing, TLS, redirects and body reading. A failed
first address fails the request rather than silently trying other addresses.

The transport does not use environment proxies, a cookie jar, custom credentials,
or the default HTTP transport. Redirects receive fresh requests without cookies,
authorization or Referer. Internal provider connectivity is a separate concern
and cannot exempt a public fetch from validation.

Both downloaded representation bytes and decompressed bytes have independent
5-MiB ceilings (one extra byte is read to detect overflow). Gzip and identity are
supported; other encodings, non-text media, statuses other than 200 OK (including
unsolicited 206 Partial Content), and incomplete bodies are refused. Headers are limited to 64 KiB. No partial page is returned
on failure. Errors are fixed, contract-prefixed messages without upstream
URLs, query strings, addresses, bodies or network diagnostics.

The IP policy conservatively refuses the nonpublic IPv4 special-use ranges,
IPv6 outside `2000::/3`, IPv6 special-use/transition/documentation ranges, and
the Azure virtual host service address. IPv4-mapped IPv6 is checked as IPv4.
The special-use classifications were checked against the IANA
[IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry/) and
[IPv6](https://www.iana.org/assignments/iana-ipv6-special-registry/) registries.
Some public exceptions within reserved blocks are intentionally refused.

This package is not an authorization boundary. Before activation, the canonical
gateway slices must enforce current Person/Agent grants, trusted provenance and
egress policy, provider readiness, task budgets and cancellation. In particular,
S24/S26 must integrate per-attempt accounting before every hop's DNS/network I/O;
one charge around an entire redirecting Fetch is insufficient. No tool or route
currently calls this package, so it remains dormant until that integration exists.
The limits here are hard upper bounds; lower configurable limits and accounting
belong to that integration. These fixtures are not live DDGS/model evidence.

Run deterministic adverse-path fixtures with:

```sh
cd keystone
go test -race ./research/publicfetch
```

Fixtures use fake DNS and in-memory pipe dialers; the TLS refusal fixture uses
only a local test server reached through the private dial seam. Asserted public
addresses are never contacted.
