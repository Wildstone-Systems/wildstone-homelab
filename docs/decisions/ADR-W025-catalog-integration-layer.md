# ADR-W025: The catalog integration layer — adding a service should be data, not a patch

- **Status:** proposed
- **Relates to:** ADR-W005 (package format, the Capability Engine,
  `.Resolved`), ADR-W010 (package toolchain, lint as the contract test,
  the closed-registry posture), ADR-W019 A10 (addressing under the
  tailnet posture), ADR-W021 (the catalog is Keystone's; `requires`
  resolves against what is enabled), ADR-W024 A10 (`publishes` and the
  three shim shapes). A6 amends the shape heuristics ADR-W009 A2's
  `secret-values-forbidden` rule and ADR-W007 A5's redaction share; it
  narrows what those count as a secret VALUE and changes neither rule.
- **Audience:** decision record AND implementation spec for the layer a
  package author writes against. Part A is binding. Part B is the build
  order for the rest of the layer, and is not decided here.

## Conditional amendment from ADR-W030

This amendment becomes binding when ADR-W030 is accepted. The catalog adds a
connection-only integration shape for an application Wildstone does not own.
Its package declares the application contract—events, queries, typed actions,
and optional indexing—while an application-specific adapter implements it.
One adapter implementation supports multiple named connections containing
endpoint, secret references, identity mappings, declared scopes, and grants.
Exact schemas land with the adapter implementation slice.

Each named connection declares a primary node and an ordered set of eligible
fallback nodes. Only those nodes receive its credentials. Keystone may move a
single fenced adapter lease within the approved set after node failure and may
fail back after a health stabilization period. With no eligible node, the
connection is unavailable and work waits; discovery never expands placement.
Adding/removing nodes follows the Plan, provisioning, erasure, and remote
revocation gates in W030/W012.

Individual connections default to personal, read-only, live-query-only, and
local-processing-only. Connections explicitly created for household
infrastructure default to household-shared and read-only. Selected indexing,
cloud routing, mutating actions, and wider audiences require separate explicit
choices; capability discovery never turns them on.

V1 adapter runtimes are package-managed containers on Wildstone nodes. The
connection owns their configuration and lifecycle, never the remote app's.
Capability discovery verifies app version, configuration, and credential
scope, but newly discovered capability remains unavailable until approved.
Removing a connection removes Wildstone's adapter/configuration and does not
delete or reconfigure the external application or its native routines.

---

## The intent, in one sentence

Adding a service to the catalog — and integrating it with the services
already there — should be something a person does by writing a
`package.yaml`, with the work on the service's own side as close to
nothing as the service allows.

## What was already true, and what was not

Most of this was already right. A package is data: templates that expand
into typed resources, `requires`/`provides` traits, params, policies,
`publishes`. `packages/home-automation` proves the far end of the
ambition — it runs no container at all, templates one config file, and
makes a Home Assistant somebody else already installed publish into the
household stream. That is the shim shape we want to be easy.

Two things were not right, and both are about the SEAM between packages
rather than about packages themselves.

**The vocabulary had no written form.** `trait` was a free string on both
sides. `requires: [{trait: identiy, mode: oidc}]` was not a malformed
package — merely one that nothing satisfied — and it failed at install
time with "unsatisfied requirement identiy:oidc", a message that names the
typo without saying it is one, in front of an operator who did not write
the package. On the `provides` side there was no failure at all: a package
advertising a name nobody else spells the same way is silently useful to
nobody. And there was nowhere to look up what the vocabulary WAS. An
author's route to "what can I require?" was reading the shipped packages
and inferring the pattern, which finds `identity:oidc` and misses that
`.Resolved.IssuerURL` comes with it.

**`.Resolved` was hardcoded Go.** The map a template renders over was
built by `keystone/capability`, which looked up `"identity"`/`"oidc"` by
name and computed `IssuerURL`, `SessionDomain` and `DashboardURL` itself.
That works exactly once. A second provider wanting to hand a consumer
anything — a database its DSN, a broker its address — was not a package
someone wrote, it was a patch to the engine. So "add a service to the
catalog" was data and "integrate with a service in the catalog" was a code
change, which is the wrong way round for the half we most want other
people doing.

