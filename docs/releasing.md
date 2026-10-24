# Releasing

How to cut a Wildstone release, check it, and recover one that went
wrong. The pipeline itself is `.github/workflows/release.yml`
(ADR-W014 A1); the installer that consumes it is `scripts/install.sh`.

## Version names

- `vX.Y.Z-alpha` — an ordinary alpha release. It becomes **latest**, so
  `releases/latest/download/install.sh` (the one-liner in the README
  and on the website) starts installing it.
- `vX.Y.Z-rc.<name><n>` — a release candidate, such as
  `v0.1.13-rc.mail1`. `release.yml` publishes anything containing
  `-rc.` as a **pre-release**, so it never becomes latest and installs
  only with an explicit `--version`. See
  [mail-monitoring-test-vm.md](mail-monitoring-test-vm.md) for the flow.

Never reuse or move a published tag: an installer that verified one
build must keep getting that build. `.github/rulesets/release-tags.json`
enforces this.

## Before you tag

- CI is green on the `main` commit you are releasing.
- The latest exit drill (`exit-drill.yml`) is green. It is not a
  required check, but a red drill is a release blocker (ADR-W018).

## Cut the release

Either publish from GitHub:

1. **Releases → Draft a new release.**
2. Type the new tag and choose **Create new tag on publish**, target
   **main**.
3. Title it, write the notes, and optionally **Generate release notes**
   to append the merged pull requests.
4. Leave **Set as a pre-release** unticked (unless it is an `-rc.`
   tag), keep **Set as the latest release** ticked, and publish.

or push a tag from a clone of `main`:

```sh
git tag -a v0.1.14-alpha -m "v0.1.14-alpha"
git push origin v0.1.14-alpha
```

Either way, the tag push starts `release.yml`. It builds every binary
for linux amd64 and arm64, the Console, the packages and wallpapers
bundles, the native agent runtime, `install.sh`, `uninstall.sh` and one
`checksums.txt`, and attaches them all. It takes a few minutes. A
release published from the GitHub page keeps its title and notes; the
workflow only uploads the files onto it.

## Check it

- The release has 20 assets, including `checksums.txt`, `install.sh`
  and an `_amd64` and `_arm64` archive of `keystone`, `wardstoned`,
  `wild`, `chat-adapter`, `agent-bridge`, `wild-mail-adapter` and
  `native-agent`.
- The latest link serves it and verifies:

  ```sh
  curl -fsSLO https://github.com/Wildstone-Systems/wildstone-homelab/releases/latest/download/install.sh
  curl -fsSLO https://github.com/Wildstone-Systems/wildstone-homelab/releases/latest/download/checksums.txt
  grep ' install.sh$' checksums.txt | sha256sum -c -
  ```

- A downloaded binary reports the tag, e.g. `keystone -version` prints
  `keystone v0.1.14-alpha`.

## A release that published with no assets

The release exists, but no files are attached. Open **Actions →
release** for that tag:

- **Startup failure, no jobs.** GitHub refused to start the workflow,
  almost always because of the Actions policy (the run page says which
  actions "are not allowed"). Fix the setting first — see
  [repository-settings.md](repository-settings.md#actions-settings--actions--general).
- **A failed step.** Read the log, fix the cause on `main` through a
  pull request.

A startup failure has no **Re-run** button, and the workflow only runs
when a tag is pushed, so the tag has to be created again. Deleting the
release is not enough: the tag survives it, and a release published
onto an existing tag pushes nothing.

1. Delete the tag: **Tags → ⋯ → Delete tag**, bypassing the
   `release-tags` ruleset as an admin. This is safe only because a
   release with no assets cannot have been installed. The release turns
   into a draft and keeps its notes.
2. Edit the draft, type the same tag again, choose **Create new tag on
   publish** with target **main**, and publish.

The tag is recreated at the current head of `main`, so check that CI is
green there first. If you would rather not touch the ruleset, publish
the next version instead and delete the empty release.
