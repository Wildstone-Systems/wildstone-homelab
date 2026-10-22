# ADR-W046: The household keeps its knowledge as Markdown

- **Status:** proposed; implemented on this branch (`keystone/knowledge`,
  `keystone/domestic/knowledge.go`, `packages/knowledge-wildstone`,
  `packages/notes-silverbullet`).
- **Issue:** old private issue 367.
- **Builds on:** ADR-W024 A5/A5.1 (household action tiers), ADR-W033 §6
  (tool assignments), ADR-W037 (path refusal rules), ADR-W045 A4 (host
  folders in a BackupPlan).

## Context

Runtime state — events, routines, permissions, audit, Butler memory — is
structured and belongs in the runtime database. Durable, human-readable
knowledge is different: notes, decisions, project context, procedures.
People want to read and edit it in a browser, agents want to read and
keep it, and it should outlive any one app or model vendor.

## Decisions

### A1. The folder is the contract

The knowledge store is one host folder of UTF-8 `.md` files at relative
paths. A note's identity is its path; its revision is the first 16 hex
digits of the SHA-256 of its bytes. Nothing else is authoritative: no
database row, index or sidecar file is needed to read a note. Any search
index or embedding store is **derived** and may be deleted and rebuilt
from the folder. None ships in this ADR (A8).

The runtime database keeps holding runtime state. Nothing in it moves
into Markdown, and nothing in the folder is required to run the house.

### A2. Two packages: the capability and a UI

- `knowledge-wildstone` (capability `knowledge`, exclusive) owns the
  contract. It installs no container. It renders a `knowledge.yaml`
  ConfigFile naming the folder (`knowledge_path`, default
  `/srv/wildstone/knowledge`), which is how Keystone finds it, as with
  the drop box. It declares the agent tools and the folder's backup.
- `notes-silverbullet` (capability `notes`) is the first human UI. It
  bind-mounts the same folder and holds nothing of its own: no Volume,
  no config inside the folder.

The folder is a parameter, never a Volume either package owns. Keystone
never deletes a host folder, so removing either package leaves every
note in place. Removing the UI leaves the agent tools working. Removing
the capability stops the tools at once (the install is checked per
call). A different Markdown client can be pointed at the same folder.

The folder is kept apart from the drop box on purpose. The drop box
promises never to pipe a household document through a model; this
folder exists so an agent can read what is in it.

### A3. Agents get six bounded tools, not the filesystem

Served by Keystone as household actions (ADR-W024 A5), via
`do_household_action`:

| tool | tier | does |
| --- | --- | --- |
| `list_notes` | read | one folder's notes and subfolders |
| `search_notes` | read | case-insensitive match on path or text, with snippets |
| `read_note` | read | one note, in 2.5 KB pieces, with its revision |
| `write_note` | domestic | create a **new** note; refuses if one exists |
| `append_note` | domestic | add a paragraph to the end of an existing note |
| `update_note` | domestic | replace a note's text; must name the revision it read |

There is no delete, move or rename tool. Tidying the folder is a
person's job, in the UI.

Writes are `domestic` rather than `confirm` because none of them can
lose anything (A5). A household rule can therefore keep a log without a
person approving each line.

A single param through `do_household_action` is at most 1 KB. That bound
is shared with every household action and stays; `append_note` exists
so an agent can build a longer note in steps.

### A4. Permissions reuse what exists

- **Which agent may use which tool:** the existing per-Agent tool
  assignments and bundles (ADR-W033 §6). An Owner gives an Agent
  read-only knowledge by assigning only the three read tools. The write
  tools are not opt-in, because they cannot destroy data.
- **Which sessions may use them:** `do_household_action` is itself a
  mutating Butler tool, so a read-only (QA) session reaches none of
  them. Every call is attributed to the session's household member.
- **Whose notes:** v1 is one shared household folder. Everything in it
  is visible to every household agent and to every `family` member who
  can open the UI. Per-person namespaces are a later decision (Q1).

### A5. Guardrails

- **Paths.** Relative only, validated by `portal.ParseRelativePath`:
  refused, never cleaned. `..`, absolute paths, hidden components,
  backslashes and control characters are refused. Every operation goes
  through an `os.Root`. Symlinks are refused at every component, and
  searches do not follow them.
- **Atomic writes.** A new note is staged in its folder, synced, and
  published by an exclusive hard link, so a same-name collision is a
  refusal. An update or append is staged and renamed over the note.
- **Concurrent edits.** `update_note` must name the revision it read. If
  the note changed, the update is refused with the current revision and
  nothing is written. The check runs again just before the rename. An
  append is also refused if the note changed while it was being written.
  The last-writer-wins window that remains is between that final check
  and the rename: microseconds, not an editing session.
