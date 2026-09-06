# packages/

Capability packages (data, not code) and the package build/lint/sign
toolchain. A package defines a profile of a capability: templates that expand
into typed resources, dependency traits, policies, lifecycle hooks (run as
containers with declared mounts — never host scripts), and the `agentTools`
declaration (ADR-W001). See `docs/decisions/ADR-W005-package-format-capability-engine.md`
for the binding format spec.

Launch packages are transcriptions of proven configs from the reference
deployment — see `testing/fixtures/`.

## Adding a service: start here

```sh
wild pkg new                          # the three shapes, and which one you want
wild pkg new my-app -shape config     # writes it, then lints it in front of you
```

Pick the shape by what you already run:

| shape | when | what you get |
| --- | --- | --- |
| `service` | Wildstone installs and runs the app | Volume, Container, Route, OidcClient — SSO already wired |
| `sidecar` | the app runs elsewhere and *cannot* call a webhook | an adapter container, handed its intake credential |
| `config` | the app runs elsewhere and *can* call a webhook | one managed config file, no container, no code |

**Try `config` first.** Most services can already POST to a URL under
some name — "webhook", "notification target", "REST command" — and that
path needs no adapter and no work on the service's side at all.
`home-automation/` is the worked example.

Every shape lints clean as written, with TODO markers everywhere a
decision is yours. `wild pkg new` counts them and prints the exact
commands to carry on with.

## Traits: how packages find each other (ADR-W025)

A package does not name another package. It names a **trait** — a
capability some other package advertises — and receives values from
whichever package provides it:

```yaml
requires:
  - { trait: identity, mode: oidc }   # gives you .Resolved.IssuerURL, ...
  - { trait: ingress, mode: public }  # structural: the system satisfies it
  - { trait: storage, class: bulk }   # structural: declare your Volumes
```

The vocabulary is closed and lives in `keystone/trait`. List it with:

```sh
wild pkg traits      # names only
wild pkg traits -v   # with what each trait and .Resolved key means
```

Lint refuses a trait, mode, or class that is not in it, and names the ones
that are — so `trait: identiy` is a message about a typo rather than an
"unsatisfied requirement" during someone else's install.

### Providing values to other packages: `exports`

If your package is the one others depend on, say what it hands them. Each
key must be one the trait mode declares (`wild pkg traits` lists them):

```yaml
provides:
  - trait: identity
    mode: oidc
    exports:
      IssuerURL: '{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}'
```

Four things to know:

- The template renders over **your own** params, plus `origin` and
  `sessionDomain`.
- **Use `origin`, never string concatenation.** It answers where a host is
  actually *served*, which under the tailnet posture is the node's one
  certified name plus a port — not the host you declared (ADR-W019 A10).
  An OIDC issuer is compared byte for byte against what the browser saw.
- **Wrap it in `with`.** An export that renders empty is not published, so
  a consumer fails on a missing key instead of receiving `https://auth.`
  and looping at a login screen.
- `OidcSecretRef`, `PublisherSecretRef` and `TailnetHost` are yours
  already — every package gets them — and an export cannot override one.

### Before you open a PR

```sh
wild pkg lint packages/<your-package>            # your package alone
cd keystone && go test ./pkgtool/ ./capability/  # the whole catalog, incl. yours
```

`TestEveryShippedPackageLintsClean` lints every directory under
`packages/` with the rest of the catalog available, and two companion
tests check what one package cannot see on its own: that everything you
`require` has a provider here, and that everything you `export` is a key
its trait carries.

If a required param needs a value with a *shape* — a listen address, a
URL — declare an `example` for it. Lint invents `lint-<name>` otherwise,
which is fine for a domain and nonsense for an IP:

```yaml
params:
  - { name: listen_addr, required: true, example: "100.64.0.1" }
```

## The two packages (Spike S3)

- **`identity/`** — LLDAP (directory) + Authelia (SSO/OIDC provider),
  profile `authelia-lldap`. Provides trait `identity:oidc`; any other
  package can `require: [{trait: identity, mode: oidc}]` and receive
  `.Resolved.IssuerURL` / `.Resolved.OidcSecretRef` in its own templates.
