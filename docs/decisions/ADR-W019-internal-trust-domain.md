# ADR-W019: The internal trust domain — internal names, an internal CA, and what stays convergent

- **Status:** accepted
- **Relates to:** ADR-W004 (Route), ADR-W005 (identity capability),
  ADR-W008 (reconcile), ADR-W011 A2 (ingress packages), ADR-W019 is the
  first ADR to touch container-to-container addressing.
- **Audience:** decision record AND implementation spec for the identity
  work package's networking half.

---

# Part A — Decisions (binding)

### A1. An app reaches another app by its Route host, resolved internally

Today `auth.<public_domain>` resolves only publicly. An app container
doing OIDC discovery — a **server-to-server** fetch of
`https://auth.<domain>/.well-known/openid-configuration` — therefore
egresses to Cloudflare and comes back through the tunnel to reach the
Authelia sitting beside it on the same bridge.

That works, and it was how the stack was first proven. It is still
wrong, for two reasons that compound:

1. **Every app login depends on the tunnel.** Inbound traffic already
   does, so this looks like no new dependency — but it is: a user
   already inside the tailnet, or on the LAN, now cannot log in when
   Cloudflare is having a bad day, for reasons entirely internal to
   their own house.
2. **It fails illegibly.** Discovery failure surfaces in the app as
   "provider not offered" or a silently absent login button. Nothing
   names the tunnel. The operator debugs the OIDC client.

So: the Route host resolves inside the Docker network, to the reverse
proxy that already serves it.

### A2. The scheme is https internally, or the whole thing is pointless

Resolving the name is not enough. Authelia refuses a non-secure
`session.cookies` scheme (4.39+), and more fundamentally an OIDC
**issuer is a string that must match** between what the app discovered
and what the browser was redirected to. If apps use `http://auth.…`
internally while browsers use `https://auth.…`, the issuer differs and
token validation fails — the internal path has to be the *same URL*, not
a parallel one.

That is the argument for A3, and it is why "internal names over plain
HTTP" was considered and rejected: it looks like the cheap version of
this decision and it does not work.

### A3. Caddy's own internal CA, not a Wildstone CA

Wildstone does **not** become a certificate authority. The reverse proxy
already has one: Caddy's `local_certs` issues from a built-in internal CA
and rotates on its own schedule.

Wildstone's job is therefore three small things, not a PKI:

- turn `local_certs` on for the internal listener,
- get the CA root out of Caddy and into the managed prefix,
- give each app container the root in the form its runtime reads
  (`REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`, and friends — this is
  per-runtime and belongs in the package, not the engine).

The rejected alternative — Wildstone issuing certs — costs issuance,
renewal, revocation and distribution, and adds a failure mode where one
expired internal cert breaks every app login simultaneously. For a
homelab that is a large standing burden bought with no gain: the trust
domain is a single host's bridge network.

**The cost we are accepting**: a CA root that Wildstone distributes but
does not control the lifetime of. If Caddy's data volume is lost, the CA
changes and every distributed bundle is stale until redistributed. That
is a real operational edge and the reason the root is treated as a
managed file (a pure function of what Caddy currently holds) rather than
as something copied once at install.

### A4. Public TLS termination does not move

Cloudflare (or the tailnet) still terminates TLS for traffic from
outside. The internal listener is additional, not a replacement, and the
base Caddyfile's `auto_https off` posture for the public listener stands
(ADR-W011 A2). Two listeners, two trust stories, one set of Route
definitions.

### A5. Cross-capability ordering stays convergent — deliberately

Nothing expresses "identity must be serving before photos' OIDC client
is usable", and nothing will. The reconciler is level-triggered: an
OidcClient whose provider is not yet up simply does not work yet, and
starts working when it does, with no operator action.

A dependency graph across capabilities would buy ordering we do not need
and cost a new class of stuck state — a plan that cannot proceed because
something it declares a dependency on is unhealthy for an unrelated
reason. `kindOrder` continues to order within a plan, which is where
ordering is actually load-bearing.

