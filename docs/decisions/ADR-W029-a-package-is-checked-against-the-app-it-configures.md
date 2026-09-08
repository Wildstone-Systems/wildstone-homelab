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
3. **Does the image declare a `HEALTHCHECK`?** `docker inspect` answers
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
