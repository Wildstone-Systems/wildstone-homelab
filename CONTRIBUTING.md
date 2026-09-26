# Contributing

Thanks for your interest. Please read this before opening anything —
it will save us both time.

## Project stage: pre-alpha, opinionated, solo-maintained

Wildstone is being built to a written scope
(`docs/02-implementation-scope.md`) with binding architecture decisions
(`docs/decisions/`, the W-series ADRs) and a vision (`docs/vision.md`)
that ambiguous questions resolve against. Until the alpha ships:

- **Issues are welcome** — bug reports, security-property questions,
  and friction from trying it out are genuinely useful. Use GitHub
  issues; for vulnerabilities use `SECURITY.md` instead.
- **Small PRs are welcome** — typos, doc fixes, a failing-test
  reproduction, a contained bug fix.
- **Large PRs will likely be declined**, however good. Features land
  epic-by-epic against the scope and its ADRs; an unsolicited feature
  PR usually conflicts with a decision it couldn't have known about.
  Open an issue first and let's talk.
- There is **no support SLA**. This runs a real family's home; that
  household's needs set the priorities.

## Ground rules (enforced by CI and structural tests — not negotiable)

1. No secrets in the repository, ever, including test fixtures.
2. No `Exec(string)` in the Wardstone protocol.
3. Console, CLI, and Butler consume the same public API — no private
   endpoints.
4. Every intentional change to host state is a plan a human approves.
5. The Butler proposes and never approves; its ceiling is structural.
   PRs that weaken a pinned red-team or structural test are rejected
   regardless of what they enable.

## Practical checklist for a PR

- Run the same checks CI runs: `gofmt -l`, `go vet ./...`, and
  `go test -race ./...` in each Go module (`proto`, `wardstone`,
  `keystone`, `cli`, `testing/e2e`); `npm run build` in `console/`;
  `bash -n` + `shellcheck` on anything in `scripts/`.
- Match the surrounding code's style and comment discipline.
- One concern per PR; reference the ADR your change touches.
- Fill in the pull request template. Its checkboxes are the review
  criteria below, in the order review applies them.

## Review criteria

What a PR is checked against, in order. The first three are blockers:
a PR that fails one is closed or sent back regardless of its merits.

1. **Nothing private, nothing secret.** No credential in any form,
   including fixtures and commit messages. No detail of a real
   deployment: hostnames, LAN or tailnet addresses, home directories,
   usernames, household names. Tests that need a secret-shaped value
   build it at runtime or use a `REPLACED-*` placeholder, because the
   secret scanner cannot tell an invented key from a real one and is
   right to fire on both. CI runs gitleaks on every push.
2. **No structural property weakened.** The five ground rules above,
   and every pinned red-team or structural test. A PR that skips,
   disables, or loosens one of these to make something work is
   rejected regardless of what it enables.
3. **Workflow changes are minimal and keep the runner safe.** CI runs
   on a self-hosted runner inside the maintainer's network. Jobs on it
   skip pull requests from forks, and a fork's workflow only ever runs
   after the maintainer approves it, having read the `.github/` diff
   first. The secret scan is the exception: it runs on a GitHub-hosted
   runner for every pull request, forks included, because it is the
   check that keeps a credential out of public history. A PR that touches `.github/workflows/` is reviewed for
   exactly that diff and nothing more; a change that removes the fork
   guard or references a new secret needs an issue first.
4. **One concern, verified.** The change does one thing, says which
   ADR it touches, and the author ran the same checks CI runs.
5. **Fits the code around it.** Style, comment discipline, and the
   design decisions in `docs/decisions/`. An unsolicited feature that
   conflicts with an ADR is declined with a pointer to the ADR.

### For the maintainer, on a pull request from a fork

- Read the diff of `.github/workflows/` before pressing "Approve and
  run". Approval runs the PR's version of the workflow, not `main`'s.
- Never approve a run for a PR that adds or renames a workflow secret.
- To test a contribution on the runner without approving the fork,
  push the branch into this repository.

## Optional: the Graft code graph for AI agents

[Graft](https://github.com/trailhq/Graft) builds a local index of this
repo so a coding agent can answer "where does X happen?" from a graph
instead of re-reading the tree every session. It is optional, local-only,
and touches no committed source.

```bash
npm install -g @nanonets/graft
graft telemetry disable          # or export DO_NOT_TRACK=1
graft init --agents claude --no-global
graft build                      # structural pass only: ~10s, no API key, no network
```

Rules for this repo:

- **Structural pass only.** Run `graft build`, never `graft build --deep`.
  The `--deep` flag sends source files to a third-party LLM for
  summarisation; that is not a call any contributor makes unilaterally
  here.
- **Telemetry off.** Graft's usage stats are opt-in, but disable them
  explicitly so a stray `graft init` cannot re-enable them.
- **Nothing generated is committed.** `graft/` is git-ignored and
  regenerable; `.ignore` keeps it out of ripgrep while leaving it
  greppable on purpose. `.claude/` is git-ignored by this repo's own
  policy, so each contributor runs `graft init` for their own machine.
- `.mcp.json` is committed so the MCP server registration is shared. If
  you do not have Graft installed, Claude Code logs a failed server and
  carries on; nothing else breaks.

Useful without an agent: `graft map` (repo orientation), `graft callers
<symbol> --depth all` (blast radius), `graft ask "<question>"`.

## Licensing

Contributions are accepted under the repository's license (AGPL-3.0).
By submitting a change you certify you have the right to contribute it
under that license (Developer Certificate of Origin,
https://developercertificate.org/ — sign-off line appreciated but not
required).
