# Repository settings

What `Wildstone-Systems/wildstone-homelab` is set to on GitHub, and why.
Most of it lives in GitHub's settings pages rather than in files, so this
is the record. The order matters on day one: the rulesets go last,
because they require CI checks that cannot pass until Actions is on.

## Organization (github.com/organizations/Wildstone-Systems/settings)

- **Authentication security:** require two-factor authentication for
  everyone in the organization.
- **Member privileges:** base permission **Read**. Members cannot create
  public repositories or change visibility.
- **Profile:** display name, a short description, a public email that is
  not a personal inbox, and the project's website if there is one.

## General (repository Settings → General)

- **Features:** Issues on, Discussions on (questions go there, see
  `.github/ISSUE_TEMPLATE/config.yml`), Wiki off (docs live in `docs/`),
  Sponsorships on once the Sponsors profile is approved.
- **Pull Requests:** allow merge commits and squash merging, not rebase
  merging. Always suggest updating pull request branches. Allow
  auto-merge. Automatically delete head branches.
- **About** (the gear on the repository's front page): description,
  topics such as `homelab`, `self-hosted`, `home-server`, `golang`.

## Actions (Settings → Actions → General)

- **Actions permissions:** "Allow Wildstone-Systems, and select
  non-Wildstone-Systems, actions and reusable workflows", with **Allow
  actions created by GitHub** ticked. Every workflow uses GitHub's own
  `actions/*` (checkout, setup-go, setup-node, setup-python,
  upload-artifact); nothing else is needed, because every third-party
  tool CI runs is a checksum-pinned download, not an action. Set it in
  the **organization's** Actions settings as well as the repository's:
  an organization policy overrides the repository.

  Getting this wrong does not fail a step — it fails every workflow
  before it starts. The run shows **Startup failure** with no jobs, and
  the run page names the actions that "are not allowed". A startup
  failure cannot be re-run; push again once the setting is fixed (for a
  release, see [releasing.md](releasing.md)).
- **Fork pull request workflows:** require approval for all external
  contributors. Read the `.github/workflows/` diff before approving.
- **Workflow permissions:** read repository contents only, and do not
  let Actions create or approve pull requests. `release.yml` asks for
  `contents: write` itself.

## Advanced Security (Settings → Advanced Security)

All free on a public repository.

- **Private vulnerability reporting:** on. `SECURITY.md` sends reporters
  there; without it the link is dead.
- **Dependency graph, Dependabot alerts, Dependabot security updates:**
  on. Version updates come from `.github/dependabot.yml`.
- **Secret scanning and push protection:** on. Push protection stops a
  credential at `git push`, before it reaches history; the gitleaks job
  in CI catches anything else.
- **Code scanning:** CodeQL **default setup**, all languages it offers.

## Rulesets (Settings → Rules → Rulesets)

Import each file with **New ruleset → Import a ruleset**, after Actions
has run CI on `main` at least once so the checks exist.

- `.github/rulesets/main.json` — `main` cannot be deleted or
  force-pushed; every change arrives through a pull request whose
  conversations are resolved and whose CI checks pass. No approving
  review is required, because a solo maintainer cannot approve their
  own pull request; raise `required_approving_review_count` when there
  is a second maintainer. Repository admins may bypass, but only by
  merging a pull request, never by pushing.
- `.github/rulesets/release-tags.json` — a published `v*` tag cannot be
  moved or deleted, so the release an installer verified stays the
  release. Admins can bypass to correct a mistake; see
  [releasing.md](releasing.md) for when that is needed.

The exit drill (`exit-drill.yml`) is not a required check: it takes up
to 45 minutes and runs weekly regardless. A red drill is still a
release blocker (ADR-W018).

## GitHub Sponsors (github.com/sponsors)

1. **Get sponsored → Wildstone-Systems.**
2. Fill in the profile: what Wildstone is, who it is for, what
   sponsorship pays for (hardware to test on, time on the roadmap).
3. Add the payout bank account and tax form. Both stay private; they
   are not shown on the profile.
4. Add a few monthly tiers and submit. GitHub reviews the profile before
   it goes live.
5. Once approved, the Sponsor button from `.github/FUNDING.yml` works,
   and Settings → General → Features → Sponsorships can be ticked.