**What we owe instead is legibility.** The failure an operator sees must
name the cause: an app whose identity provider is not yet serving should
say so, rather than presenting a login that fails. That is a status and
diagnosis concern (ADR-W012's Butler `diagnose`, and the onboarding
probes), not a scheduling one.

---

# Part B — Implementation notes (non-binding)

- **B1. Network aliases.** `EnsureContainer` grows an alias list, applied
  to the container's network attachment. The ingress Caddy container
  takes one alias per Route host it serves, so Docker's embedded DNS
  resolves the real hostname to the proxy. This is a protocol change
  (the alias list is new wire surface) and therefore a minor bump.
- **B2. Where the root lands.** Caddy's PKI storage is pointed at a
  writable subtree of the managed prefix so the root certificate is a
  file the agent can already read, rather than requiring a new
  "read a file out of a container" op. The subtree is writable by the
  ingress container specifically — the managed prefix is otherwise
  mounted read-only (ADR-W011 A2), and this is the documented exception.
- **B3. Distribution is per-runtime.** There is no single environment
  variable for "trust this CA". Python wants `REQUESTS_CA_BUNDLE` and
  `SSL_CERT_FILE`, Node wants `NODE_EXTRA_CA_CERTS`, Java wants a
  truststore. Packages declare which, because packages already know what
  the app is written in.
- **B4. Sequencing.** A1–A3 are worth nothing until an app can be
  configured to use them, and every app package's config path currently
  runs through secret materialization in managed files. That lands
  first; this follows.

---

# Part C — Amendments (2026-09-04, binding)

Added after attempting A1–A3 on the homelab VM against v0.1.6-alpha.
Both are defects in the surrounding surface that A1–A3 assume and do not
themselves supply; neither changes a decision above.

### A6. The reverse proxy trusts the terminator in front of it

A4 says public TLS terminates upstream and the Caddy listener stays
plain http. That is right, and incomplete: Caddy replaces
`X-Forwarded-Proto` with the scheme of the request *it* received unless
the sender is a trusted proxy. So a correctly-terminated request reaches
Authelia claiming `http`, and Authelia refuses it outright:

    invalid X-Forwarded-Proto header value 'http'

Observed on `caddy-tailnet` with a local TLS front; `packages/ingress`
has the same `cloudflared -> Caddy(http) -> Authelia` shape and should
be assumed to fail identically until tested. **No OIDC request of any
kind succeeds through either shipped profile today** — this is not an
edge case, it is the whole feature.

So `renderBaseCaddyfile` emits `trusted_proxies` naming the terminator
that fronts that listener, and only it: the `cloudflared` container's
address for the public profile, the tailnet interface for the tailnet
profile. Never `0.0.0.0/0` — a proxy list that trusts everyone lets any
container on the bridge assert any scheme and any client IP, which is
the header-spoofing hole the list exists to close.

**Rejected: setting `X-Forwarded-Proto https` unconditionally.** It
produces the same result in the working case and lies in the failing
one. A request that genuinely arrived over http — a misconfigured
profile, a direct hit on the container — would be relabelled as secure
instead of rejected, converting a loud, correct failure into a silent
one that ends in a cookie sent in clear.

### A7. `.Resolved` carries the trust context

B3 requires each package to hand its runtime the internal CA root, in
whatever form that runtime reads. Nothing supplies the input: a package
cannot ask whether an internal CA exists, or where its root landed.
Today `.Resolved` carries `IssuerURL` and `OidcSecretRef` and nothing
else, and `IssuerURL` is built by concatenating `"https://auth." +
domain` — so a package cannot distinguish "TLS terminates at Cloudflare
with a public cert" from "TLS terminates at a Caddy internal CA whose
root you must trust", and those need different container config.