- **Undo.** Before an update replaces a note, the old bytes are copied
  to `.history/<note path>/<time>-<revision>.md`. The tools cannot see
  `.history`, and SilverBullet is told to ignore it.
- **Active content.** Some clients run parts of a note: SilverBullet runs
  `space-lua`, `space-script` and `space-style` blocks and `${...}`
  expressions in the reader's browser. Agent-written text containing any
  of them is refused. A fence opener is refused anywhere in a line, so a
  block nested in a quote or list is caught too. The refusal does not
  quote the text, because it lands in the audit stream. So are the files a client treats as configuration
  or runs (`CONTAINER_BOOT.md`, `CONFIG.md`, `SETTINGS.md`, `PLUGS.md` at
  the top level) and any path component starting with `_`.
- **Credentials.** Agent writes that name a credential are refused, as
  `remember` refuses them.
- **Untrusted reads.** Note text, and a note's path (people name notes
  in the UI), reach the model inside the `<untrusted_data>` envelope, because anyone with the UI open can write
  a line that reads like an instruction.
- **Audit.** Each household action already writes one operator event.
  Knowledge tools set `Result.AuditSummary`, so that event (and a
  routine's effect record) holds the tool, the member, the path and the
  new revision, never the note's text. The Agent task ledger keeps its full step record as it does for
  every tool.
- **Execution fence.** Writes check the household execution fence at
  entry and again just before publishing.
- **Ownership.** Keystone runs as root under `UMask=0077`. Notes are
  published `0644` in `0755` folders, owned by the owner of the folder
  they land in. SilverBullet runs as the folder's owner, so both sides
  can edit each other's notes.

### A6. SilverBullet runs in single-space mode behind forward-auth

SilverBullet 2.10+ defaults to multi-space mode, which keeps accounts
and server config in its data folder and starts with a setup wizard.
Mounting the knowledge folder there would mix that config into the
notes. Setting `SB_NAME` selects single-space mode, where the mounted
folder *is* the space. Who may open it is decided by the Route
(`authelia_oidc`) and the `family` access group; there are no
SilverBullet accounts.

- The shell backend is off (`SB_SHELL_BACKEND=off`), and the `-slim`
  image has no Chromium, so the runtime API is off too
  (`SB_RUNTIME_API=0`).
- The Route has no per-path auth bypass, so SilverBullet's
  `/service_worker.js` and `/.client/*` are behind sign-in. It works in a
  signed-in tab, but may not install as an offline app.
- The origin is pinned (`ingressorigin` label `notes`, port 54443,
  which extends the tailnet port block), so the service worker's cache
  never moves.
- The image is pinned by digest (`2.11.1-slim`).

Single-space mode is marked legacy upstream. If it goes away, the
package moves to multi-space with the folder as a space's custom path.
The contract in A1 does not change.

### A7. Backup and restore

`knowledge-wildstone` ships a daily, `critical` BackupPlan over the
folder as a host path (ADR-W045 A4), into a local restic repository
Volume. `.history` is included, so a restore brings back the undo trail.
The Owner can route it off-site like any other plan.

To restore, follow `docs/recovery.md`:

1. Run `wild backup restore knowledge`. The latest snapshot lands
   alongside, in the `knowledge-restore` volume, and nothing live is
   overwritten.
2. Stop anything that edits the folder: remove `notes-silverbullet`, or
   stop its container.
3. Copy the restored tree over the folder, keeping the folder's owner.
4. Start it again.

A UI's own index rebuilds from the files.

### A8. Answers to the issue's open questions

1. **Shared vault or per-user?** One shared household folder for v1.
   Per-person folders need the same member-scoped resolution the drop
   box's "Mine" has, and a decision about agent access to them, so they
   are left for later.
2. **Write permissions:** tiers and the existing per-Agent tool
   assignments (A4). No new permission system.
3. **Versioning:** `.history` copies plus the operator event stream,
   not Git. Git would add a second writer to the folder, and a repo
   inside it would be exposed to every client that reads it. A Git sync
   UI can be layered on later without changing A1.
4. **Semantic indexing:** later. When it comes, it is derived data under
   Keystone's data dir, never in the folder, and rebuildable (A1).
5. **Concurrent edits:** optimistic revisions (A5). Agents never
   overwrite a change they have not read. A person editing in the UI
   always wins.

## Consequences

- The household gets notes that outlive every app and model in front of
  them, with a browser editor on day one.
- Agent notes are small by construction: 1 KB per call, with appends to
  grow them.
- A household member with the UI can still author scripts a later
  reader runs. That is SilverBullet's documented trust model for a
  "fully-trusted" household (its Security Profiles page). Agents cannot
  do it.