---

# Part A — Decisions (binding)

### A1. The trait vocabulary is closed, written down, and lives in `keystone/trait`

Every trait a package may `provide` or `require` is an entry in one Go
file: its name, what it means, its modes, its classes, whether the system
satisfies it structurally, and — for each mode — the `.Resolved` keys it
carries.

**Closed, like every other vocabulary here** (`policy.Classifications`,
`pkgspec`'s publish shapes, the registry index). A vocabulary that grows by
guessing cannot tell a rule from an accident: nobody can say whether
`trait: storag` was refused because it is wrong or accepted because the
check is weak. Adding a trait is a reviewed change to one table.

That closure is narrower than it sounds, and the narrowness is the point.
What a household actually adds — a service, its containers, its routes,
its events, the values it hands to others — stays entirely in a
`package.yaml`. Only the NAMES packages meet each other under are closed.

Three consequences:

- **`wild pkg traits`** prints it. A closed vocabulary nobody can list is
  indistinguishable from an arbitrary one.
- **Lint refuses an unknown trait, mode, or class**, naming the ones that
  exist (`keystone/pkgtool`, rule `trait`). Vocabulary checks live in
  lint and not in the parser, exactly as `policies.classifications` does:
  `pkgspec` must stay able to LOAD a package written against a vocabulary
  this binary does not have, so that the diagnostic comes from the tool
  built to explain it rather than from a parser refusing to start.
- **Structural traits are the vocabulary's to declare.** `ingress` and
  `storage` are satisfied by the system rather than by a package
  (ADR-W010 A1), and that fact moved out of a map literal inside the
  engine. The engine and lint reading one table is what stops them
  disagreeing about which requirements can be met with no provider — a
  disagreement whose symptom is one of them refusing a package the other
  installs.

**Rejected: an open vocabulary with a warning.** A warning nobody must
act on is a warning everybody stops reading, and the failure it warns
about is invisible rather than loud.

**Rejected: a `traits.yaml` data file shipped alongside packages.** The
engine's structural-trait behaviour and the export functions are code;
splitting the names from the behaviour they drive means the two can
disagree, which is the defect this ADR exists to remove.

### A2. A provider declares what it hands over; the engine stops knowing what identity is

A `provides` entry may carry an `exports` block: one template per
`.Resolved` key the trait mode declares.

```yaml
provides:
  - trait: identity
    mode: oidc
    exports:
      IssuerURL: '{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}'
      SessionDomain: '{{ with .Params.public_domain }}{{ sessionDomain . }}{{ end }}'
      DashboardURL: '{{ with .Params.public_domain }}{{ origin (printf "home.%s" .) }}{{ end }}'
```

The three OIDC values are unchanged in every posture; they simply live in
`packages/identity` now instead of in `capability.buildResolved`.

The rules, each because of a specific way this can go wrong:

- **An export renders over the PROVIDER's own params**, plus the
  functions in `trait.ExportFuncs`. An export answers "what do I hand my
  consumers", and the only inputs to that are what this package was
  configured with and where this deployment serves things.
- **The functions are closed and few** — `origin`, `sessionDomain` — and
  they exist so that an export cannot get addressing wrong by
  concatenating strings. Under the tailnet posture a Route is served at
  the node's one certified name plus a port, not at the host it declares
  (ADR-W019 A10); an OIDC issuer is compared byte for byte against what
  the browser saw, and a value derived from the declared host produces a
  login loop that names nothing.
- **An export that renders EMPTY is not published.** A portal origin
  built from a domain nobody has supplied yet is `https://auth.`, which is
  not a better answer than no answer — it is a service that starts,
  accepts a login attempt, and loops. Publishing nothing makes the
  consumer's own template fail on a missing key, which names the problem.
  This is why exports are written with `with`.
- **Intrinsic keys cannot be overridden.** `OidcSecretRef`,
  `PublisherSecretRef` and `TailnetHost` come from the package itself or
  the node, and the engine ACTS on the first two — it synthesises the
  matching `Secret` resources. A package that could redefine the
  reference would produce a template pointing at one name and a Secret
  created under another, with nothing comparing them.
- **First provider in catalog order wins a contested key**, which is the
  rule the hardcoded lookup had. Two modes carrying the same key is
  refused in the vocabulary itself, so this only arises between two
  packages providing the same trait.
- **Structure is `pkgspec`'s to check, vocabulary is lint's.** The parser
  refuses an export that does not parse, calls a function that does not
  exist, or names a param its own package does not declare — that last
  one because the alternative is an export that renders empty, is dropped
  for being empty, and surfaces as a missing key three packages away from
  the typo.

**Rejected: exports as plain strings with no template.** The values that
matter are computed from the deployment's posture, not copied from a
param; a plain string would have forced the concatenation this decision
exists to prevent.

**Rejected: giving exports access to the whole desired state.** An export
is evaluated before the Routes it describes exist. The only honest
questions it can ask are the ones `keystone/ingressorigin` answers from
the declared posture, and a wider context would invite ones it cannot.

### A3. The shipped catalog is lint-clean, and CI proves it

`keystone/pkgtool` runs the full contract test over every directory in
`packages/`, with the whole shipped catalog available to satisfy
`requires` — plus two catalog-wide properties no single package can
check: every non-structural `requires` has a provider somewhere in the
catalog, and every `exports` key is one its trait actually carries.

Lint has been the package contract test since ADR-W010 A5, and nothing
ran it over the packages this repository ships. F31 is the recorded
consequence: `ingress-tailnet` shipped `classifications: [admin]`, a value
that does not exist, and lint called the package clean because the rule
was written after the package. A rule that the shipped catalog does not
have to satisfy is a rule that decays.

This is also the check a NEW package meets first: drop a directory into
`packages/`, run the test, and the answer names what is wrong — before an
install, before a plan, before an operator.

### A4. A param may declare an `example`, and lint uses it

`example` is a syntactically valid value for a param, used by lint's
dry-run expansion and available to the install form as a placeholder.

Lint auto-fills a required param with `lint-<name>`, which is a fine
stand-in for a domain or an id and nonsense for anything with a shape. The
four ingress packages ask for a listen address, got `lint-listen-addr`,
and failed resource validation on a value no operator would ever have
typed — so the gate in A3 could not have been turned on at all without
either this or a filler table maintained outside the packages. The author
is the only person who knows the shape, so the manifest is where they say
it: once, rather than as a `-set` flag every person who lints the package
has to rediscover.

Never a real value. A param carrying secret material is refused by lint
outright (ADR-W021 A2), and an example is written into the repository.

### A5. `wild pkg new` scaffolds a package that lints green, and proves it on the spot

Everything above makes a package CHECKABLE. None of it helps with the
blank file, and an author's first act was to copy an existing package and
delete the parts they did not recognise — which is how a scaffold gets
built anyway, once per person and badly. The forgotten digest pin is a
validation error they will see; the `auth: none` inherited from the wrong
donor package is a policy violation they will see later; the `container/`
prefix in env is six shipped occurrences of a mistake that could never
have started.

So `wild pkg new <name> -shape <shape>` writes a package with the idioms
already in place, then **lints what it just wrote and prints the result**,
against the catalog it found alongside. A scaffold that merely claims to
be correct is a claim the author cannot check in their first minute; one
checked in front of them is a starting point they can trust, and the
command they watched it run is the one they will run again.

Three shapes, ordered by how much work falls on the service's side:

- **`service`** — Wildstone runs the app: Volume, Container, Route,
  OidcClient, already wired to whichever package provides `identity:oidc`.
- **`sidecar`** — the app runs elsewhere and cannot call a webhook, so an
  adapter container watches it and publishes on its behalf.
- **`config`** — the app runs elsewhere and can already call a webhook, so
  Wildstone renders one managed config file and nothing else. This is the
  cheapest integration there is and the one the menu argues for: no
  container, no adapter, no code on either side.

Binding consequences:

- **Every shape lints green as written**, pinned by
  `TestScaffoldLintsGreen`. This is not a nicety. A scaffold whose first
  lint fails teaches the reader that failing output is normal, which is
  the opposite of what a contract test is for.
- **Every shape leaves TODOs**, and `wild pkg new` counts them. Correct
  and unfinished: a file that lints green while saying nothing about what
  is still the author's decision is one they will ship as-is, placeholder
  image digest and all.
- **The image placeholder is all-zeroes.** A digest pin is mandatory and
  there is no real digest to know at scaffold time, so the choice is
  between a scaffold that lints and a scaffold with an honest blank.
  All-zeroes is both: valid syntax, so the first lint is about the
  author's decisions rather than the placeholder — and no registry will
  ever serve it, so it cannot reach a deployment pretending to be pinned.
- **The shape is not a manifest field.** It is a choice about what to
  write, made once; the file it produces is an ordinary package with
  nothing recording which scaffold it came from. Declaring the posture in
  metadata stays with Part B item 1.
- **It refuses to overwrite.** A scaffold is for a package that does not
  exist yet, and the alternative is a command that can silently discard an
  afternoon's work because someone re-ran it with the wrong name.

**Rejected: scaffolding from a copy of a shipped package.** The shipped
packages are correct for their own service and carry decisions that do
not transfer — a classification, an auth mode, a subdomain convention —
and a copy inherits all of them silently. A scaffold that carries TODOs
where a decision belongs is the difference.

### A6. A URL is structure, not a value: `redact` shelters http(s) URLs

Found while building A5's sidecar shape, and it ran in both directions at
once. `redact`'s base64 shape rule scans runs of `[A-Za-z0-9+/_-]{24,}`,
and `/` is in that class — so a URL path of ordinary short segments reads
as one long token run:

```
https://wildstone.tailnet.ts.net/api/v1/household/events  → judged secret
https://wildstone.tailnet.ts.net/household/events         → fine
```

The policy engine's `secret-values-forbidden` rule therefore **refused** a
sidecar carrying the household intake URL in its env — a package that
cannot be planned at all, told only that its "value is secret-shaped",
which names nothing the author can act on. And the redactor wrote
`[REDACTED]` over the same string, so the plan step that would have
explained where the service posts was unreadable. That second half is
F27's failure exactly: a plan a human cannot read is a plan that cannot
be reviewed. No shipped package hit either, because their env URLs carry
no path; the first one to need a real path would have.

An http(s) URL is now sheltered, the same mechanism digest pins (ADR-W004
A1) and `Kind/name` references (F27, F36) already use. The judgement is
per-part, not per-URL:

- **A URL is sheltered only when NO part of it is token-shaped.** The
  match is split on `/?#&=:@.` — chosen because none of those characters
  appears inside base64url or hex material, so the split can never break
  a token into pieces small enough to hide. A token in a path segment, a
  query value, or a fragment still trips on its own and the whole URL
  falls through to the value rules.
- **Userinfo is refused outright**, before shape is considered.
  `https://admin:…@host/path` is the textbook way a credential reaches a
  config value, and it is what this rule is for.
- **The shelter is stricter than `SecretShaped`'s own bar**, ignoring its
  digit requirement, so sheltering can never make `String()` less
  aggressive than it was before the shelter existed.
- **Sheltered URLs carry their own sentinel**, distinct from the
  secret-reference one, so `kvSecretRegexp` still redacts a URL sitting
  under a credential-shaped key. Sharing a marker would have exempted
  `WEBHOOK_TOKEN: https://hooks.example.com/…` from the one rule that
  catches a credential by its KEY rather than its shape — the regression
  the tests caught while this was being written.
- **The anchor is the scheme.** `ftp://` and everything else are judged
  exactly as before; widening the set would be a decision about a value
  class nobody has needed, made by accident.

Two residual gaps, stated rather than papered over. A secret containing a
raw `/` that also breaks into pieces all shorter than 24 characters would
pass — that is a deliberately obfuscated secret, not the pasted literal
these rules exist for. And a short credential in a URL
(`https://admin:hunter2@host/…`) is not caught by shape and never was.
Both are covered where it matters by the rules that read the KEY: the
policy engine requires a credential-shaped key to hold a `secret/<name>`
reference and nothing else, and `kvSecretRegexp` redacts such a value
whatever its shape.

**Rejected: exempting anything that parses as a URL.** That is the
version of this change that silently stops catching presigned URLs,
webhook secrets and userinfo credentials — every case in
`TestURLsCarryingCredentialsAreStillCaught`.

**Rejected: sheltering in `SecretShaped` only.** It would have fixed the
refusal and left the plan unreadable, which is half a fix and the wrong
half: the two are supposed to agree about what counts as a secret.

---

# Part B — The rest of the layer (build order, not decided here)

A1–A4 are the half that had to come first: while `.Resolved` was
hardcoded, every other improvement would have been decoration on a seam
that still required a Go patch to cross. A5 (the scaffold) landed next,
ahead of the manifest metadata it was originally sequenced behind — the
shape turned out not to need a manifest field to be useful, and the
scaffold is what puts A1–A4 in front of an author on their first minute.

What remains, in intended order; each gets its own decision when it is
scheduled.

1. **Human metadata on the manifest** — `displayName`, `description`,
   `docs`, `maintainer`, and an explicit `integration.shape` (does this
   package RUN the service, adapt one with a sidecar, or only configure
   one that already exists). The Console can render nothing but a slug
   today, and the three shim shapes are declared in `publishes` while the
   package's overall posture is not declared anywhere. A5's scaffolds are
   where the field would be filled in.
2. **`packages/AUTHORING.md`** — the page a first-time author reads before
   running `wild pkg new`. The knowledge is currently spread across
   ADR-W005, ADR-W024, `packages/README.md`, the scaffold's own comments,
   and `keystone/capability`.
3. **Catalog-wide collision checks** — two packages claiming the same
   capability and profile, or the same Route label, are catalog defects
   that only a catalog-wide view can see. A3 is where they land.
4. **A Catalog holds packages as DECLARED, not as installed.** An export
   renders over the requesting package's params overlaid by the
   provider's declared *defaults* (`exportParams`), because a
   `capability.Catalog` is built from package specs and a default is the
   only provider-side value in reach. So a provider installed with a
   required or overridden param — the common case: `public_domain` is
   required and undefaulted — contributes nothing, and the export falls
   back to the consumer's own value for that name. Two deployments where
   those agree, which is every shipped one, cannot tell the difference;
   one where they do not gets an issuer derived from the consumer's
   domain.

   This is not new — the hardcoded lookup A2 replaced did exactly the
   same thing, `providerParams` returning defaults with the consumer's
   value as the fallback — and A2 preserved it deliberately rather than
   changing behaviour inside a refactor. Raised by automated review on
   this PR and confirmed real.

   The fix is to give the Catalog the params each enabled capability was
   actually installed with. They exist: `plan.CapabilityOrigin` records
   package, version and params on every capability proposal (A2 of
   ADR-W021). What is missing is a way to ask for them — the origin lives
   inside the stored plan's JSON, with no index by capability — so this
   needs a lookup ("the newest applied plan whose origin names this
   capability"), a decision about capabilities with no origin at all
   (installed before ADR-W021, or by hand), and a `Catalog` that carries
   configured params alongside declared ones. That is a change to what a
   Catalog IS, and it belongs in its own review rather than inside the
   change that made exports data.

Nothing here changes the trust boundary. A package is still data, still
never executed by the toolchain, and every install is still a plan a human
approves (ADR-W004).
