# Testing mailbox monitoring on a VM from a pre-release tag

This runbook takes a branch carrying mailbox monitoring (epic #326) to a
throwaway VM: cut a release-candidate tag, install that exact build, turn
on *Watch this mailbox* for a test mailbox, watch a review arrive, add the
event, and then prove that revoking and erasing really stop and remove
everything. It ends with what to do for each health state.

Use a mailbox you can throw away, at a provider that issues **app
passwords** (a separate, revocable password per app). Do not point a test
VM at a mailbox whose password you also use elsewhere.

## 1. Cut the pre-release

From the branch, tag and push a release candidate. Anything containing
`-rc.` is published as a **pre-release**, so it never becomes the
installer's default `latest`; only an explicit `--version` installs it.

```sh
git checkout claude/exciting-volta-s3mgzs && git pull
git tag v0.1.13-rc.mail1          # next version, then -rc.mail<N>
git push origin v0.1.13-rc.mail1
```

`.github/workflows/release.yml` builds `keystone`, `wardstoned`, `wild`,
`chat-adapter`, `agent-bridge` and `wild-mail-adapter` for amd64 and arm64,
the Console, the bundled packages and `install.sh`, checksums them all in
one `checksums.txt`, and publishes the release with `--prerelease`. Check
the run finished and the assets are there:

```sh
gh release view v0.1.13-rc.mail1 -R chickenrat17/wildstone-homelab --json isPrerelease,assets \
  --jq '.isPrerelease, [.assets[].name]'
# true, and the list includes wild-mail-adapter_linux_amd64.tar.gz
```

Need another build? Bump the suffix (`-rc.mail2`); do not move a tag.

## 2. Install that build on the VM

On a fresh VM (Debian/Ubuntu, 2 vCPU, 4 GB RAM is plenty), fetch the
installer *from that release* and pin the version:

```sh
gh release download v0.1.13-rc.mail1 -R chickenrat17/wildstone-homelab -p install.sh
sudo GH_TOKEN=github_pat_... bash ./install.sh --role keystone --version v0.1.13-rc.mail1
```

(`GH_TOKEN` only while the repository is private; see `docs/install.md`.)
Then join the VM as its own node, again with `--version`:

```sh
sudo bash ./install.sh --role node --keystone 127.0.0.1:9440 \
  --join-token <token> --ca-fingerprint <hex> --version v0.1.13-rc.mail1
```

Confirm the adapter program is on the node and is this build:

```sh
/srv/wildstone/bin/wild-mail-adapter -version   # wild-mail-adapter v0.1.13-rc.mail1
```

Record the admin token from `journalctl -u keystone -o cat | grep -A1 'first start'`
and `export WILD_TOKEN=wst_...` for the `curl` calls below
(`API=http://127.0.0.1:9441`). The `/api/v1/mail/setup` read model
shows the *caller's own* monitors, so read it with the Person's own
Console session when the Person is not the Owner.

## 3. Install email, a calendar and a local model

1. **Email**: install `mail-roundcube` 1.1.x from Console → Capabilities
   (or `wild capability enable mail-roundcube -set imap_host=imap.<provider> -set smtp_host=smtp.<provider> ...`).
   Approve its Plan. Installing reads no mail and creates no connection —
   check that no `mail-adapter-*` container exists (`docker ps`).
2. **Calendar**: install the calendar capability (`calendar-radicale`) and
   declare, in the shared calendar registry (ADR-W038), a calendar the
   test Person may **write** (the target) and any they may read (for
   conflicts).
3. **Model (optional)**: an `.ics` invitation is read deterministically
   and needs no model. Prose mail needs the monitor Agent to have a
   **local** model (for example an Ollama route) — with none, prose mail
   is recorded `skipped: no_local_model` and nothing goes to the cloud.
4. **People**: the test Person must be an admitted member with access to
   the Agent that will watch (Console → People, and the Agent's access).
   To test isolation, admit a second Person with a second test mailbox.

## 4. Set up the test mailbox

At the provider, create an **app password** for the test mailbox. Keep
the address and app password at hand; you will type the password once,
into Wildstone's protected input, and never into chat.

## 5. Turn monitoring on, in order

Open Console → **Mailbox** (or the email capability → *Watch this
mailbox*). The screen decides its step from the server, so a refresh or a
restart resumes where you were. The order, and who does each step:

| # | Who | Step | What changes |
|---|-----|------|--------------|
| 1 | Owner | **Prepare**: create the Person's mailbox connection and its placement node | A Personal connection owned by the Person; nothing runs |
| 2 | Owner | **Render** the placement | Records the adapter executable and mints a node-bound adapter token; still nothing runs |
| 3 | Person | **Credential**: mailbox username and app password | Sent once over mTLS to the placement node's secret store; Keystone keeps no copy |
| 4 | Person | **Choose** folders (INBOX), sender/subject filters, cadence, target calendar, conflict calendars, files the subscription | A subscription Plan for the Owner; authorizes nothing |
| 5 | Owner | **Approve** the subscription Plan (Console → Plans) | The exact subscription version is approved; still not authorized |
| 6 | Person | **Authorize** that subscription | The Person's authorization; Keystone files the adapter render Plan |
| 7 | Owner | **Approve** the render Plan | The reconciler starts `mail-adapter-<connection>-<node>` with the credential materialized |
| 8 | Owner | **Mark the placement ready** | The node takes the lease; scheduled reads begin |

Step 8 is still manual. Keystone does not yet have a trustworthy signal
that the adapter is running *with its credential* and has reached
Keystone (an adapter can only claim work once it holds the lease), so an
Owner checks and says so:

```sh
docker ps --filter name=mail-adapter-          # running
docker logs mail-adapter-<connection>-<node> 2>&1 | grep 'wild-mail-adapter starting'
# ... operations=4 available=4   (not_configured would show available=0)
curl -fsS -H "Authorization: Bearer $WILD_TOKEN" -X POST "$API/api/v1/integrations/<connection>/placement/ready" \
  -d '{"node":"<node>","containerRunning":true,"secretProvisioned":true,"readinessVerified":true}'
```

The first scheduled run commits a **baseline**: mail already in the
folder is not read. Only mail that arrives afterwards is.

## 6. Send test mail

From another account, send two messages to the test mailbox, from a
sender inside the filter you chose:

1. **An invitation**: create an event in any calendar app for a time a
   few days out and invite the test address (a `text/calendar` part).
2. **A prose email**: "The bake sale is Friday, October 16, 2026 from
   3:00 to 4:00 p.m. in the school lobby." (Use a real future date.)

Also send one from a sender **outside** the filter: it must produce
nothing.

## 7. Watch the review arrive, then Add

Within one cadence (15 minutes by default; set 5m while testing), the
Person sees a private review on Home (and on their one-to-one chat with
the Agent, if they have one), naming only conflicts on calendars they may
read. The Mailbox screen shows the run and the candidate:

```sh
curl -fsS -H "Authorization: Bearer $WILD_TOKEN" "$API/api/v1/mail/setup" | jq '.monitors[].subscriptions[] | {schedule, checkpoint, runs}'
```

Choose **Add**. Wildstone rechecks availability and creates exactly one
event on the target calendar (UID `wildstone-candidate-<id>`). Clicking
Add again, or replying "yes", does nothing.

## 8. Verify the event

Open the target calendar (calendar app, or the calendar capability's web
view): one event at the right time in the household timezone. The
candidate now shows `created` with its effect (calendar, UID, ETag), and
no excerpt.

## 9. Verify revoke and erase

As the Person, **Stop watching** (revoke the credential) on the Mailbox
screen. An Owner administering can do the same — withdrawing access needs
no one's consent:

```sh
curl -fsS -H "Authorization: Bearer $WILD_TOKEN" -X POST "$API/api/v1/integrations/<connection>/credential/revoke" | jq
```

Then check, in this order, that each step happened:

1. **Fenced**: the next scheduled run does not dispatch (the run list
   shows it blocked), and any open review is revoked.
2. **Removed**: `docker ps --filter name=mail-adapter-<connection>` is empty.
3. **Erased**: the answer lists one verified erasure per node for
   `pc.<connection>.v<n>` and `pc.<connection>.fk.v<n>`, and the
   connection is **not** `degraded`. If it says `erasure_unverified`, see
   below.
4. A removal render Plan is proposed; approve it so desired state agrees.
5. At the provider, revoke the app password too.

Optionally, as the Owner, remove `mail-roundcube` (Console →
Capabilities → Remove). The removal fences every monitor the adapter runs,
removes their containers and erases their credentials *when the removal
is proposed*, and the removal Plan deletes their adapter resources.

## 10. Troubleshooting by health state

The Mailbox screen shows the subscription's last outcome and hold, the
connection's state, and per-candidate states. What each means:

| Shows | Meaning | Do |
|-------|---------|----|
| run `empty` | The provider answered; nothing new in scope | Check the sender is inside the filter and the mail arrived after the baseline |
| run `unavailable` — *not configured yet* | The adapter has no credential or key | Re-enter the credential; check the render Plan was approved and the container restarted |
| run `unavailable` — *could not be reached* | IMAP host/port/encryption, DNS or firewall | Check `imap_host`, `imap_port`, `imap_encryption` on the package; `docker logs` on the adapter |
| run `unavailable` — *did not answer in time* | No adapter holds the lease | Placement not marked ready (step 8), container down, or the token was not placed; re-render |
| hold `auth_failed` | The provider refused the app password | Create a new app password, re-enter it, then **Resume** the subscription |
| hold `config_failed` | A folder that does not exist, or a malformed filter | Fix the scope (files a new version for approval), then Resume |
| run `error` — *model extraction was unavailable* | Prose mail and the local model is down | Bring the model back; the batch is retried (up to three times), nothing is skipped |
| run `abandoned` | Keystone restarted mid-run | Nothing; the next run re-reads from the committed cursor |
| blocked — *not authorized* / *not approved* | One of the two keys is missing or was changed | Re-authorize as the Person, or approve the new subscription version as the Owner |
| connection `unavailable` — *no prepared node* | No placement is ready | Mark the placement ready, or prepare a fallback node |
| connection `degraded` — `erasure_unverified` | A credential copy could not be proven erased (node unreachable, Wardstone below protocol minor 22) | Bring the node back or update it, then `POST /api/v1/integrations/<connection>/credential/erase` |
| candidate `needs_details` | Missing or ambiguous time or timezone | **Edit** the exact fields, then Add |
| message `skipped: no_local_model` | Prose mail, and the Agent has no local model | Give the Agent a local model, or rely on invitations |
| message `skipped: oversized` / `too_deep` / `malformed` | Outside the bounded parser's limits | Expected for unusual mail; nothing is guessed |
| review `expired` | Not answered within 72 hours or before the event | Nothing; a new message about it will ask again |

Nothing on these screens, in the logs, or in a backup contains the app
password, a full message, its subject or its sender — if you ever see one,
treat it as a security bug.
