# ADR-W029: A package is checked against the app it configures

- **Status:** accepted
- **Relates to:** ADR-W005 (package format), ADR-W010 (package
  toolchain), ADR-W019 A10 (a Route is not always served at the host it
  declares), ADR-W021 A2/A5 (params and the install form), ADR-W025
  (traits), ADR-W008 A3 (the reconcile loop's health gate)
- **Audience:** decision record, and the standing brief for anyone —
  person or model — writing an integration package.

---

# Part A — Decisions (binding)

### A1. Lint checks internal consistency; that is not the same as working

Three launch services shipped with every existing check passing and none
of them working:

- `books-calibre-web` was enabled with `timezone: CST`. `CST` is a real
  abbreviation and not a zone. Calibre-Web's scheduler asked `tzlocal`
  for it, got `ZoneInfoNotFoundError`, and the app died during startup —
  while the linuxserver.io image's s6 init kept PID 1 alive, so Docker
  reported the container `Up` and the proxy answered 502.
- `photos-immich` set nine `OAUTH_*` environment variables on
  `immich-server`. Immich has not read OAuth from the environment since
  it moved those settings into its system config. Every one was inert.
  Immich's own startup log printed the config it actually held —
  `"oauth": false` — beside an Authelia client that was registered,
  reachable, and answering discovery. The login page had no button.
- `ai-open-webui` set `WEBUI_URL=https://chat.<public_domain>`, a name
  that resolves nowhere under the tailnet posture.

None is a malformed package. Each is a package that was never compared
against the thing it configures. The catalog's cross-resource checks —
secrets referenced are declared, mounts name declared volumes, a Route's
upstream is a container the package runs, OIDC redirects land on a host
the package serves — are all checks of the package against **itself**.

So the binding rule: a package's claims about an application are
findings that must be recorded where a checker can reach them. What an
app reads, and what it does with what it reads, is knowledge; this
repository is where that knowledge is written down.

### A2. A param declares the SHAPE of its value, and the shape is enforced

`ParamDecl.Validate` names a shape from the closed vocabulary in
`pkgspec.ParamShapes` (`iana_timezone`, `url`, `hostname`, `email`,
`port`, `duration`). `capability.Expand` refuses a value that fails it.

Enforcement belongs at Expand, not at lint alone, because Expand is the
only point every value passes through: the package's own default, an
operator's `-set`, and the install form's answer all arrive as one
merged map. A bad param value is almost never the author's.

`Description` and `Example` were already there and neither helped. The
calibre-web param's description read "Your timezone, e.g.
Europe/London" — the value it received was ruled out by its own
documentation, and the first thing to notice was a Python traceback.
A description is addressed to a human who may not read it; a shape is
addressed to the machine that must.

### A3. A fact the household states once is not asked twenty-four times

Twenty-four packages ask for a timezone. A household has one clock.
Asking per-package is how one service ends up an hour off from the rest,
and it is twenty-four chances to type an abbreviation instead of a zone.

`source: timezone` joins `node`, `domain`, `keystone_url`,
`directory` and `chat_adapter_image` in `pkgspec.ParamSources`. Keystone
already keeps the household's zone for the automation engine's
`timeOfDay` rules, so the install form offers it rather than presenting
a blank box.

The shape check in A2 stays regardless. Sourcing removes the question;
it does not make the answer unnecessary to check, because an operator
may still override it and because the household setting itself is a
value someone typed.

### A4. `origin` is available wherever a URL is built

`packages/README.md` already told authors: "Use `origin`, never string
concatenation. It answers where a host is actually *served*, which under
the tailnet posture is the node's one certified name plus a port — not
the host you declared."

The function existed only while rendering `provides.exports`. Package
`templates:` were parsed with no FuncMap at all. So an app package that
needed its own browser-visible URL had **no correct spelling available**,
and all twelve that needed one concatenated `public_domain` instead.

That is not author error; it is a missing affordance. `renderTemplate`
now takes the same funcs an export gets, and the `self-url` lint rule
makes the concatenated form a finding.

The rule is scoped to `env:` values. A Route's `host:` is the declared
host and *should* be built by concatenation — it is the input `origin`
resolves *from*. An `OidcClient`'s `redirectUris` are compensated
downstream, where the reconciler adds the resolved form alongside the
declared one.

### A5. What an image ignores is written down, not rediscovered

`pkgtool.inertEnv` is a table of environment variables a given image
accepts and reads nothing from, keyed by a substring of the image
reference so a digest pin still matches.

It has to be a curated table. Nothing in a manifest can tell you which
variables a binary consults; the only source is the app's own
documentation, read by someone integrating it. This is where that
reading is recorded so the next author does not repeat it and cannot
skip it.

A wrong row costs a false positive an author can read and argue with.
That is much cheaper than the silence it replaces.

### A6. A container that is running is not an app that is serving

`Outcome.AppHealth` already carries a container's own healthcheck
verbatim, and already documents that an absent healthcheck means "no
opinion" rather than "healthy". The calibre-web failure is the shape
that makes this matter: the linuxserver.io image declares no
`HEALTHCHECK`, its s6 init holds PID 1 after the app exits, and so the
strongest statement Wildstone could make about a dead service was
`healthy: true`.

`wild status` showed `Container/calibre-web healthy: true` with no
`appHealth` field, beside `immich-server` and `open-webui` reporting
`appHealth: "healthy"` from healthchecks their images declare.

An author adopting an image with no healthcheck is adopting this blind
spot. Say so in the package header, and prefer the Route health gate —
which probes the front door — as the check that means something.

### A7. Two profiles of one capability may both be installed

Resources are owned by `capability/<name>`, so `books-calibre-web` and
`books-kavita` both render resources owned by `capability/books`.
`droppedByUpgrade` — "what this capability owns today that its new
expansion no longer produces" — had no finer key than that owner, so
enabling one profile read the other's Container and Route as leftovers
of an upgrade and proposed deleting them.

Installing one book app therefore uninstalled the other. Nobody decided
that; it fell out of a key that was too coarse. A household comparing
Calibre-Web against Kavita before choosing, or running Jellyfin beside
Plex over one library, is doing something entirely ordinary.

Three changes make it possible:

- **`Metadata.Package` records which package produced a resource**, and
  `droppedByUpgrade` is scoped to it. Upgrade convergence — the thing
  that function exists for — is unchanged; it simply no longer reaches
  into a sibling. Resources written before the field exists carry no
  provenance, and are attributed to the package `packageIsTheInstalledOne`
  identifies, so a legacy deployment upgrades as it always did.
- **`metadata.exclusive: true`** marks the capabilities where two really
  cannot coexist: one identity provider, one reverse proxy owning the
  ports, one resolver on `:53`, one dashboard. Off by default, because
  most household capabilities are plural and were only ever singular by
  accident. Where it is on, the replacement still happens — but the
  deletions appear in the plan, to be read before approval, rather than
  as a side effect of an install nobody read as a removal.
- **A shared resource name must mean the same thing in both profiles.**
  Sharing a name is fine and often right: both book packages declare
  `Secret/restic-books` and `Volume/books-backup-repo` identically, one
  repository for the household's books whichever app reads them.
  Declaring the same name with a *different* spec is not. Both declared
  `BackupPlan/books`, differing in which config volume they backed up,
  so whichever installed second silently redefined the first one's
  backup. Such a resource is named for the profile
  (`BackupPlan/books-calibre-web`), and
  `TestSiblingProfilesDoNotCollide` keeps it that way.

The chat packages had already escaped this by other means — each sets
`capability: chat-telegram-admin`, `chat-telegram-family` and so on, one
capability per package, which is why those four coexist today while the
book apps could not. Their `profile:` field is decorative as a result.
They can be folded back onto a shared `chat` capability now, but doing
so re-owns live resources, so it is left for a deliberate migration
rather than bundled here.

### A8. Removing an app names the app, and never removes data

`wild capability disable <capability>` deleted everything owned by
`capability/<name>`. That was unambiguous only while a capability could
have one profile. With two installed it means three different things,
and it took the most destructive reading.

It now names a PACKAGE, everywhere — including for the capabilities that
only ever have one profile, so one command means one thing. A capability
name is answered with the packages installed under it rather than with a
guess.

Two rules ride with it, and both were learned the hard way while getting
this wrong twice client-side:

- **It never leaves the named package's own capability.** Scoped per
  capability instead, the legacy-attribution rule applied to every
  capability with no stamped resources — so disabling one book app
  proposed deleting Authelia, LLDAP, Caddy, the dashboard and the chat
  adapters. 34 resources, from a command naming one app.
- **It never deletes what a surviving sibling still needs.** Both book
  packages declare `Secret/restic-books`, the restic repository
  password, and whichever installed last owns the stamp on it. A lost
  repository password is not a broken backup, it is an unreadable one.

And the rule `droppedByUpgrade` already had, which this never got: DATA
IS NEVER PROPOSED FOR DELETION. Volumes and SeedFiles belong to the
household (ADR-W004 A4), and the reconciler refuses a Volume Delete
outright — so proposing one never removed data, it only guaranteed the
plan ended in a step that failed every tick until F60 set it aside.
Removing an app is not removing the library it was reading.

The computation lives in Keystone, not the CLI. Answering it needs the
catalog and the recorded install params — what a package DECLARES, not
merely what is installed — and the CLI has neither. Both bugs above were
client-side, and neither was possible to get right there.

### A9. A login box is a claim about credentials, and it has to be answered

`mail-roundcube` is correct. Roundcube and Davis both authenticate
against the household's own IMAP server, so a person has one credential
— their mail password, checked by their provider, stored nowhere here.
The wiring is right: Davis's own `.env` documents `IMAP_AUTH_URL` as
`host[:port]`, which is what the package renders, and every secret the
capability generates materialized correctly.

Both surfaces were still unusable, and for a reason no check here could
see. The household clicked "Mail", met a username and password field,
tried the password they know, and got "invalid credentials". What Gmail
actually said was in a container log nobody was reading:

    AUTHENTICATE PLAIN: Application-specific password required

Gmail, Outlook and Yahoo stopped accepting an account password over IMAP.
The credential is an App Password from the provider, and nothing in the
product said so — the design was written down in a YAML comment, the
dashboard card said "Your email, in a browser", and the install form
asked for an IMAP host without ever naming what people would sign in
with.

So: a package whose sign-in posture is "the app's own login" must say
WHICH credential, where the person meets the login — the dashboard row,
which is the only text between a household member and the box. And where
a common provider refuses the obvious answer, the package says that too,
on the param an operator is filling in at install time.

The related trap, from the same package: `davis-admin-password` is
generated and write-only, so the admin page it protects could not be
opened by anyone, ever. That is a defensible security posture — the page
is closed rather than open with a default, and household members never
need it — but a generated secret nobody can read is a login nobody can
perform, and the param that mentions it now says so and names the
command that sets it.

None of this is checkable by lint. It is the same lesson as A1 in a
different place: what a package claims about the world outside it has to
be verified against that world, and then written where the person who
hits it will read it.

### A10. A secret says what it is for, and setting one takes effect

Write-only is not negotiable (ADR-W012 W2): the protocol has no
GetSecret arm, so a generated value cannot be shown to anyone, ever.
That is a property worth keeping, and it has a consequence the product
has to carry rather than hide — the only way into a service whose
password was generated is to SET it to a value you choose.

Two things made that unreachable without a terminal.

`SecretSpec.Purpose` did not exist, so the Secrets screen could show a
name, a version, and what rotating it would restart, and nothing about
what any of it meant. `davis-admin-password` does not tell a household
admin that it is the calendar service's admin login, that the username
beside it is `admin`, or that the value cannot be read back. Packages
now declare that sentence and the screen shows it, joined from the
package because the node holds values and knows nothing about what they
are for.

And `wild secret set` did not take effect. `recreateDependents` is
documented right beside it — "stops and removes each dependent container
so the level-triggered loop re-creates it against the secret's new
value" — and rotation called it while set did not. A container's
environment is fixed when it is created, so setting the Davis admin
password wrote the value, reported success, and left the service
checking the previous one. A new value nothing reads is not a new value.
Set now propagates exactly as rotate does, reports how many containers
it re-created, and skips the case that must not restart anything: a
generate on a secret that already exists, which is the install-time
"make sure this is here" call.

The general rule: an operator should not need a shell for anything the
household product asks them to do. Where a design forces a step —
write-only forces "set, do not discover" — the surface has to say so at
the point of the step, and the step has to work.

### A11. A secret's readers include the files it is written into

`secretDependents` scanned `Container.Env` and nothing else, which was
the whole answer until a package moved a credential into a managed file.
`photos-immich` did exactly that in this ADR's own A1 fix: Immich does
not read OAuth from the environment, so its client secret now lives only
inside `immich.json`. Rotating it therefore found no dependent, rewrote
nothing, re-created nothing, and reported success while Immich went on
presenting the previous client secret.

A rotation that reports success and changes nothing is worse than one
that fails: it is the state where an operator believes a credential has
been retired.

So a secret's readers are: the containers whose env names it, the
ConfigFiles whose CONTENT names it, and the containers that MOUNT those
files — the last because a file is read at startup, and a rewritten file
behind a running process is the same silence in a different place. The
files are rewritten before the containers are re-created, so a container
never starts against the old value.

**Known gap, not closed here.** An `OidcClient`'s secret reference lives
in the merged Authelia clients file, which is rendered from desired
state and compared by its PRE-materialization content. A rotation
changes the value but not that content, so the comparison matches and
the file is never rewritten — leaving the provider holding the old
digest while the app holds the new secret, which is a login that fails
with both halves internally consistent. This predates the change above
and is not made worse by it, but `wild secret rotate <capability>-oidc`
should be considered unsafe until the reconciler can be told that a
secret version moved.

### A12. What an image REQUIRES is written down beside what it ignores

`inventory-homebox` shipped without `HBOX_AUTH_API_KEY_PEPPER`. Homebox
panics on it before it opens its database:

    panic: auth.api_key_pepper must be set to at least 32 bytes

The Container reconciled, Docker reported it, the capability read as
installed — and the app crash-looped 58 times behind a 502. A package
that cannot start the app it installs is this ADR's own A1 in its
purest form, and it shipped anyway.

`pkgtool.requiredEnv` is the mirror of `inertEnv` (A5), and the two are
the same knowledge from opposite ends. A5 catches configuring an app
through a door it does not have; this catches failing to configure it
through the door it insists on. Neither is derivable from a manifest:
the only source is the app's startup path, read by someone integrating
it, and this is where that reading gets written down.

The check is trivial once the row exists, which is the point. What is
not trivial is noticing — so the pre-write question in Part B gains one:
**what does this app refuse to start without?** Run the image with the
package's environment and nothing else before believing the answer.

### A13. Installing every package is the check nothing else performs

Twenty-three packages were installed on a live node, probed on their own
port and through the front door, and removed again. Eighteen worked.
Five could not start, and each had shipped, linted clean, and reported
itself installed:

| package | fault |
| --- | --- |
| `inventory-homebox` | never set `HBOX_AUTH_API_KEY_PEPPER`; the app panics before opening its database |
| `metrics-prometheus` | ran as `nobody` against a managed config the agent wrote root-only |
| `metrics-grafana` | put its OIDC secret in a config file, which makes that file 0600, which uid 472 cannot read |
| `sync-syncthing` | mounted its volume one level below the directory the image's entrypoint chowns |
| `documents-paperless` | set `POSTGRES_PASSWORD` on the database and never told the app what it was |

Two of those are packages configuring an app through a door it does not
have (A1's shape). Two are permissions. One — paperless — is a package
that could never have worked for anybody: the credential was declared,
materialized and delivered correctly, to one side of a conversation
between two containers.

The sweep also found what no package could: a Route delete asserted its
file read back EMPTY while writing a tombstone comment, so the gate
never passed, the plan halted, and the Container delete after it never
ran. Every removal on every deployment had been leaving a container
behind. Nothing in the catalog could have surfaced that, because it is
not in the catalog.

So: **install it.** Lint is necessary and it is not sufficient; the only
check that catches "this cannot start" is starting it. The harness is
four steps and worth keeping — enable, wait for readiness (a declared
healthcheck, or retries where there is none), probe the container's own
port AND the front door, remove and confirm it went.

Two cautions learned by getting them wrong. Probing at "container
running" tested a Java app three seconds into a forty-second boot and
reported a fault that was not there. And a 302 at the front door is
CORRECT for a proxy-auth route — it is the Authelia redirect — so a
sweep that flags non-200 flags most of the catalog.

---

# Part B — Guidance (not binding, but this is the shape of the work)

## The integration is the part nobody checked

Every failure above lives in the same place: between the package and the
application. The resource model is well covered — that is why every one
of these lints clean. What was uncovered was the claim each package made
*about the app*: that Immich reads `OAUTH_ENABLED`, that Calibre-Web
accepts `CST`, that Open WebUI can be reached at the host the Route
declares.

An AI-assisted session is especially prone to this, and the reason is
worth naming plainly. A model writing a package is fluent in the format
and is pattern-matching on other packages in the catalog. Both of those
produce a package that lints clean. Neither produces a check against the
upstream app, because that requires reading that app's current
documentation and comparing it to what the template says — which is a
different activity, and the one most easily skipped when the output
already looks right.

Where the two disagree, the app wins. It is the thing that runs.

## Before you write the template

Answer these, and put the answers in the package header where the next
author will find them:

1. **How is this app configured?** Environment, a config file, an API
   call, a database row, or its own UI? Name the mechanism. If it is a
   file, that is a `ConfigFile` — see `photos-immich` and
   `cameras-frigate`.
2. **Which of those settings survive a restart, and which are read once
   at install?** A setting stored in the app's database is not
   Wildstone's to state unless a file overrides it.
3. **What does it refuse to start without?** Run the image with exactly
   the environment the package gives it and read what happens. Homebox
   panics on a missing `HBOX_AUTH_API_KEY_PEPPER`; the package shipped
   without it and the crash-loop was invisible behind a 502. Record what
   you find in `pkgtool.requiredEnv`.
4. **Does the image declare a `HEALTHCHECK`?** `docker inspect` answers
   it. If not, say so in the header (A6).
4. **What does this app need to know about its own address?** If
   anything, it is `origin`, never concatenation (A4).
5. **What claims does it need from the identity provider?** Open WebUI
   rejects a login with no `email` claim, which is a fact about Open
   WebUI and also a fact about how the directory must be provisioned.

## Verify against the running thing, not the plan

A green plan is not a working service. The checks that actually
distinguish them, cheapest first:

```sh
# The app answers on its own port — not "the container is Up".
docker exec <proxy> wget -S -O /dev/null http://<container>:<port>/

# The config the app HOLDS, which is not always the config you sent.
docker logs <container> | head -50

# The front door, which is what a person will use.
curl -sk -o /dev/null -w '%{http_code}\n' <the URL the dashboard links to>
```

For an OIDC integration, follow the whole round trip. Discovery,
authorization, consent, token and userinfo can each return 200 and the
login still fail — Open WebUI's did, at the last step, on a claim that
was absent rather than wrong.

## The failure mode to design against

All three services here failed *silently and positively*: the container
was Up, the resources were healthy, the plan was applied, the env vars
were present in `docker inspect`. Every surface Wildstone offered said
the install had worked.

A package that cannot be wrong in a way something notices is a package
that will eventually be wrong in a way nobody notices. When you add an
integration, add the check that would have caught it being wrong.
