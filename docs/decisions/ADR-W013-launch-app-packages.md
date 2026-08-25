# ADR-W013: Launch app packages — media (Jellyfin), documents (Paperless), photos backup hooks

- **Status:** accepted
- **Relates to:** ADR-W005/W010 (format + toolchain), ADR-W011
  (infrastructure packages, backups), ADR-W012 (secrets), implementation
  scope E7
- **Audience:** decision record AND implementation spec for E7.

---

# Part A — Decisions (binding)

### A1. Transcription from the reference deployment, dedicated infra

Both new packages transcribe the reference deployment's proven configs
(homelab-config `deployments/media-services/`), keeping the §5.1
dedicated-per-capability infra posture photos-immich set: each
capability's DB/cache/pipeline containers are its own, never shared, so
capabilities fail, back up, and upgrade independently. All images
digest-pinned via the E5 resolver at authoring time.

### A2. media (Jellyfin) — and the `app_ldap` auth value

- Jellyfin's login is its own, backed by the official LDAP-Auth plugin
  against LLDAP (the reference's working posture; its plugin XML is the
  fixture). That surface is authenticated but is neither Authelia
  forward-auth nor an OIDC client, so the closed Route auth vocabulary
  gains one value: **`app_ldap`** — the app runs its own LDAP-backed
  login; policy-equivalent to app_oidc (public-requires-auth flags only
  `none`). Closed-set extension, code-reviewed here, like portal/
  app_oidc before it.
- The package declares an `LdapGroup media_users` (the reference
  registry's required group) and the jellyfin container with config +
  cache volumes. The media LIBRARY is a read-only host-path mount under
  a managed prefix (param `media_path`, default `/srv/wildstone/media`)
  — the agent's mount guard requires managed-prefix host paths, which
  is why the reference's `/srv/data/media` shape is not carried over.
- The LDAP plugin's XML (install + bind password) is app-side state
  inside the config volume; deploying it stays a documented manual step
  at MVP (the fixture is the guide). A managed-file materialization of
  plugin config is future work, noted, not silently skipped.
- Hardware transcode (`/dev/dri`) needs a `devices` field ContainerSpec
  does not have; deliberately out of E7 — a schema/protocol addition to
  make when a real deployment demands it, not before.

### A3. documents (Paperless-ngx)

Full reference pipeline: paperless webserver + dedicated redis broker,
postgres 16, gotenberg (with the reference's hardening flags via
ContainerSpec.command) and tika. OIDC via Authelia exactly as the
reference does it (django-allauth provider config), so the route is
`app_oidc` with an OidcClient registration. Wardstone materializes
WHOLE env values (ADR-W012 A4), and allauth's provider config is one
JSON blob with the client secret inside — so the ENTIRE
`PAPERLESS_SOCIALACCOUNT_PROVIDERS` value is a secret
(`secret/paperless-oauth-providers`, seeded via `wild secret set` with
the reference-shaped JSON), and Paperless's own SECRET_KEY and DB
password are ordinary `secret/` references.

### A4. Backup hooks: one BackupPlan per app capability

Each app package declares its BackupPlan (ADR-W011 A4) plus a dedicated
repository volume and a per-capability `secret/restic-<capability>`
password:

- photos: immich-upload + immich-pgdata, daily, 7d/4w/6m.
- documents: paperless-data + paperless-media + paperless-pgdata,
  daily, 7d/4w/6m.
- media: jellyfin-config only, weekly, 4w — the media LIBRARY is bulk
  replaceable content on the storage layer, deliberately outside restic
  (rip it again vs. back it up); the config volume is what hurts to
  lose.

### A5. The e2e flow is the harness's, lint is the gate here

The scope's per-app e2e (install → OIDC login probe → backup →
restore-alongside) needs live VMs: it belongs to the QEMU
workflow-dispatch harness and the E13 exit drill. In-repo, the merge
gate is the packages/ lint walk (every template expands, validates, and
passes policy) — which now also covers BackupPlan and app_ldap.

# Part B — Work packages

- **L1** — `app_ldap` vocabulary; both packages; photos-immich gains
  its A4 backup hook; lint-all green.
- **L2** (deferred to the e2e epic) — harness scenarios per A5.
