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
