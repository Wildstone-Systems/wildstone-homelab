<!--
Thanks. CONTRIBUTING.md is short and worth reading first: small,
one-concern PRs are welcome; large ones need an issue and a conversation
before the code. The checklist below is what review looks for, so
ticking it honestly saves a round trip.
-->

## What and why

<!-- One or two sentences on the change and the problem it solves.
Link the issue if there is one, and name the ADR it touches
(docs/decisions/ADR-W0xx). -->

## How it was verified

<!-- What you ran and what happened. CI runs the same checks; say if
you could not run one locally. -->

- [ ] `gofmt -l` clean, `go vet ./...` and `go test -race ./...` pass in each Go module I touched
- [ ] `npm run build` passes in `console/` (if the Console changed)
- [ ] `bash -n` and `shellcheck` pass on anything in `scripts/` (if a script changed)

## Nothing private, nothing secret

This repository is public and runs a real household. Every box below
is a release blocker, not a nit.

- [ ] No credential, token, key, or password anywhere, including in test fixtures and commit messages
- [ ] Test fixtures that need a secret-shaped value assemble it at runtime or use a `REPLACED-*` placeholder; no `KEY=value` literal in source
- [ ] No hostnames, LAN or tailnet addresses, home directories, usernames, or household names from a real deployment. Use `example.internal`, `192.0.2.x`, `100.64.0.x`, `~`
- [ ] No compiled binaries, archives, or database files committed

## Structure

- [ ] One concern per PR
- [ ] No pinned red-team test, structural test, or security property is weakened or skipped
- [ ] The Wardstone protocol still has no `Exec(string)`; the Butler still cannot approve or apply
- [ ] Console, CLI, and Butler still share the same public API
- [ ] Docs and ADRs updated where behaviour changed

## CI and runners

<!-- Delete this section if .github/workflows/ is untouched. -->

- [ ] I changed a workflow, and the diff is limited to what the PR needs
- [ ] Every job stays on a GitHub-hosted runner (`runs-on: ubuntu-latest`)
- [ ] No new secret is referenced by a workflow