`.Resolved` therefore grows one documented key,
`InternalCaBundlePath`: the path, inside the managed prefix, of the root
the ingress profile currently holds — empty when the active profile
terminates with a publicly-trusted certificate and there is nothing to
trust. Small and explicit, per ADR-W005 A2. Packages render
`REQUESTS_CA_BUNDLE` and friends from its presence, never from a test
against a profile name — which is what makes the ingress profiles
genuinely swappable rather than swappable-in-principle.

### A8. The ingress profile's posture is a resource, not a constant or a guess

B5 said `trusted_proxies` should be "a parameter of the ingress package,
not a constant in the renderer". Implementing A1–A3 showed the same
channel is needed for a second, larger fact — whether this proxy
terminates TLS at all — and that neither can be a constant once two
profiles ship: `caddy-cloudflared` sits behind a tunnel that terminates
for it, and `caddy-tailnet` has nothing in front and must terminate its
own. The two need opposite output from the same renderers.

So the channel is a resource kind, `Ingress`, carrying `tls`
(`off`/`internal`) and `trustedProxies`, declared by whichever ingress
package is installed. A resource for the reason ADR-W005 gives and the
Secret kind followed: everything else a package causes to happen on a
node is one, and being one means it inherits the closed registry,
validation, plan ordering, approval and audit without rebuilding any of
them.

A node with no `Ingress` declared renders exactly what it rendered
before the kind existed — terminated upstream, private ranges trusted.
An upgrade must not silently change how a household's front door handles
TLS.

**Rejected: inferring it from the published port.** "The container
publishes 443, so it terminates TLS" is derivable from desired state and
would have needed no new kind. It is also precisely the kind of implicit
heuristic this repo refuses everywhere else, and it would have been
load-bearing for whether the family's traffic is encrypted. A closed,
declared vocabulary is worth a resource kind here.

---

# Part D — Implementation notes for the amendments (non-binding)

- **B5. `trusted_proxies` values are per-profile**, so the value is a
  parameter of the ingress package, not a constant in the renderer:
  the renderer is told what to trust and by whom.
- **B6. A6 lands before A1–A3.** It is a one-line change to a rendered
  config and it unblocks the OIDC path in profiles that already ship,
  whereas A1–A3 are the larger internal-listener build. Sequenced the
  other way, the internal work cannot be verified end to end, because
  the browser half of every flow still fails.
- **B7. A regression test worth having**: assert that a request arriving
  with `X-Forwarded-Proto: https` from the trusted terminator reaches
  the backend still saying `https`. The bug is invisible to any test
  that speaks to Authelia directly, which is why it survived to a VM.

# Part E — Amendment (2026-09-05, binding)

### A9. A third posture: a publicly trusted certificate over DNS-01

A3 chose Caddy's internal CA, and the reasoning holds for what it was
answering: a hostname no public CA would sign still gets real TLS, and
Wildstone does not become a CA to do it. What A3 did not weigh is the
cost the internal CA pushes onto every device that will ever open a
page — installing a root certificate, by hand, per device.

That step does not get done. The browser says "not secure", somebody
clicks through, and the encryption is still there but the thing it was
supposed to establish — that this is the site it claims to be — is gone.
An internal CA is only trustworthy to someone who has been told to trust
it, and a household has not been. Worse, "click through the warning" is
a habit, and it is the exact habit that makes the warning useless
somewhere it matters.

So `IngressSpec.tls` gains a third value, `public`: this proxy holds the
certificate, and a certificate authority the world already trusts issued
it. `internal` is unchanged and remains right for a deployment with no
domain.

**The challenge is answered in DNS, and it has to be.** These names
resolve to a tailnet address the internet cannot route to, so HTTP-01
and TLS-ALPN-01 — both of which require the CA to connect to the
listener — cannot ever succeed. DNS-01 asks the CA to believe the zone
instead of the socket, which is the only ACME challenge compatible with
a front door that is deliberately unreachable. `acmeDns` names the
provider module, and validation refuses `public` without it: a profile
that asked for a public certificate and named no way to get one would
not fail to install, it would serve an untrusted certificate — the exact
state the operator chose this profile to leave.

