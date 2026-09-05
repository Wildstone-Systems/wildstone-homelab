# ADR-W022: The addressing ladder — how a household reaches its own services

- **Status:** proposed
- **Relates to:** ADR-W019 (internal trust domain; A8 the `Ingress`
  kind, A9 DNS-01, A10 the tailnet posture and its addressing),
  ADR-W004 A5 (`DnsRecord` through the Cloudflare provider),
  ADR-W011 (infrastructure packages), ADR-W021 (installing a capability)
- **Audience:** decision record for what an operator must configure to
  reach their services from where they actually are.

---

Every posture in ADR-W019 answers "who signed the certificate". None of
them answers the question a household actually asks, which is "why does
the link work at home and not on my phone". That question is about
resolution, not trust, and it has been decided implicitly three times:
`nip.io` encoding a LAN address, a wildcard `A` record, MagicDNS. This
records it once.

The forcing observation: **a link is a string, and it is rendered once.**
The dashboard writes `href: https://...` into a static file. Whatever is
in that string has to be correct from the kitchen, from a phone on
cellular, and from a laptop in another country — or the household learns
that some links work sometimes, which is worse than a link that never
works, because nobody can tell you what is broken.

# Part A — Decisions (binding)

### A1. Resolution is DNS's problem, not the application's

The tempting fix is to make the link depend on the request: detect where
someone came from — a header from the proxy, something Authelia knows —
and render a different address for them.

Rejected. Homepage renders a static file and has no per-request
templating, so honouring this would mean replacing it with something
dynamic. Every service's own internal links and redirects would need the
same treatment, and the system would have acquired a URL rewriter as a
core component. The information is not missing from the system; it is
simply being asked for at the wrong layer. One name that resolves
differently depending on who asks is what DNS has always done.

So: **one name per service, everywhere. The answer varies, the question
does not.**

### A2. On the tailnet, routing already solves the local case

Split-horizon exists to hand a nearby client a nearby address. For a
client on the tailnet that is unnecessary: Tailscale establishes a
DIRECT connection between peers on the same network, so a laptop in the
same house reaches the homelab across the LAN, not through the internet.

Therefore one DNS answer — the node's tailnet address — is correct in
every location for every tailnet client, and the "at home it should use
my LAN" requirement is met by routing rather than by naming.

Split-horizon returns only for devices that CANNOT join the tailnet: a
television, a guest's phone. That is a real case and A5 covers it, but
it is a smaller case than it first appears, and the design must not pay
for it up front.

### A3. Three tiers, and what each costs the operator

The tiers are ordered by what the operator must possess, not by how good
the result is. Nobody should have to buy anything to reach tier 0.

**Tier 0 — the tailnet name. Nothing to configure.**
`<node>.<tailnet>.ts.net`, resolved by MagicDNS, certificate issued by
Tailscale (ADR-W019 A10). There is no DNS setup because there are no
records to create. Costs: every service shares one hostname and is told
apart by port, so the addresses have port numbers in them.

**Tier 1 — an owned domain. One credential, no DNS work.**
The operator supplies a domain and a Cloudflare API token. Wildstone
writes the records itself through the `DnsRecord` provider (ADR-W004
A5): a wildcard `A` for the zone pointing at the node's tailnet address,
unproxied. The certificate is a real wildcard obtained over DNS-01
(ADR-W019 A9). Real hostnames return; ports go away. The operator never
opens the Cloudflare dashboard.

**Tier 2 — LAN resolution for devices that cannot join the tailnet.**
A DNS server on the node, published either through the Tailscale DNS API
or by setting one field in the household's router. This is the only tier
where an operator touches network infrastructure, and it is optional.

### A4. The floor on tier 1 is one domain and one token, and that is not reducible

A certificate authority will not issue for a name the requester cannot
prove they control, and proving control requires a credential. Any
design that claims to give publicly trusted certificates on a real
domain with no operator input is either wrong or is quietly using
somebody else's domain.

So tier 1's cost is honest and fixed: a domain name and one API token,
pasted once. Everything downstream of that — zone records, challenge,
renewal, the wildcard — is the product's job and none of it is the
operator's.

### A5. Tier 2 is a capability, not a runbook

When it is built it ships as a package like everything else: a DNS
server declared as a Container, its records rendered from the Route set
the way the dashboard's service list already is (ADR-W021 A6), and
published to the tailnet through the Tailscale API where a credential
allows it. A documentation page telling an operator which fields to type
into an unknown router is not an acceptable substitute; it is the
failure mode this product exists to remove.

# Part B — Implementation notes (non-binding)

- **B1. Tiers 0 and 1 are built.** Tier 0 is `ingress-tailnet-ts`; tier 1
  is `ingress-tailnet-public`. What remains is the upgrade path between
  them being a supported move rather than a manual reinstall — see B4.
- **B2. Publishing a tailnet address in public DNS is deliberate.** A
  tier 1 wildcard resolves to a `100.64.0.0/10` address, which is
  unroutable from the internet. It reveals that the household uses
  Tailscale and nothing else. Stated because it looks alarming in a zone
  file and should not be rediscovered as a surprise.
- **B3. Tier 1 does not serve the general public.** A client on neither
  the tailnet nor the LAN gets a name resolving to an address it cannot
  reach. Genuine public exposure is the Cloudflare TUNNEL profile
  (`packages/ingress`), which is a different posture with a different
  threat model — not a further rung on this ladder.
- **B4. Moving between tiers currently means removing the old ingress
  capability and installing another**, because the API has no uninstall
  and two ingress profiles both bind :443. The switch is expressible as
  one proposal — `delete` the old proxy, `put` the new — but nothing
  offers it as a single operation, and an operator discovers the
  collision by having the new container fail to start.
- **B5. Every browser-visible string must come from `.Resolved`.** The
  addressing tier changes what a URL is, and anything assembling one
  from `public_domain` will be wrong on tiers 0 and 2 while every check
  still passes. Authelia's `authelia_url`, its session cookie domain and
  its default redirect were exactly this bug.
