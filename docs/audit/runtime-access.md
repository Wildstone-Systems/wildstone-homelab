# Reaching a runtime

Runtime evidence is what §17 asks for, and an audit that marks
everything `unverified` because it could not find the API has answered
a different question than the one it was given.

## Read this first: which runtime you can reach

**A cloud session cannot reach the household deployment.** All six
slices of the first audit round ran in isolated containers with no
route to it, and this file used to assume otherwise. Symptoms are
unambiguous: `systemctl` reports the system was not booted with systemd,
`/etc/wildstone/` and `/usr/local/bin/keystone` do not exist, and
`curl http://127.0.0.1:9441/api/v1/butlers` returns `000`.

That is not a dead end. Three of those six slices got genuine runtime
evidence anyway by **building the audited commit and running it against
a scratch data directory** — see below. It is real evidence about *this
code*, which is what most findings are about. What it cannot tell you
is how the household's own deployment is configured; record that as
`unverified` and say what would settle it.

| You are on | Use |
|---|---|
| A cloud/container session | A scratch instance. The live deployment is unreachable. |
| The homelab host | A scratch instance for anything that **writes**, and the live deployment **read-only** for configuration questions. |

**Do not run mutating tests against the household deployment.**
Creating admissions, revoking grants, or storing memory on the running
system disrupts a household and is hard to unwind. A scratch instance
on the host can point at the *real* model endpoints, which gives the
genuine cloud path without any of that.

**No credential values in this file, ever** — paths and procedures
only. The same goes for anything you write into a report or an issue.

---

## One working tree each

**Two sessions in one clone will silently overwrite each other.** It
happened: both host-round sessions checked out through
`~/wildstone-homelab` fifteen seconds apart, and either
one's next `git checkout` would have swapped the files out from under
the other mid-edit.

If another session is already working in the clone, take your own tree:

```bash
cd ~/wildstone-homelab
git worktree add ../ws-<issue> <your branch>
cd ../ws-<issue>
```

Same repository and history, separate checkout, and git refuses to have
one branch checked out in two trees — which is the guard you want.
`git worktree remove ../ws-<issue>` when you are done.

Pick your own port and data directory for the scratch instance too. Two
of them on `:19441` is the same collision by another route.

## A scratch instance

Build the commit under audit and run it beside everything else. It has
its own data directory, its own port and its own admin token, and it
shares nothing with the household.

```bash
cd keystone && go build -o /tmp/ks ./cmd/keystone
mkdir -p /tmp/ksdata

# One-time: mint an admin token into the scratch data directory.
/tmp/ks claim-code -data-dir /tmp/ksdata -out /tmp/ks.token

/tmp/ks -data-dir /tmp/ksdata \
        -api-listen 127.0.0.1:19441 \
        -hosts localhost,127.0.0.1 \
        -butler-provider ollama \
        -butler-endpoint http://192.0.2.99:11434 \
        -butler-model qwen3.5:4b &

export WILD_TOKEN=$(cat /tmp/ks.token | tr -d '\n')
wild butler status -surface family -api http://127.0.0.1:19441
```

Notes that matter:

- **`-butler-endpoint` is the real Ollama** when you are on the host,
  and unreachable from a container. Point it at a stub when it is, and
  say so in the report — a stub proves routing and cannot prove what
  left the house.
- Migrations run on start, so the scratch database is the schema of the
  commit under audit. That makes it the right place to answer schema
  questions and the wrong place to answer "what does the household's
  database actually contain".
- **Delete it afterwards** — the data directory, the token file and the
  process. Say in the report that you did.

---

## The household deployment

Read-only, and only from the host. Everything in this section describes
the household's running system; nothing here should be written to.

### What is running

`keystone.service`, a systemd unit on this host.

```
systemctl status keystone
sudo journalctl -u keystone -f           # follow
sudo journalctl -u keystone --since "10 min ago" -o cat
```

Its arguments live in `/etc/wildstone/keystone.env` (root-readable).
The ones that matter for an audit:

| | |
|---|---|
| Public API | `0.0.0.0:9441` — reach it on `http://127.0.0.1:9441` |
| Data directory | `/var/lib/wildstone/keystone` |
| Console | served from `/usr/share/wildstone/console` |
| Legacy Butler provider | `ollama` at `http://192.0.2.99:11434` |

The node also runs `wardstoned.service`, the agent.

---

### Authenticating

The API takes `Authorization: Bearer <token>`, and the CLI reads
`WILD_TOKEN`.

A working admin token is at `/var/lib/wildstone/keystone/claim-code`
(root, mode 0600). Load it without printing it:

