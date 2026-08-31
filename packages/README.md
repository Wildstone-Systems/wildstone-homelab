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
