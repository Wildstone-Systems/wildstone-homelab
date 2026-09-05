# packages/

Capability packages (data, not code) and the package build/lint/sign
toolchain. A package defines a profile of a capability: templates that expand
into typed resources, dependency traits, policies, lifecycle hooks (run as
containers with declared mounts — never host scripts), and the `agentTools`
declaration (ADR-W001). See `docs/decisions/ADR-W005-package-format-capability-engine.md`
for the binding format spec.

Launch packages are transcriptions of proven configs from the reference
deployment — see `testing/fixtures/`.

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

## The adapter package (ADR-W022 H5)

`home-automation/` is unlike the rest: it runs nothing. A household with
home automation already has a Home Assistant, so instead of adopting it
the package templates one managed file that configures Home Assistant to
publish household events into Wildstone's intake — the "config" shim
shape (ADR-W022 A10). It is the model for any third-party service that
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
