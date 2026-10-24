# Package deployment capabilities contract

- **Issue:** [#179](https://github.com/Wildstone-Systems/wildstone-homelab/issues/179) (W050 gate)
- **Replaces:** the `availability` array drafted in [#178](https://github.com/Wildstone-Systems/wildstone-homelab/pull/178), which stays a draft until this is accepted
- **Evidence base:** `main` after #177 (`97628db`), 2026-10-24
- **Status:** accepted 2026-10-24 by the owner, with the §9 recommendations taken as decisions
- **Amends:** ADR-W005 (A5, package format) and ADR-W050 §2.3 (placement)

This document defines what a package may **say about itself** when it runs on more than one machine, and what the **household chooses**. It keeps those two apart.

No runtime behaviour is authorized here. The Docker backend keeps running every package on its one named node. Until a multi-node backend consumes these fields (W050 slices 11, 14 and 15), they are checked by lint and nothing reads them.

## 1. Why the array in #178 is not enough

#178 put one list on each package, `availability: [replicated, failover]`, with the default first. The review in #179 found five problems:

1. **It mixes what the package allows with what the household picks.** "Default first" means the package author picks `replicated` for `tools-it-tools`. Running several copies is a household decision, bounded by what the package allows.
2. **It treats every device the same.** A GPU app can move to another machine with a GPU. A Zigbee app needs *that* USB stick. #178's lint forced both to `[pinned]`.
3. **It checks default settings only.** Frigate with a Coral set and `failover` declared would pass lint.
4. **`offload` is not a mode.** "Has workers" describes one part of a package (Immich's machine-learning container), not the whole package.
5. **"No volume" was treated as close to "stateless".** No volume doesn't prove an app has no state. A read-write mount is a starting restriction on running copies, not proof that copies are impossible.

## 2. The model in one table

There are three separate questions, and each has its own owner.

| Question | Who answers | Where it lives | Default when unsaid |
|---|---|---|---|
| **What it needs from a machine:** a specific device, a type of device, the host's network, a shared folder | Lint **works it out** from the rendered tree; the package labels its device params | Rendered resources + `devices:` in `package.yaml` | A device not labelled `class` or `any-node` counts as **a specific device** |
| **What it can safely do:** run as several copies, restart on another machine, act as a worker | The **package author** declares it; lint refuses claims the tree contradicts | `deployment:` in `package.yaml` | **One copy, does not move, main** |
| **What the household picks** within that | The **owner**, through the Console or CLI, as a Plan | Placement policy in Keystone, not the package | **Stay on the chosen machine, one copy**: exactly today's behaviour |

The rule that ties them together: **the household's policy can never exceed what the package declares, and the package's declaration can never exceed what its needs allow.** Lint enforces the second half; Keystone's policy check enforces the first.

## 3. Package fields

Two new optional top-level fields replace `availability`.

### 3.1 `devices`: what each device is

Every device a template can map must be labelled, whether it comes from a param or is written literally:

```yaml
devices:
  - { param: usb_device, kind: identity }           # that exact stick
  - { param: gpu_device, kind: class, class: gpu }  # any machine with a GPU
  - { path: /dev/net/tun, kind: any-node }          # every Linux machine has one
```

| `kind` | Meaning | Effect on moving |
|---|---|---|
| `identity` | This exact physical device | The component can't move while the device is mapped |
| `class` | Any device of this type; `class` is a node label (`gpu`, `coral`) | May move to a machine with that label |
| `any-node` | A kernel-provided node on every machine | No effect |

### 3.2 `deployment`: what each component can safely do

A **component** is one Container the package renders. Components not listed get the defaults.

```yaml
deployment:
  components:
    - name: "immich-server"
      relocate: true           # default false
    - name: "immich-machine-learning"
      role: worker             # default main
      copies: many             # default one
      relocate: true
```

| Field | Values | Default | Meaning |
|---|---|---|---|
| `name` | Container name **after rendering** (template expressions allowed, rendered with the same params) | required | Which component this is about |
| `copies` | `one`, `many` | `one` | `many` = several copies may run at once without corrupting anything |
| `relocate` | `true`, `false` | `false` | `true` = the component may stop here and start on another eligible machine, given its storage moves with it |
| `role` | `main`, `worker` | `main` | A `worker` does extra work for a `main` component in the same package and may run elsewhere |

Jobs are not components. They run once, where Keystone sends them, and they don't take these fields.

**Why the defaults are conservative.** A package that says nothing behaves exactly as it does today: one copy, on the node the household chose. Leaving out a field can never give an app more freedom than its author meant.

## 4. Lint rules

Lint renders the package in **every variant that changes a need**, not only with defaults (§5), and applies these rules to each variant:

1. **Every mapped device is labelled.** A device source that matches no `devices` entry is an error. Lint fills each device param with a unique placeholder (`/dev/lint-<param>`), so it can tell which param produced which mapping.
2. **`relocate: true` is refused** on a component that maps an `identity` device in any variant. It is allowed for `class` and `any-node` devices.
3. **`relocate: true` with a host folder** is allowed only when the folder comes from a param the household fills in (`source: directory`), so that they can point it at their NAS/NFS share (W050 D4). Lint tells the two apart the same way it does for devices: it fills each `source: directory` param with a placeholder. A fixed path written in the template is an error, read-only or not, because it is only on one machine's disk. Jellyfin reads its library read-only, and moved without it, it would show an empty library. Keystone's own managed config under `/etc/wildstone` is the exception: Keystone renders it and can deliver it to any machine. Whether a given install may actually move is the household-policy check in §8: every such folder must be on the share.
4. **`relocate: true` with host networking** is a **warning, not an error.** Host networking needs a machine, not necessarily *this* machine, but LAN discovery and fixed ports often assume one. The author confirms by setting `relocate: true` on purpose, and the warning stays in lint output.
5. **`copies: many` is refused** on a component with any read-write mount, volume or host folder. This is a starting restriction. A later, reviewed exception (for example, a database the package proves is shared-safe) is out of scope.
6. **`copies: many` is refused with host networking or an `identity` device**, because two copies would fight over one port or one stick.
7. **`role: worker` needs a `main`** in the same package. A worker's `copies` and `relocate` are its own.
8. **Every `deployment.components[].name` must match** a rendered Container in at least one variant. A misspelt name is an error, not a silent default.
9. **No `deployment` and no `devices` is valid.** Everything gets the defaults. Rule 1 still applies: any mapped device must be labelled.
10. **A free-text param may not change needs** (§9 decision 4). If rendering the param empty and with a placeholder changes a component's devices, host networking or host folders, the param must become an `enum:` or a labelled device param.

The vocabulary (`identity`, `class`, `any-node`, `one`, `many`, `main`, `worker`) is checked in lint, as `accessGroups` is. See §7 for the parser.

## 5. Which variants lint renders

Rendering every combination of every param would be too many. Lint renders a bounded set:

1. **Defaults**, as today.
2. **Every device param filled at once**, with the placeholders from rule 1.
3. **Each value of every `enum:` param**, one at a time, with the other params at their defaults. This catches Home Assistant's `bluetooth: host-bluez`, which adds host networking and D-Bus.

That covers every case #179 names: Frigate with and without GPU or Coral, host networking switched on by a param, and tun. A free-text param that changes needs is refused (rule 10).

## 6. Worked examples

**Stateless app, `tools-it-tools`.** No volume and no device. The author declares that copies are safe:

```yaml
deployment:
  components:
    - { name: it-tools, copies: many, relocate: true }
```

The household may later pick 2 copies. Until they do, it runs one copy where it was installed.

**SQLite singleton, `recipes-mealie`.** It has a read-write data volume. The author may allow moving but not copies:

```yaml
deployment:
  components:
    - { name: mealie, relocate: true }
```

`copies: many` would fail rule 5.

**Device-pinned app, `zigbee-zigbee2mqtt`.** With a USB coordinator, it needs one specific stick:

```yaml
devices:
  - { param: usb_device, kind: identity }
```

There is no `deployment` block, so the defaults apply: one copy, doesn't move. `relocate: true` would fail rule 2. With a network coordinator (`coordinator_url`) instead, no device is mapped and the app could move. That is the same param-dependent case as Frigate, and under §9 decision 2 it doesn't move either.

**Relocatable GPU app, `media-jellyfin`** (when it gains its iGPU param):

```yaml
devices:
  - { param: gpu_device, kind: class, class: gpu }
```

The GPU is a type of device, so on its own it would allow `relocate: true` on any machine labelled `gpu`. Jellyfin also reads its media folder from the host, but that folder comes from the `media_path` param (`source: directory`), so rule 3 allows `relocate: true`. The household gets the choice to let it move only if `media_path` is on their NAS/NFS share (§8). On a machine's own disk it stays put.

```yaml
deployment:
  components:
    - { name: jellyfin, relocate: true }
```

**Main plus workers, `photos-immich`:**

```yaml
deployment:
  components:
    - { name: immich-server, relocate: true }
    - { name: immich-machine-learning, role: worker, copies: many, relocate: true }
```

Postgres and Redis aren't listed, so they stay one copy and don't move until their author says otherwise.

**Frigate with optional devices:**

```yaml
devices:
  - { param: coral_device, kind: identity }
  - { param: gpu_device, kind: class, class: gpu }
deployment:
  components:
    - { name: frigate, relocate: true }
```

This **fails** rule 2 in the "every device filled" variant, because the Coral is a specific device. The honest declaration is to leave `relocate` out (the default). Under §9 decision 2 it stays that way: Frigate doesn't move.

## 7. Compatibility and versioning

- **The parser is strict.** `pkgspec.Parse` uses `KnownFields(true)`, so a Keystone that doesn't know `devices` or `deployment` **refuses to load** a package that has them. This is the real compatibility cost, not the vocabulary.
- **The plan:**
  1. Ship the parser support for both fields in one release. It is informational only, and lint rules land with it.
  2. Add the fields to catalog packages **in the next release or later**, so every supported Keystone can already parse them.
  3. Bump each changed package's patch version, as the version-lock test requires.
- **#178 is not merged, so nothing needs migrating.** Its useful parts carry over: the lint plumbing, the scaffold change, the README section and the per-package survey. The 64-package edit becomes `devices` labels on the 6 packages that map devices, plus `deployment` blocks only where an author is confident. Everything else keeps the defaults, which is what it does today.
- **Lint is opt-in during the first release.** Rules 1–10 run only for a package that declares `devices` or `deployment`. Enforcing rule 1 on every package before the catalog can carry the fields would fail the whole catalog. When the catalog adopts the fields, lint requires them of every package that maps a device.
- **No runtime reads these fields** until a multi-node backend slice does, and that slice must cite this contract.

## 8. Household policy (outside the package)

This section is recorded here so it stays separate. It is implemented in the multi-node slices, not now.

- **Policy per installed capability:**
  - `keepRunning`: restart on another machine if this one is lost. Offered only if every `main` component has `relocate: true` **and** every host folder it mounts resolves to the household's NAS/NFS share (a `NetworkMount`), not the machine's own disk.
  - `copies`: N. Offered only for components with `copies: many`.
  - `workers`: where workers may run. Offered only for `role: worker` components.
- **Defaults:** `keepRunning: off`, `copies: 1`, workers on the main component's machine. That is exactly today's behaviour.
- **Changing it is a Plan,** approved like any other change. Keystone's policy check refuses a policy that exceeds the package's declaration.
- **This document does not promise availability.** `keepRunning` only means "Keystone will try to restart it elsewhere". Whether that is safe depends on #180's storage and partition proof (single writer, detach and reattach).

## 9. Owner decisions (2026-10-24)

1. **Conservative default: kept.** A package that says nothing doesn't move. `relocate: true` is declared one package at a time.
2. **Param-dependent packages: leave the field out.** Frigate (Coral) and Zigbee (USB stick) declare no `relocate`, and there is no condition language. This can be revisited if it is missed.
3. **Host networking: a warning is enough** (rule 4). There is no second switch.
4. **Free-text params that change needs: refused.** Lint rule 10: if a free-text param changes a component's needs (a device, host networking or a host folder), it must become an `enum:` or a device param. Lint detects this by rendering the param empty and with a placeholder.
5. **Host folders: only on a share.** A component that mounts a host folder may move only if that folder is on the household's NAS/NFS share. Read-only folders are included, because a read-only library on the wrong machine is just as empty. Keystone-managed `/etc/wildstone` config is the exception. *D4, decided the same day:* a share is optional on any topology; without one, folders stay on the machine's own disk and those apps stay put. Rule 3 and §8 implement this.

## 10. Acceptance (from #179)

- [x] Schema and examples for a stateless app, a SQLite singleton, a device-pinned app, a relocatable GPU app, and a main plus workers (§3, §6).
- [x] Conservative defaults, supported-versus-selected semantics, and a parameter-aware lint strategy (§2, §4, §5, §8).
- [x] #178 reconciled before merge (§7).
- [x] #151, #158, #161, #167 and #171 aligned, with no runtime behaviour from metadata alone (ADR-W005 A5, ADR-W050 §2.3).
