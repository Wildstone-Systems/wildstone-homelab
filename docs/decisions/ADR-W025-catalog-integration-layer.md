# ADR-W025: The catalog integration layer — adding a service should be data, not a patch

- **Status:** proposed
- **Relates to:** ADR-W005 (package format, the Capability Engine,
  `.Resolved`), ADR-W010 (package toolchain, lint as the contract test,
  the closed-registry posture), ADR-W019 A10 (addressing under the
  tailnet posture), ADR-W021 (the catalog is Keystone's; `requires`
  resolves against what is enabled), ADR-W024 A10 (`publishes` and the
  three shim shapes)
- **Audience:** decision record AND implementation spec for the layer a
  package author writes against. Part A is binding. Part B is the build
  order for the rest of the layer, and is not decided here.

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

---

# Part B — The rest of the layer (build order, not decided here)

A1–A4 are the half that had to come first: while `.Resolved` was
hardcoded, every other improvement would have been decoration on a seam
that still required a Go patch to cross. What follows is the intended
order; each gets its own decision when it is scheduled.

1. **Human metadata on the manifest** — `displayName`, `description`,
   `docs`, `maintainer`, and an explicit `integration.shape` (does this
   package RUN the service, adapt one with a sidecar, or only configure
   one that already exists). The Console can render nothing but a slug
   today, and the three shim shapes are declared in `publishes` while the
   package's overall posture is not declared anywhere.
2. **`wild pkg new <name> -shape <...>`** — scaffold a package that lints
   green on the first run. The shape choice is the one from (1), and the
   `config` shape's scaffold is the important one: it is the path with no
   work on the service's side at all.
3. **`packages/AUTHORING.md`** — the five-step page. The knowledge is
   currently spread across ADR-W005, ADR-W024, `packages/README.md`, and
   comments in `keystone/capability`.
4. **Catalog-wide collision checks** — two packages claiming the same
   capability and profile, or the same Route label, are catalog defects
   that only a catalog-wide view can see. A3 is where they land.

Nothing here changes the trust boundary. A package is still data, still
never executed by the toolchain, and every install is still a plan a human
approves (ADR-W004).
