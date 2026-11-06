# Synthetic Flux secret-custody oracle

These files specify Child A of #162. Every name, path, encrypted string and
recipient is synthetic. `secret.sops.yaml` is deliberately non-decryptable: it
tests the existing closed structural validator and safe-review boundary, not a
SOPS MAC or age identity.

- `versions.json` pins the implementation versions; floating versions fail the
  contract test.
- `privileged-inventory.json` specifies the one planned `secretDelivery`
  addition to the existing local inventory. Child B accepts only that closed
  opt-in shape and keeps legacy bootstrap behavior when it is absent.
- `secret.sops.yaml` contains fake encrypted-shaped strings and no plaintext
  credential or private key.
- `safe-review.json` is the complete permitted new Owner-review surface. It
  contains names, declared generation, digests and effects, never encrypted
  bytes, raw recipient, private path or identity material.

No file here authorizes private Git publication, decryption, bootstrap-key
creation, Kubernetes mutation, rotation, rollback or cleanup.