```bash
export WILD_TOKEN=$(sudo cat /var/lib/wildstone/keystone/claim-code | tr -d '\n')
```

The files in `~` — `.wild_token`, `.wild_operator_token` — are **stale**
and return `401 unauthenticated`. Do not spend time on them.

If that token stops working, `keystone claim-code` mints a fresh one
into the data directory, but it needs the **service stopped**, which
takes the household's Butler offline. That is a deploy-shaped action,
not an audit-shaped one: ask before doing it.

```bash
sudo systemctl stop keystone
sudo /usr/local/bin/keystone claim-code -name admin
sudo systemctl start keystone
```

The other authentication path is Console forward-auth, gated on the
identity proxy's exact address (`-household-ingress`). It cannot be
exercised from `127.0.0.1`, so anything that depends on it is
`unverified` unless you drive it through the real Console.

---

### The Butlers on this deployment

As of 2026-09-10. Confirm rather than assume — `GET /api/v1/butlers`.

| Name | Surface | Model | Kind | State |
|---|---|---|---|---|
| Alfred | admin | `gemma4:31b-cloud` | **cloud** | enabled |
| Sandy | family | `kitchen-box` → `qwen3.5:4b` | **local** | enabled |
| Operator | admin | `gemma4:31b-cloud` | cloud | retired |

The cloud/local split is load-bearing for the privacy slice: Alfred's
context leaves the house and Sandy's does not.

`qwen3.5:4b` is small and behaves like it. It occasionally returns an
empty answer, and it needs blunt instructions to reach for a tool. When
a behaviour fails once, run it two or three more times before recording
it as a finding — distinguishing "the code does not do this" from "the
model did not do this today" is the whole job on this deployment.

---

### Exercising it

The CLI is the shortest path to a real turn:

```bash
wild butler ask -surface family "..."     # answers as Sandy
wild butler ask -surface admin  "..."     # answers as Alfred
wild butler status  -surface family
wild butler sessions -surface family -limit 5
wild butler sessions -id bs-44
```

`wild butler ask` opens a **new conversation** each time, which is what
you want for testing whether something persists across conversations.

Useful endpoints, all under `http://127.0.0.1:9441`:

```
GET  /api/v1/butlers
GET  /api/v1/butler/sessions?surface=family&limit=5
GET  /api/v1/butler/sessions?id=bs-44
GET  /api/v1/butler/memory?butler=<name>       # omit butler for every one
GET  /api/v1/butler/memory/digest
POST /api/v1/butler/memory/correct   {"seq":N,"content":"...","confidence":"stated"}
POST /api/v1/butler/memory/forget    {"seq":N}
GET  /api/v1/butler/index/measurements
GET  /api/v1/butler/tools?profile=household
```

Responses pass through the redacting encoder, so a secret-shaped value
comes back as `[REDACTED]`. That is the API behaving correctly, not the
data being absent — worth knowing before you record it as a finding.

---

### Reading the database directly

`/var/lib/wildstone/keystone/keystone.db` is SQLite, root-only, and
**open by the running service**. Do not write to it. If you need to
read it, copy it out first rather than attaching to the live file:

```bash
sudo cp /var/lib/wildstone/keystone/keystone.db /tmp/audit.db
sudo chown "$USER" /tmp/audit.db
```

A copy is enough to answer schema questions. It is not evidence of
runtime behaviour.

---

## Deploying a build

**Not part of an audit.** It restarts the control plane, which
interrupts the household and anyone else reading it. Recorded here
because a remediation issue will need it.

**Commit before you build.** Go stamps the build with the revision it
came from and marks `vcs.modified=true` when the tree was dirty, and a
binary built from a dirty tree is one no commit describes. The
household is running one right now (F-157-22): answering "is the
deployment the code we audited" took string-matching a wordlist out of
the binary, and the assumption was one keystroke from being wrong.

```bash
git status --short                      # must be empty
cd keystone && go build -o /tmp/keystone ./cmd/keystone
go version -m /tmp/keystone | grep vcs  # vcs.modified must be false
sudo install -m0755 /tmp/keystone /usr/local/bin/keystone
sudo systemctl restart keystone
```

Afterwards, `go version -m /usr/local/bin/keystone` names the commit
that is running. That is the whole point of the extra line.

Migrations in `keystone/runtimedb/migrations/` run on start. They are
append-only history: once one has applied, undoing it means writing
another.

---

## When something is blocked

Some credential reads are refused by the sandbox even with `sudo`. That
is expected, not a fault to work around. Record the check as
`unverified`, say exactly which command was refused and what it would
have shown, and move on. §17 requires that deployment checks which
cannot run stay explicitly unverified — an honest gap beats a guess
dressed as a result.