**What it costs, said plainly.** This posture needs a real domain in a
zone the operator controls and an API token that can edit it. That is a
strictly larger ask than `internal`, which needs nothing, and it is why
this is a third posture rather than a replacement for the second. It
also puts a zone-editing credential on the node, so the token travels in
the environment from a Provided Secret and never into a managed file:
files under the managed prefix are readable by anything that can read
the node's config, and this credential can obtain a certificate for any
name in the zone.

**Rejected: Tailscale's own certificates.** `tailscale cert` issues
publicly trusted certificates for a device's MagicDNS name with no
domain and no token, which is a genuinely smaller ask. It was rejected
because it issues for exactly one FQDN per device — no wildcards, no
subdomains — and the Route model is host-based, with `auth.<domain>`
load-bearing for Authelia's OIDC issuer and redirect URIs. Adopting it
would have meant collapsing every service onto one hostname distinguished
by port or path, i.e. changing the routing model to suit the certificate
mechanism. The certificate is the smaller thing; it should bend.

**Rejected: shipping the internal root to devices automatically.** There
is no mechanism to do this that does not amount to writing an MDM, and a
household has no fleet management. The manual step was not the
implementation's failure, it was the approach's.

# Part F — Amendment (2026-09-05, binding)

### A10. A tailnet posture, and the addressing it forces

A9 removed the manual trust step at the cost of a domain. That cost is
not payable on day one: a household installing this has no domain, and
telling them to buy one before anything works is the same barrier A9
existed to remove, moved earlier.

Tailscale issues publicly trusted certificates for a node's own MagicDNS
name, with no domain and no credential, and Caddy fetches them from a
locally-running `tailscaled` with no configuration whatsoever. So
`IngressSpec.tls` gains `tailscale`.

**The certificate covers exactly one name.** No wildcards, no
subdomains, and no way to add records under `ts.net`. Four Routes that
each declared a hostname cannot each have one. They share the node's
name and are distinguished by PORT.

**So a Route's `host` becomes its identity, not its address.** Under
every other posture the two coincide. Under this one the declared host
stays what the package chose, what policy reads and what an operator
recognises, while `routeSiteAddress` decides where it is actually
served. That separation is what lets every service package remain
untouched while the addressing model underneath changes completely —
the alternative was rewriting the Route in each of them, which would
have made the addressing model a property of the catalog rather than of
the deployment.

**Ports are assigned by Keystone, not declared by packages**, for the
same reason hostnames are not: a package knows what it is, not what else
is installed beside it, and a collision is a fact about the neighbours.
The shipped capabilities are pinned to fixed ports so that installing a
new service never moves an existing one — a household's bookmarks should
not break because somebody added something unrelated. The dashboard
holds 443, so the address people actually type has no port in it.

**Rejected: assignment by sorted position.** Simpler, fully
deterministic, and it moves every service alphabetically after whatever
was just installed. Determinism is necessary here — the reconciler
compares rendered bytes to decide NoOp versus Update — but it is not
sufficient, because a deterministic function of the whole set is still
free to churn when the set changes.

**Rejected: the caddy-tailscale plugin.** It would give each Route its
own tailnet node and therefore its own real hostname, which is strictly
nicer than ports. It is badged experimental by Tailscale, and using it
means building and publishing our own Caddy image instead of pinning an
official one. Neither belongs in the path a non-technical household
installs by default. The plugin is the right thing to revisit if it
stabilises.

**Known incomplete.** Authelia's issuer and redirect URIs are derived in
`capability.buildResolved` as `https://auth.{public_domain}`, from the
catalog's declared DEFAULTS. A ported origin is not derivable from
defaults — it depends on the installed ingress profile's actual params —
so a login under this posture is not yet correct. The `.Resolved`
channel needs to see installed params, not just declared ones, and that
is the next change rather than a workaround here.
