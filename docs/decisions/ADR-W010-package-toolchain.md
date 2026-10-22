# ADR-W010: Capability engine to production grade — structural traits, package toolchain

- **Status:** accepted
- **Relates to:** ADR-W005 (format v1 + engine, unchanged as the base
  contract), implementation scope E5
- **Audience:** decision record AND implementation spec for E5.

---

# Part A — Decisions (binding)

### A1. Structural traits are engine semantics, not catalog entries

ADR-W005 A2 named `ingress:*` and `storage:*` as satisfied "by the
reverse-proxy/Route mechanism" and "a declared Volume" — always-available
system mechanisms. The S3 engine checked every requirement uniformly
against the catalog, so the gate test carried a synthetic
"system-capabilities" package (a flagged scaffold). E5 makes the ADR's
prose real: the engine satisfies `ingress` and `storage` requirements
structurally — a CLOSED list in code (the closed-registry philosophy),
they contribute nothing to `.Resolved`, and the synthetic catalog entry
is deleted. Every other trait still resolves through the catalog.

### A2. The package artifact and its signature

A distributable package is a deterministic tar.gz of the package
directory (sorted paths, zeroed timestamps/uids, no external metadata) so
its sha256 is reproducible from a checkout. It is signed with an ed25519
detached signature over `sha256(archive)` with a domain-separation prefix
(`wildstone-pkg-v1`, the ADR-W003 W1 pattern). Keys are plain ed25519
files: MVP has exactly one trusted publisher — the owner (D2: you-first;
the community-registry governance story is Phase 3). Verification happens
at install/fetch time; an unsigned or badly-signed archive never reaches
Expand.

### A3. The local registry is a directory served over HTTP

`<root>/index.json` lists `{name, version, sha256, path}` entries;
archives live beside it. Publishing = build archive + sign + rewrite the
index (the toolchain does this atomically). Serving = any static file
server; keystone ships a handler but the format works from `python -m
http.server` — the escape-hatch principle. No database, no API: the
registry is data, like packages themselves.

### A4. Versions and upgrade plans

Package versions are semver (already validated by pkgspec). Resolution
picks the highest version satisfying a caller constraint (exact, `^major`
at MVP — no full range algebra until something needs it). An upgrade is
NOT a special mechanism: resolve the new version, `Expand` it with the
same params, and the resulting tree goes through the normal
propose→policy→approve→reconcile pipeline — the plan diff IS the upgrade
plan (ADR-W004 A3: one plan artifact for everything).

### A5. Lint is the package contract test

`pkgtool.Lint` runs, without touching any host: parse + validate
(pkgspec), a dry-run Expand with declared param defaults (+ caller-
supplied fillers for required params), resource validation of everything
rendered, digest-pin enforcement on every image, and the E4 policy engine
over the rendered tree (a package that ships a policy violation fails
lint, not the user's install). Lint failures name the template index.

# Part B — Work packages (all implemented)

- **C1** — structural traits in the engine; the gate test's synthetic
  catalog package deleted.
- **C2** — `keystone/pkgtool`: Lint (A5), deterministic archive build,
  sign/verify (A2), registry index read/write + resolve (A3, A4);
  `wild pkg lint|keygen|publish|resolve` as the first real `wild`
  subcommands.
- **C3** — verified fetch over HTTP (any static server; digest +
  signature checked before extraction) and digest-refresh tooling
  (textual in-place rewrite preserving comments; OCI-registry resolver);
  `wild pkg fetch|refresh-digests`.
- **C4** — the upgrade test: resolve `^1` → verify → extract → Expand →
  diff against a Statefile holding vN — the plan IS the upgrade plan.

## Addendum: route auth vocabulary (found by C2's lint contract test)

Linting the shipped packages against the E4 policy engine surfaced a
modeling gap: both used `auth: none` on `family_public` routes for
surfaces that ARE authenticated (the Authelia portal itself; Immich's
own OIDC flow). The Route auth vocabulary gained `portal` (the IdP's own
login surface — public by necessity) and `app_oidc` (the app runs its
own OIDC flow against our IdP), both packages adopted them, and `none`
regained its honest meaning: truly unauthenticated, still forbidden on
public routes without an exemption (ADR-W009).
