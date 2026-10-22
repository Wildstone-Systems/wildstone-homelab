# ADR-W023: The install is a conversation

- **Status:** accepted; amended by ADR-W033 (A3, A7)
- **Governed by:** ADR-W033 where they overlap.
- **Relates to:** ADR-W014 (installer bootstrap), ADR-W020 (identity
  onboarding; A3 `LdapUser`, A4 credentials are never
  declarative, A7 reuse Authelia's reset flow), ADR-W021 (installing a
  capability; A5 the generated form, A7 the approval gate),
  ADR-W022 (the addressing ladder)
- **Audience:** decision record for what installing Wildstone is like for
  the person doing it.

---

The installer works and asks nothing. Every decision is a flag that must
be known before the command is typed, and everything after it — read a
token out of the journal, mint a join token, run a second install for the
node role, enable six capabilities, approve six plans — is left to a
person who has just been handed a URL and no context.

Measured on a real alpha VM, a working single-host install took: two
installer runs, one journal grep for a token that returned three
candidates from different installs, one `mint-join`, four capability
installs, five plan approvals, and two container restarts by hand. Every
one of those steps is knowable by the machine. The operator supplied
almost nothing that could not have been derived.

This records what the install should be instead, and it is not a
smaller script. It is a conversation that ends with a working household.

# Part A — Decisions (binding)

### A1. The install is interactive, and it is the whole setup

One command, run once, produces a household that works: Docker present,
the host on a tailnet if wanted, a first Owner who can log in, an ingress
serving trusted TLS, and the chosen services installed and reachable.

The nine steps, in order, because the order is a constraint and not a
preference:

1. **License** — shown, with an explicit accept. Nothing is shown today.
2. **Preflight** — Linux, systemd, architecture. Fail early and say which.
3. **Docker** — if absent, ASK, then install. Today it is installed by
   piping a remote script to a shell with no consent and no mention.
4. **Remote access** — offer Tailscale; print the auth URL; wait for the
   join. Signup is a link, not an automation.
5. **Owner account** — username and password, captured here (A3).
6. **Services** — choose from the catalog.
7. **Summary and confirm** — everything about to happen, one answer (A2).
8. **Install** — Docker, Tailscale, keystone, pair the node, ingress,
   identity, dashboard, services.
9. **Done** — print the URL. That is the whole handoff.

### A2. Step 7 IS the approval gate, not a bypass of it

ADR-W021 A7 says installing from the Console does not bypass approval,
and that holds. An install that stopped for six separate approvals would
be asking a person to approve plans they cannot yet read, for a system
they have not yet seen.

The confirmation at step 7 is a human approving a described change while
present at the machine, which is exactly what the gate exists to
require. The installer therefore approves the plans it proposes, and the
audit trail records that they were approved by the installing Owner.

**Rejected: proposing without approving**, leaving a fresh install with
six pending plans and a Console the Owner cannot reach until identity
is up — which is one of those plans. The gate would be satisfied and the
household would be broken.

### A3. The first Owner is created during the install

A fresh install does not complete without an active Owner (ADR-W033
A3). The installer already has the person's attention and a terminal. It
asks for a username and password, creates the first Owner and their
directory account together, and never prints a token anybody has to
find. There is no Console first-run screen and no "first browser visitor
becomes admin".

If web sign-in is lost, recovery is the audited host CLI run as root
(`keystone owner create|reset-password`, ADR-W033 A7).
`keystone claim-code --data-dir` is a root-only host tool that mints a
bearer token from the data directory; it is not a sign-in path. The
journal is not a mechanism.

### A4. Interactive only on a TTY with no role flag

The installer is also the upgrade path and the node-join path, and both
are scripted. So: interactive when stdin is a terminal AND no `--role`
was given; otherwise exactly the flag-driven behaviour that exists now.

Every current flag keeps working and keeps meaning what it means. An
upgrade re-runs the same command and changes only what changed — it must
never re-ask a question, and must never reinstall a household.

### A5. A failed install resumes; it does not restart

Installs fail partway. On the alpha VM two did, once for a forbidden
mount and once for an agent that had not connected yet. A wizard that
loses ten answers to a failure is worse than the script it replaced.

Answers are persisted as they are given, and every step is idempotent
and skippable when already satisfied. Re-running after a failure
continues from the first unsatisfied step and asks nothing already
answered. The persisted answers hold no credential — the Owner's password
is used and discarded, never written down (A3, and ADR-W020 A4's rule
that credentials do not live in state).

### A6. A single host installs both roles and pairs itself

Keystone and the node agent on one machine is the common case and
currently takes two runs plus a hand-minted join token. Nothing in that
sequence needs a human: the installer mints the token, runs the node
role, and waits for the agent to connect before any capability is
proposed — because a capability proposed before the agent connects fails
and leaves a poisoned plan (F60).

### A7. Household members are added by an Owner, as declared resources

An Owner adds people from the Console, which declares `LdapUser`
resources (ADR-W020 A3). Their accounts are in the Statefile, in plans,
in approval and in audit, like everything else.

**Rejected: handing the Owner LLDAP's own UI.** It is the shortest path
and it puts the household's account list outside the Statefile, which
A3 rejects for a reason that has not changed: a Statefile restore would
bring back every group with nobody in them, and read as a working system
while being an empty one.

The first credential does not travel in the spec (ADR-W020 A4). For this
version the Owner hands the person a one-time credential and Authelia's
own reset flow takes it from there (ADR-W020 A7), rather than building a
second password surface. The QR claim page (A5/A6 of that ADR) remains
the intended end state and is not required to add a household member.

### A8. Linux and a terminal, deliberately, for now

The target is someone who can get a shell on a Linux box and paste a
command, and who does not know what LDAP, OIDC, a reverse proxy or a
capability is. They should never need to learn.

That is not "non-technical", and calling it that would design against a
user who cannot arrive: a person who cannot use a terminal cannot get a
Linux host in the first place. Windows is a later target and a real one;
nothing here should make it harder, and nothing here should wait for it.

# Part B — Implementation notes (non-binding)

- **B1. Almost nothing needs asking.** `node` is the hostname;
  `tailnet_name` and `listen_addr` come from `tailscale status`;
  `public_domain` is a logical identifier under the tailnet posture
  (ADR-W022 A1). The genuine questions are: accept the licence, install
  Docker, use Tailscale, Owner username and password, which services.
- **B2. The first-target catalog needs no operator secrets.** Only
  `documents-paperless` (an OIDC providers block) and the two Cloudflare
  profiles declare `provided: true` secrets. Jellyfin, Immich, identity
  and dashboard need none, so a tier-0 install can be genuinely
  question-light. A package that needs a secret must collect it at step 6
  using ADR-W021 A5's generated form, not fail at step 8.
- **B3. The Tailscale step leaves the terminal.** `tailscale up` prints a
  URL that must be opened in a browser, and signup cannot be automated.
  Design for the hop rather than discovering it: print the URL plainly,
  poll for the join, and say what to do if there is no account yet.
- **B4. Ordering is load-bearing.** identity before any service that
  needs an OIDC client; ingress before Routes mean anything; the agent
  connected before any capability is proposed. Getting this wrong is not
  a slow install, it is a failed plan that stays in desired state and
  blocks its own repair (F60).
- **B5. Config changes must reach the services that read them**, or a
  service selected at step 6 appears installed and serves a stale
  configuration (F73). The digest mechanism covers this now; an installer
  that restarts nothing by hand depends on it continuing to.
