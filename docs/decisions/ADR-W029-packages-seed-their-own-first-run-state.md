# ADR-W029: Packages seed their own first-run state; no install needs a human step

- **Status:** accepted
- **Relates to:** ADR-W004 A4 (Wildstone never auto-deletes data),
  ADR-W005 (packages own their app's config), ADR-W021 (capability
  install), ADR-W010 (lint is the package contract test), ADR-W028 (a
  capability waiting is not a node failing)
- **Audience:** decision record.

---

# Part A — Decisions (binding)

### A1. A package may not require a human step to work

An install that reports success must BE a success. If a package needs
something done, it is done by Wildstone: derived from what the control
plane already knows, asked for up front as a param so the install form
collects it before anything is proposed, or seeded into the app's own
volume before it starts.

The one exception is a file whose reader is genuinely outside Wildstone
— the `home-automation` package renders config for a Home Assistant on
someone's own hardware — and it must be declared in `externalConfig`,
which already names the consumer and the reason.

`pkgtool` enforces this: rule `manual-step` fails a package whose
rendered content instructs a person, excusing only declared
externalConfig files.

### A2. `kind: SeedFile` writes into an app's own volume, once, before it starts

A ConfigFile is Wildstone's file: under the managed prefix, rewritten
every tick, reverted on drift. A SeedFile is the app's file: written
only if absent, never rewritten, never reverted, never deleted.

Guarantees, in order of how much they matter:

- **Write-if-absent, enforced with `O_EXCL` at the node.** Not
  stat-then-write: the kernel decides, so a retry, a concurrent seed and
  a household editing the file at the same moment cannot combine into a
  lost edit.
- **Confined to the volume, by the kernel.** Every path is resolved
  through an `os.Root` anchored at the volume, so no symlink at any
  component can redirect a write onto the host. Done by hand first, and
  wrongly: resolving only the parent and treating `ENOENT` as proof of
  confinement lets `escape -> /etc` with path `escape/newdir/file`
  through, because the missing `newdir` masks the symlink above it. That
  wrote outside the volume, as root, and reported success.
- **Confined to a volume the same capability owns**, enforced in the
  RECONCILER, because it is the only side that can be: the node's owner
  label says Wildstone created the volume, not which capability owns it.
  `pkgtool`'s `seed-volume` rule checks the same thing at lint time so an
  author learns before an install, but the reconciler's copy is the
  load-bearing one — a hand-written statefile never passes through lint.
- **Observed by existence, never by content.** Comparing content would
  report drift the moment somebody edited their own config, and
  "converged" would come to mean "nobody has touched their settings".
- **Unobservable is absent, never an error.** An agent one minor behind
  answers `INVALID` for an op it does not know. Treating that as an
  observation failure aborts the pass for the WHOLE NODE, so one
  SeedFile in desired state would stop every unrelated capability
  converging until the agent was upgraded — the exact blast radius
  ADR-W028 exists to remove. Absent instead keeps the node reconciling
  and lets the seed's own apply report the skew, halting that capability
  alone.
- **Never deleted.** An undesired seed observes as absent, so no Delete
  is ever diffed. Uninstalling a capability leaves its seeded config
  exactly where it is, for the same reason it leaves its volumes
  (ADR-W004 A4).
- **No secret references.** A seed is written once and owned by the app
  afterwards, so a secret in one could never be rotated. Refused at
  validation; use a ConfigFile for anything secret-bearing.

### A3. Derived values are derived once, centrally

`.Resolved.ProxyTrustCIDRs` is the address ranges an app behind the
reverse proxy should believe `X-Forwarded-For` from. It exists so that
no package guesses its own list — `home-assistant` shipped a hardcoded
one, and a second package would have shipped a different one.

---

# Part B — Why (non-binding)

## The bug this is written from

The `home-assistant` package rendered a correct reverse-proxy config,
mounted it read-only into its own container, and put the step that made
it take effect in a comment inside that file:

```
# Include from /config/configuration.yaml (once):
#   homeassistant:
#     packages: !include_dir_named /config/wildstone
```

Nobody reads a comment inside a file mounted inside a container. The
install reported success — every resource it owned had converged, 66
observed, 0 unhealthy — and the service answered `400 Bad Request` to
every request. The operator saw a green install and a broken link, with
nothing anywhere connecting the two.

That is the whole class: **a package may not address a person.** Every
piece of A1 follows from it.

## Why seeding, and not a mounted file

Home Assistant reads `http:` exactly once. On its first boot it migrates
the block out of `configuration.yaml` into its own settings store and
records the migration as done; from then on the YAML is ignored
permanently. A file mounted alongside the app after that boot has
already missed its turn, however correct it is. Ordering is the
mechanism, so ordering had to become expressible: SeedFile is ordered
with Volume, ahead of Container.

## Why the settings store, and not the seeded YAML

Seeding `http:` into `configuration.yaml` was tried first, and it worked
— for five minutes.

A migrated YAML block is staged as a **pending** config on trial. Unless
a person promotes it in the UI within `AUTO_REVERT_DELAY` (five
minutes), Home Assistant reverts to stable, restarts, and marks the
pending config `not_promoted` so it is never applied again. Measured on
the shipped image: `302` at boot, `400` and `pending.error:
not_promoted` six minutes later.

That failure is worse than the original. The original was broken
immediately and visibly. This one works at approval time and breaks
while the operator is doing something else, with no plan, no event and
no log line an operator would ever look at.

So the package seeds the settings store itself, with the value already
in `stable` and `pending` empty: no trial, no revert, nobody to promote
it. `configuration.yaml` deliberately carries no `http:` block, because
with the migration marked done a leftover one only raises a "your YAML
is being ignored" repair notice.

This couples the package to another project's on-disk store shape, which
is a real cost and the reason it is recorded here rather than left as a
clever line in a template. It is bounded: the image is digest-pinned,
and `TestSeededHomeAssistantTrustsTheProxy` boots that exact image and
asserts both that a proxied request succeeds and — the load-bearing part
— that `pending` is empty, which is what says there is no trial to lose.

That test is OPT-IN (`WILDSTONE_DOCKER_TESTS=1`), because it needs a
Docker daemon and pulls a large image, so it does not run on every push.
It is the check to run when the pinned digest is refreshed. Wiring it
into a scheduled job is worth doing and is not done here.

## What the seed set has to contain

Home Assistant creates its default configuration directory only when
`configuration.yaml` is missing, and returns early the moment it finds
one. Seeding that file therefore means seeding every sibling its own
`!include` lines name — `automations.yaml`, `scripts.yaml`,
`scenes.yaml` — or the first boot fails on a missing file.

`.HA_VERSION` is seeded for a subtler reason: its ABSENCE is read as
version `0.7.7`, which runs every migration since 2019 against a
directory created seconds ago. It is also why seed paths allow dot-files
and refuse only traversal — a rule that banned dot-led names would have
ruled out the case seeding exists for.

## What this does not solve

`ProxyTrustCIDRs` is the private ranges Docker allocates bridge networks
from, not the one subnet Wildstone's network actually occupies, because
nothing asks Docker what that subnet is. The practical trust boundary is
"any container on this host" — the same boundary the app already has,
since everything on that network can reach its port directly. Narrowing
it means teaching HostFacts about networks, and is worth doing when a
second node makes the difference real.