- **`photos-immich/`** — Immich server + machine-learning workers, a
  dedicated postgres + redis (proposal §5.1's "dedicated-per-capability
  infra" posture), an upload volume, a public Route, and an `OidcClient`
  resource that registers Immich with whatever package provides
  `identity:oidc`.

Image digests in both packages are **real**, resolved from the public
registries (ghcr.io / Docker Hub) at authoring time via the registry's
anonymous-pull token + manifest-digest endpoints — not placeholders. They
will need periodic refreshing as upstream images are rebuilt, which
`wild pkg refresh-digests` does.

**Refreshing is not always safe one image at a time.** A package's images
can be a matched set: `photos-immich` pairs a postgres image against an
`immich-server` that supports only certain vector extensions, and a skew
between the two fails at container startup ("No vector extension found"),
not at refresh.

Declare those pins as an `imageSet` and `refresh-digests` refuses to move
one without the others:

```yaml
imageSets:
  - name: immich
    images:
      - ghcr.io/immich-app/immich-server
      - ghcr.io/immich-app/postgres
    reason: >-
      immich-server accepts only the vectorchord and pgvector extensions;
      the postgres image supplies one.
```

A set moves as a unit or not at all — if one member is already current, or
fails to resolve, the others are held back and the command exits non-zero
naming the set. This cannot make a combination *valid*; nothing here pulls
an image, so nothing here knows what one contains. It only stops the tool
from creating a mismatch the author never chose.

## The adapter package (ADR-W024 H5)

`home-automation/` is unlike the rest: it runs nothing. A household with
home automation already has a Home Assistant, so instead of adopting it
the package templates one managed file that configures Home Assistant to
publish household events into Wildstone's intake — the "config" shim
shape (ADR-W024 A10). It is the model for any third-party service that
can already call a webhook.

Two things it demonstrates that a normal package does not:

- **`publishes`**, which declares the event types the capability emits
  and earns it a provided `<capability>-publisher` secret. `wild
  household publisher home-automation -node <node>` mints the token and
  places it on the node; the rendered config references it, and the node
  resolves the reference at write time. Enabling the capability before
  minting fails loudly on the missing provided secret, which is the
  right way round.
- **`externalConfig`**, which declares that the rendered file is read by
  something Wildstone does not run. Without it the file trips the
  standing invariant that every rendered config is mounted into a
  container of its own package — the check that exists because Authelia
  once shipped a correct `configuration.yml` on disk that its container
  never read. A declaration names the consumer and the reason; a stale
  one (naming a file the package does not render) fails just as loudly,
  for the same reason an unused policy exemption fails startup.
- **Double templating.** The file is rendered by Wildstone's Go
  templates once and by Home Assistant's Jinja forever after, and both
  spell an action `{{ ... }}`. Every Jinja expression is wrapped in a Go
  raw string — ``{{`...`}}`` — so it survives expansion literally.
  Contiguous Jinja goes in one wrapper rather than one per expression.

## Appearing on the dashboard (F55, F87)

A package does **not** declare how it looks on the Homepage dashboard.
Everything the dashboard shows is derived from the Route the package
already declares, by `keystone/reconcile/homepageservices.go`:

| What you see | Where it comes from |
| --- | --- |
| The link | `Route.host`, resolved to the address a browser actually uses |
| The group (Household / Admin) | `Route.classification` — `admin_tailscale` is Admin |
| Status dot | `Route.upstream`, dialled over the Wildstone Docker network |
| Name, icon, description | The `homepageApps` table, keyed by the upstream container name |

So integrating a new service is: declare a Route (which you must do
anyway to be reachable), then add **one row** to `homepageApps`:

```go
"paperless": {"Documents", "paperless-ngx", "Scanned paperwork you can search"},
//  ^ container   ^ title    ^ dashboard-icons slug   ^ one line, for the family
```

An app with no row still renders — a titled name from the Route, a
generic icon, and the capability as its description. Nothing breaks; it
just looks unbranded, which is the honest signal that nobody has
described it yet.

Two things deliberately **not** package fields:

- **Icon and description.** A package declaring them restates what its
  own Route already implies — the "declaring half of a relationship"
  trap ADR-W021 A6 rejects for the href itself. One table means one
  place to look and one place to fix.
- **The status probe target.** It is always the container, never the
  `https://` link: the household CA is not trusted inside the Homepage
  container (so every dot would be red under `tls: internal`), and a
  forward-auth Route answers a login redirect that says the proxy is up
  and nothing about the app.

Per-service **widgets** (Jellyfin's now-playing, Immich's photo count)
are not wired yet, and the blocker is not the renderer: each needs the
app's API key, secrets are write-only (ADR-W012 W2), and only a
materializing write resolves a `secret/` reference — so `widgets.yaml`
would need a materialize flag that depends on which apps are installed.
Tracked as F87.

## Container-to-container addressing

Containers on the Wildstone network reach each other by container-name
DNS, so an env value naming another container is just its name:

```yaml
env:
  DB_HOSTNAME: immich-postgres           # right
  DB_HOSTNAME: "container/immich-postgres"  # WRONG — lint refuses it
```

The `container/<name>:<port>` form is real syntax, but only in
`Route.upstream`, where the reconciler resolves it. Nothing resolves it in
env: the agent substitutes `secret/` and passes everything else through
byte for byte, so the process receives the string as written. Two shipped
packages carried this in six places and neither could have started. Lint
now refuses it (`container-env-ref`).

## The go/no-go gate (ADR-W005 A4)

`keystone/vertical`'s `TestGoNoGoIdentityPhotosVertical` is the CI-runnable
equivalent of the **live VM demo** (an owner deploying real Immich behind a
real Authelia and completing a browser OIDC login) — that live run is the
owner-run `testing/e2e` path; this test is what proves the same wiring in
CI, with Docker and the managed filesystem faked but a real gateway, real
mTLS agent session, and the real Capability Engine / Reconciler in the
loop. It loads both packages, expands them, writes the result to a real
Statefile, reconciles it, and asserts the result matches
`testing/fixtures/immich/authelia-oidc-client.yml`'s shape with zero
hand-editing between package and applied state. Run it with:

```sh
cd keystone && go test -run TestGoNoGoIdentityPhotosVertical -v ./vertical/
```

and read the `GATE VERDICT: GO`/`NO-GO` line.
