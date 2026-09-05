# ADR-W021: Installing a capability — where expansion lives, and what a catalog is

- **Status:** proposed
- **Relates to:** ADR-W005 (capability engine, `.Resolved`), ADR-W007
  (there is ONE API), ADR-W010 (package artifact, signed registry),
  ADR-W015 (`wild` is an API client), ADR-W016 A2 (Console IA),
  ADR-W019 A7 (trust context on `.Resolved`), ADR-W020 (onboarding)
- **Audience:** decision record AND implementation spec for installing a
  service from the Console.

---

Enabling a capability already does most of the work. One approved plan
creates the volumes and containers, generates the secrets the package
declares, adds the OIDC client to Authelia's merged clients file, adds
its `access_control` rule, renders the Caddy route fragment, and
declares the LDAP group. What it cannot do is start from the Console,
and the reason is not the packages.

# Part A — Decisions (binding)

### A1. Expansion moves into Keystone; the CLI stops rendering

`wild capability enable` loads a package **directory**, expands the
templates locally with `capability.Engine`, and POSTs the rendered
resource documents to the generic `POST /api/v1/plans`. Keystone has
never seen a package.

Three things follow from that, and all three are defects:

- **The Console cannot install anything.** It has no filesystem and no
  Go template engine. There is no endpoint to call.
- **Provenance is lost.** The plan records fourteen documents, not
  "photos-immich 1.0.0 with `public_domain=…`". Audit cannot answer what
  was installed, and neither can an upgrade.
- **`requires` resolves against whatever directories the operator
  passed** — the `-catalog` flags — rather than against what the control
  plane has enabled. That is F40, and it is a direct consequence of
  rendering happening somewhere that does not know the deployment.

So: `POST /api/v1/capabilities` takes `{package, version, params}`,
Keystone expands, and the result is proposed as an ordinary plan. `wild`
becomes a client of that endpoint like everything else it does
(ADR-W015), and the Console gains the same capability by construction —
which is the whole point of there being ONE API (ADR-W007).

**Rejected: leaving rendering in the CLI** and having the Console shell
out to it or reimplement the engine in TypeScript. Two engines is two
behaviours, and the second one diverges silently — the same argument
ADR-W007 makes for one API, applied one level down.

### A2. A capability plan records what was ASKED FOR, not only what it rendered

The plan stores the package name, its version, and the params it was
expanded with, alongside the rendered documents.

Without it, a re-render is not reproducible, an upgrade (ADR-W010 A4)
has no previous input to diff against, and the audit trail describes
consequences rather than intent. "Fourteen documents changed" is not an
answer to "what did we install".

### A3. The catalog is Keystone's, and has two sources

- **Bundled with the release.** The shipped packages travel as a release
  asset, verified through `checksums.txt` like every other artifact
  (ADR-W014 A1) — the mechanism `console_dist.tar.gz` already uses. This
  is the default, always present, and works air-gapped.
- **A registry URL.** ADR-W010 A3's signed directory-over-HTTP, fetched
  and ed25519-verified before the archive is parsed.

Bundled is not optional and registry is additive. A homelab must not
need to host a registry to install the packages that shipped with the
product; equally, a catalog that can only ever be the shipped set
forecloses the third-party packages ADR-W010 exists to allow.

**Rejected: pointing Keystone at a directory of package sources on the
host.** That is what the CLI does today, it is why F40 exists, and a
control plane whose catalog is "whatever is on this disk" cannot answer
what it is able to install.

### A4. `requires` resolves against enabled capabilities plus the catalog

Once Keystone expands, it already knows which capabilities are enabled —
that is Statefile state — and what the catalog holds. A requirement is
satisfied by an enabled provider, and the operator never restates a
dependency by pointing at its source tree.

`-catalog` on the CLI survives as a **lint-only** affordance, for
authoring a package against providers that are not installed anywhere
yet. It stops being part of installing.

### A5. The install form is generated from the package, and asks for provided secrets BEFORE approval

A package already declares its `params` with `required` and `default`.
That is a form.

More usefully: with ADR-W020's `Secret` declarations, a package now also
states which secrets **only the operator can supply** (`provided: true`
— a Cloudflare tunnel token, Paperless's provider block). Those are
knowable from a render, before anything is applied. The install
interaction therefore collects them **in the same screen as the params**,
and the plan is not proposed until they are present.

This is the direct fix for the shape of F37: an operator meeting a
missing secret as a failed reconcile tick, after approving a plan that
could not have worked, is a sequence the control plane had enough
information to avoid.

It requires a **preview**: expand with the given params and return the
plan-to-be plus the provided-secret list, without proposing anything.

### A6. The dashboard's service list is a renderer, not a package concern

Homepage's `services.yaml` is rendered from the desired `Route` set, as
a node prereq, exactly like the Caddy fragments, the Authelia clients
file, the access-control block and the LLDAP lists (#69).

**Rejected: each package declaring its own homepage entry.** It would
duplicate the host its own Route already states, and it is the same
"declaring half of a relationship" trap `LdapGroupSpec` names — two
places that must agree, one of which nobody updates.

### A7. Installing from the Console does not bypass the approval gate

A Console install **proposes**. A human approves. ADR-W016 A2 calls
approval the interaction the whole architecture funnels through, and
that does not weaken because the trigger became a button.

---

# Part B — Implementation notes (non-binding)

- **B1. `ParamDecl` is too thin to generate a good form.** It is
  `{Name, Required, Default}`: no type, no description, no enumeration,
  every value a string. A form built on it can only render labelled text
  boxes with no help. Adding `description`, `type` and `enum` is additive,
  lint can enforce them, and it is the smallest change that makes A5
  pleasant rather than merely possible.
- **B2. Catalog packaging.** Bundle as `packages.tar.gz` beside
  `console_dist.tar.gz`; `checksums.txt` is already the manifest, and
  `install.sh` already grew `fetch_asset`/`release_has` for exactly this
  shape of optional asset.
- **B3. Endpoint sketch.** `POST /api/v1/capabilities` with
  `{package, version, params, dryRun}`; `dryRun` returns the rendered
  plan and the provided-secret names, proposes nothing.
  `GET /api/v1/packages` lists the catalog with params and provides/requires,
  which is what the Console browses.
- **B4. Verify before parsing.** A registry archive is ed25519-verified
  (ADR-W010 A2) before it is unpacked or parsed. An unverified package is
  attacker-controlled template input to the engine.
- **B5. The scheme problem is now load-bearing twice.** A6's renderer
  needs the browser-facing scheme, and no ingress profile declares one —
  the same gap as ADR-W019 A7's trust context. Whichever lands first
  should define it for both.
- **B6. Sequencing.** A1 and A2 first: they unblock everything and are
  testable without any UI. Then A3 bundled-only, then A5's preview, then
  A6. The registry source and B1's richer params come last — both are
  additive and neither blocks a Console install of the shipped set.
