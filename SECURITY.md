# Security policy

Wildstone is alpha software that runs privileged agents on your hosts.
Treat every release accordingly: expect breaking changes between
releases, and keep backups you have tested.

## Reporting a vulnerability

Please report vulnerabilities **privately** via GitHub's security
advisories: *Security → Report a vulnerability* on this repository.
Do not open a public issue for anything exploitable.

You can expect an acknowledgement within a week. This is a solo-
maintained project — there is no bounty program and no SLA, but
security reports are taken ahead of all other work: per the project's
own rules, a failed security property is a release blocker.

## Scope

Especially interested in reports that break a **structural claim** the
docs make, since those are the product's core promises:

- Executing arbitrary code on a node through the Wardstone protocol
  (there is deliberately no Exec operation).
- Any write to host state that does not pass the plan → human-approve
  pipeline.
- Reading a secret value back out of the system after `wild secret set`
  (the protocol has no read-back operation).
- A secret-shaped value escaping the API's redacting encoder, or
  reaching the Butler's model unredacted.
- The Butler acting beyond its ceiling: approving, applying, or
  mutating anything other than creating a `proposed` plan record —
  including via prompt injection through tool results (see the pinned
  red-team suite in `keystone/butler/`).
- Authentication or RBAC bypass on the public API.

See `docs/threat-model.md` for the full model, including what is
explicitly out of scope for the alpha (e.g. a malicious admin, Docker
escape, DoS beyond rate limiting).

## Supported versions

Only the latest tagged release and the tip of `main` are supported.
Earlier tags receive no fixes.
