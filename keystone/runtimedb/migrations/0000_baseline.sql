-- Baseline: the runtime schema as migrations 0001 through 0104 left it,
-- squashed into one file (ADR-W007 A4, "Baseline"). A new database runs
-- this file and then any later migration. A database that already ran
-- every one of those migrations is marked as having run this file instead
-- (see baseline.go), so its schema and rows are untouched.
--
-- Generated once by building a database through the old chain and
-- writing out its schema and its seed rows. Like any applied migration it
-- is never edited: a change to the schema is a new migration after it.

CREATE TABLE events (
    seq      INTEGER PRIMARY KEY AUTOINCREMENT,
    time     TEXT NOT NULL,
    type     TEXT NOT NULL,
    node     TEXT NOT NULL DEFAULT '',
    kind     TEXT NOT NULL DEFAULT '',
    name     TEXT NOT NULL DEFAULT '',
    message  TEXT NOT NULL DEFAULT '',
    attrs    TEXT NOT NULL DEFAULT '{}'  -- JSON object
);

CREATE TABLE plans (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    principal  TEXT NOT NULL,
    acting_for TEXT NOT NULL DEFAULT '',
    summary    TEXT NOT NULL,
    risk       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN
        ('proposed', 'approved', 'applying', 'applied',
         'failed', 'rejected', 'superseded')),
    plan_json   TEXT NOT NULL,
    resolved_by TEXT NOT NULL DEFAULT '',  -- principal that approved/rejected
    resolved_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE audit (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    time       TEXT NOT NULL,
    principal  TEXT NOT NULL,
    acting_for TEXT NOT NULL DEFAULT '',
    method     TEXT NOT NULL,
    path       TEXT NOT NULL,
    status     INTEGER NOT NULL,
    remote     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE statefile_head (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    head TEXT NOT NULL
);

CREATE TABLE observed (
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    node       TEXT NOT NULL DEFAULT '',
    healthy    INTEGER NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL, awaiting_approval BOOLEAN NOT NULL DEFAULT 0, app_health TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kind, name)
);

CREATE TABLE backup_runs (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_name   TEXT NOT NULL,             -- BackupPlan metadata.name
    run_kind    TEXT NOT NULL,             -- 'backup' | 'drill' | 'restore'
    plan_id     TEXT NOT NULL DEFAULT '',  -- the plans-table record for this run
    started_at  TEXT NOT NULL,             -- RFC3339Nano UTC
    finished_at TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,             -- 'running' | 'succeeded' | 'failed'
    detail      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE butler_sessions (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at      TEXT NOT NULL,       -- RFC3339Nano UTC
    principal       TEXT NOT NULL,       -- always "butler"
    acting_for      TEXT NOT NULL,       -- the authenticated caller
    provider        TEXT NOT NULL,       -- provider name, or "" if none ran
    mode            TEXT NOT NULL,       -- 'full' | 'qa-only'
    final_text      TEXT NOT NULL,
    capped          INTEGER NOT NULL DEFAULT 0,
    transcript_json TEXT NOT NULL
, surface TEXT NOT NULL DEFAULT 'admin', butler TEXT NOT NULL DEFAULT '', revoked_at TEXT NOT NULL DEFAULT '', revoked_by TEXT NOT NULL DEFAULT '', person_id TEXT REFERENCES butler_members(person_id), agent_id INTEGER REFERENCES butlers(seq));

CREATE TABLE nodes (
    node_id         TEXT PRIMARY KEY,
    first_joined_at TEXT NOT NULL,             -- RFC3339Nano UTC
    last_seen_at    TEXT NOT NULL,             -- last session open or close
    agent_version   TEXT NOT NULL DEFAULT '',  -- from the agent's Hello
    protocol_minor  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE household_events (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        TEXT NOT NULL,             -- publisher-supplied
    occurred_at     TEXT NOT NULL,             -- RFC3339Nano UTC, per the publisher
    recorded_at     TEXT NOT NULL,             -- RFC3339Nano UTC, stamped here
    subject         TEXT NOT NULL DEFAULT '',  -- member / device / household id
    source          TEXT NOT NULL,             -- the publishing principal
    type            TEXT NOT NULL,             -- e.g. person.left_home
    entity          TEXT NOT NULL DEFAULT '',  -- optional affected thing
    payload         TEXT NOT NULL DEFAULT '{}',-- small JSON object
    correlation_id  TEXT NOT NULL DEFAULT '',
    schema_version  INTEGER NOT NULL DEFAULT 1,
    UNIQUE (source, event_id)
);

CREATE TABLE automation_cursor (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    seq  INTEGER NOT NULL
);

CREATE TABLE butler_config (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    provider    TEXT    NOT NULL,
    model       TEXT    NOT NULL DEFAULT '',
    endpoint    TEXT    NOT NULL DEFAULT '',
    -- The probe verdict for THIS provider and model (A9). Recorded with
    -- the config it describes, so a degraded mode means "this model did
    -- not pass" rather than "something did not pass, once, at boot".
    probe_mode  TEXT    NOT NULL DEFAULT '',
    probe_detail TEXT   NOT NULL DEFAULT '',
    probed_at   TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL,
    updated_by  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE chat_inbound (
    dedupe_key TEXT PRIMARY KEY,
    surface    TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    claimed_at TEXT NOT NULL,
    queued_seq INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE chat_adapters (
    capability   TEXT PRIMARY KEY,
    transport    TEXT NOT NULL,
    surface      TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    handle       TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    seen_at      TEXT NOT NULL
);

CREATE TABLE butler_session_turns (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    session    INTEGER NOT NULL REFERENCES butler_sessions(seq),
    ordinal    INTEGER NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    text       TEXT NOT NULL,
    created_at TEXT NOT NULL,           -- RFC3339Nano UTC
    capped     INTEGER NOT NULL DEFAULT 0,
    tool_json  TEXT NOT NULL DEFAULT '{}',
    plan_ids   TEXT NOT NULL DEFAULT '' -- comma-separated plan record ids
, source TEXT NOT NULL DEFAULT '' CHECK (source IN ('', 'voice')));

CREATE TABLE butler_task_steps (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    ordinal          INTEGER NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('model', 'tool', 'wait', 'delivery')),
    name             TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('proposed', 'running', 'verified', 'failed', 'skipped')),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    params_json      TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL
);

CREATE TABLE butler_task_attempts (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    step             INTEGER NOT NULL REFERENCES butler_task_steps(seq),
    attempt          INTEGER NOT NULL,
    epoch            INTEGER NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('dispatched', 'succeeded', 'failed', 'stalled', 'fenced', 'cancelled')),
    started_at       TEXT NOT NULL,
    ended_at         TEXT NOT NULL DEFAULT '',
    -- Post-redaction result only, exactly as butler_sessions requires of
    -- its transcript. Nothing pre-Dispatch reaches this column.
    result_json      TEXT NOT NULL DEFAULT '{}',
    error_text       TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL
);

CREATE TABLE butler_task_transitions (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    task       INTEGER NOT NULL REFERENCES butler_tasks(seq),
    at         TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE coordinator_lease (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    holder      TEXT NOT NULL,
    epoch       INTEGER NOT NULL,
    acquired_at TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL,
    -- promoted_from records who was fenced, so a recovery has a written
    -- answer to "what happened to the old one".
    promoted_from TEXT NOT NULL DEFAULT ''
);

CREATE TABLE butler_runtime_settings (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    household_rounds  INTEGER NOT NULL,
    operator_rounds   INTEGER NOT NULL,
    -- Aggregate tool calls per task, across every slice and restart.
    max_tool_calls    INTEGER NOT NULL,
    -- How long one model call may hang before the attempt is fenced and
    -- retried. A stall is not a failure and not a hang: it is a deadline.
    stall_seconds     INTEGER NOT NULL,
    -- How many times one stalled or failed model call is retried inside
    -- the remaining budget before a person is told.
    retry_limit       INTEGER NOT NULL,
    -- How long a recorded observation may be reused on a resume before
    -- it is re-read instead. "Do not repeat already-verified effects"
    -- and "refresh stale observations" are both A4; this number is where
    -- they meet.
    observation_ttl_seconds INTEGER NOT NULL,
    -- Quiet hours in the household timezone, as minutes from midnight.
    -- Equal values mean no quiet hours. A window that wraps midnight is
    -- normal and is what a household actually configures.
    quiet_start_min   INTEGER NOT NULL,
    quiet_end_min     INTEGER NOT NULL,
    timezone          TEXT NOT NULL DEFAULT 'UTC',
    updated_at        TEXT NOT NULL,
    updated_by        TEXT NOT NULL DEFAULT '', session_recency_seconds INTEGER NOT NULL DEFAULT 1800
    CHECK (session_recency_seconds BETWEEN 60 AND 604800),

    CHECK (household_rounds BETWEEN 1 AND 64),
    CHECK (operator_rounds BETWEEN 1 AND 64),
    CHECK (max_tool_calls BETWEEN 1 AND 512),
    CHECK (stall_seconds BETWEEN 5 AND 900),
    CHECK (retry_limit BETWEEN 0 AND 10),
    CHECK (observation_ttl_seconds BETWEEN 0 AND 86400),
    CHECK (quiet_start_min BETWEEN 0 AND 1439),
    CHECK (quiet_end_min BETWEEN 0 AND 1439)
);

CREATE TABLE butler_task_feedback (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    task       INTEGER NOT NULL REFERENCES butler_tasks(seq),
    trace      INTEGER REFERENCES butler_work_trace(seq),
    created_at TEXT NOT NULL,
    -- author is the person, never the Butler. There is no code path
    -- that writes this row on a model's behalf.
    author     TEXT NOT NULL,
    label      TEXT NOT NULL CHECK (label IN (
                   'wrong-answer', 'stale-read', 'repeated-work',
                   'inappropriate-tool', 'permission-refusal',
                   'excessive-interruption', 'useful-completion')),
    note       TEXT NOT NULL DEFAULT '',
    CHECK (author <> '')
);

CREATE TABLE butler_evaluations (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at       TEXT NOT NULL,
    author           TEXT NOT NULL,
    name             TEXT NOT NULL,
    -- source_task is where it came from. Nullable because an evaluation
    -- outlives the task's retention: deleting the conversation must not
    -- delete the lesson learned from it.
    source_task      INTEGER,
    summary          TEXT NOT NULL,
    expected         TEXT NOT NULL,
    -- The versions in force when the original behaviour happened.
    prompt_version   TEXT NOT NULL DEFAULT '',
    policy_version   TEXT NOT NULL DEFAULT '',
    manifest_version TEXT NOT NULL DEFAULT '',
    model_endpoint   TEXT NOT NULL DEFAULT '',
    evaluator_version TEXT NOT NULL DEFAULT '',
    fixture_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (name <> ''),
    CHECK (expected <> ''),
    CHECK (author <> '')
);

CREATE TABLE butler_debug_capture (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    enabled_by TEXT NOT NULL,
    -- scope is one task id, so capture is never deployment-wide.
    task       INTEGER NOT NULL REFERENCES butler_tasks(seq),
    reason     TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    CHECK (enabled_by <> ''),
    CHECK (reason <> '')
);

CREATE TABLE butler_conversation_sessions (
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    session      INTEGER NOT NULL REFERENCES butler_sessions(seq),
    attached_at  TEXT NOT NULL,
    PRIMARY KEY (conversation, session)
);

CREATE TABLE butler_memory_derivations (
    derivative INTEGER NOT NULL REFERENCES butler_memory(seq),
    source     INTEGER NOT NULL REFERENCES butler_memory(seq),
    PRIMARY KEY (derivative, source)
);

CREATE TABLE butler_memory_suppressed_sources (
    source_kind  TEXT NOT NULL,
    source_id    TEXT NOT NULL,
    -- content_key is a normalized form of what was forgotten, so
    -- suppressing one fact from a long conversation does not suppress
    -- the whole conversation.
    content_key  TEXT NOT NULL,
    suppressed_at TEXT NOT NULL,
    suppressed_by TEXT NOT NULL,
    PRIMARY KEY (source_kind, source_id, content_key)
);

CREATE TABLE butler_memory_digest (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    memory     INTEGER NOT NULL REFERENCES butler_memory(seq),
    change     TEXT NOT NULL CHECK (change IN ('recorded', 'corrected', 'forgotten')),
    summary    TEXT NOT NULL,
    actor      TEXT NOT NULL,
    at         TEXT NOT NULL,
    -- immediate marks a change the household must hear about now rather
    -- than in the next digest — the safety-relevant removals of A1.
    immediate  INTEGER NOT NULL DEFAULT 0,
    delivered_at TEXT NOT NULL DEFAULT '',
    CHECK (summary <> ''),
    CHECK (actor <> '')
);

CREATE VIRTUAL TABLE butler_index_fts USING fts5(
    content,
    content='butler_index',
    content_rowid='seq'
);

CREATE TABLE butler_index_measurements (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    at           TEXT NOT NULL,
    fixture      TEXT NOT NULL,
    strategy     TEXT NOT NULL CHECK (strategy IN ('keyword', 'vector', 'hybrid')),
    queries      INTEGER NOT NULL,
    hits         INTEGER NOT NULL,
    -- recall_pct and p95_micros are what a person actually compares.
    recall_pct   INTEGER NOT NULL,
    p95_micros   INTEGER NOT NULL,
    index_bytes  INTEGER NOT NULL DEFAULT 0,
    note         TEXT NOT NULL DEFAULT '',
    CHECK (fixture <> '')
);

CREATE TABLE butler_index_jobs (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL CHECK (kind IN ('index', 'compact', 'reindex')),
    payload      TEXT NOT NULL DEFAULT '{}',
    state        TEXT NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),
    -- priority is lower-is-sooner, and background work starts at 100 so
    -- anything interactive can be queued in front of it without
    -- renumbering.
    priority     INTEGER NOT NULL DEFAULT 100,
    created_at   TEXT NOT NULL,
    started_at   TEXT NOT NULL DEFAULT '',
    finished_at  TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE integration_principals (
    principal  TEXT NOT NULL,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    bound_at   TEXT NOT NULL,
    bound_by   TEXT NOT NULL, node TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (principal, connection)
);

CREATE TABLE integration_capabilities (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    kind       TEXT NOT NULL CHECK (kind IN ('event', 'query', 'action')),
    name       TEXT NOT NULL,
    version    TEXT NOT NULL DEFAULT '',
    -- available is false when the app is down or the granted access no
    -- longer covers it. Dependent work then waits rather than failing.
    available  INTEGER NOT NULL DEFAULT 1,
    verified_at TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    CHECK (name <> '')
);

CREATE TABLE integration_work (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    connection  INTEGER NOT NULL REFERENCES integration_connections(seq),
    kind        TEXT NOT NULL CHECK (kind IN ('query', 'action', 'subscribe')),
    capability  TEXT NOT NULL,
    -- correlation_id ties the request to the task or routine that asked,
    -- so a result can be matched without the adapter knowing what a task
    -- is.
    correlation_id TEXT NOT NULL DEFAULT '',
    -- request_json is a versioned envelope. The adapter validates it
    -- against its own contract; Keystone carries it.
    request_json TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,

    state       TEXT NOT NULL CHECK (state IN ('queued', 'claimed', 'done', 'cancelled', 'expired')),
    -- epoch is the connection's lease at enqueue time. A result carrying
    -- an older epoch is refused.
    epoch       INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    -- deadline_at is when this stops being worth answering. An
    -- interactive query defaults to five seconds end to end including
    -- queue delay (A5.1), which is why the deadline is absolute rather
    -- than a duration the adapter starts counting on receipt.
    deadline_at TEXT NOT NULL,
    claimed_at  TEXT NOT NULL DEFAULT '',
    claimed_by  TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT '',

    -- The four result types are the contract's whole point. A valid
    -- EMPTY answer ("the list has nothing on it") is not the same as
    -- UNAVAILABLE ("the app is down") or STALE ("this is what it said an
    -- hour ago"), and a caller that cannot tell them apart will report
    -- an outage as an empty shopping list.
    result_type TEXT NOT NULL DEFAULT '' CHECK (result_type IN ('', 'ok', 'empty', 'unavailable', 'stale')),
    result_json TEXT NOT NULL DEFAULT '{}',
    result_at   TEXT NOT NULL DEFAULT '',
    observed_at TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',

    CHECK (capability <> '')
);

CREATE TABLE "principals" (
    name       TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'butler', 'publisher', 'concierge', 'valet', 'integration-adapter')),
    acting_for TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE "tokens" (
    hash       TEXT PRIMARY KEY,
    principal  TEXT NOT NULL REFERENCES "principals"(name),
    acting_for TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_used  TEXT NOT NULL DEFAULT '',
    revoked_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE integration_placements (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    node       TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('primary', 'fallback')),
    -- rank orders the fallbacks. The primary is rank 0.
    rank       INTEGER NOT NULL DEFAULT 0,

    -- The three facts that together make a node eligible. Each is
    -- recorded separately because each fails separately, and a node that
    -- has a container but no secret is a different problem from one with
    -- a secret and no container.
    container_running   INTEGER NOT NULL DEFAULT 0,
    secret_provisioned  INTEGER NOT NULL DEFAULT 0,
    readiness_verified  INTEGER NOT NULL DEFAULT 0,

    -- healthy_since supports the stabilization period: automatic
    -- failback waits for a node to be healthy for a while, so a
    -- flapping primary does not drag the lease back and forth.
    healthy_since TEXT NOT NULL DEFAULT '',
    last_health   TEXT NOT NULL DEFAULT '',
    -- holder marks the node currently holding the lease. Exactly one per
    -- connection, enforced by the partial unique index below.
    holder     INTEGER NOT NULL DEFAULT 0,

    added_at   TEXT NOT NULL,
    added_by   TEXT NOT NULL,
    -- Removal is not a DELETE. It is fenced, then proven: the container
    -- stopped and removed, the local secret erased, and where the remote
    -- service permits it, the node's credential revoked. Until all of
    -- those are proven the row stays, marked removing, and the
    -- connection reports degraded — because a credential that might
    -- still exist on a machine you have stopped watching is worse than
    -- one you know about.
    removing_at        TEXT NOT NULL DEFAULT '',
    container_removed  INTEGER NOT NULL DEFAULT 0,
    secret_erased      INTEGER NOT NULL DEFAULT 0,
    credential_revoked INTEGER NOT NULL DEFAULT 0,

    CHECK (node <> ''),
    CHECK (added_by <> '')
);

CREATE TABLE integration_secrets (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    -- member scopes a PERSONAL credential — an IMAP or CalDAV account
    -- that belongs to one person, onboarded through the authenticated
    -- secret-input surface and never collected through chat (A5.1).
    -- Empty means a household-level connection credential.
    member     TEXT NOT NULL DEFAULT '',
    -- secret_ref is the name in the secret path. A reference, resolved
    -- on the node into the adapter's scoped environment; it never
    -- travels through a work or result payload and never reaches a
    -- model.
    secret_ref TEXT NOT NULL,
    -- read_only records whether the upstream credential is genuinely
    -- read-only. Where a provider cannot issue one, adapter operations
    -- are enforced read-only and the credential's wider power is
    -- DISCLOSED rather than quietly accepted (A5.1).
    upstream_read_only INTEGER NOT NULL DEFAULT 0,
    disclosure TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '', need TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 0, login TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'recorded'
    CHECK (state IN ('recorded', 'staged', 'active', 'superseded', 'revoked', 'failed')),

    CHECK (secret_ref <> ''),
    CHECK (created_by <> ''),
    -- A credential that is not read-only upstream must say so. An
    -- undisclosed wider power is the thing a person would have wanted to
    -- know before handing it over.
    CHECK (upstream_read_only = 1 OR disclosure <> '')
);

CREATE TABLE household_routines (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    -- ownership is the distinction above and it is immutable: a native
    -- routine cannot become a Wildstone one by editing a column,
    -- because that would silently move responsibility for running it.
    ownership   TEXT NOT NULL CHECK (ownership IN ('native', 'wildstone')),
    -- external_id is the application's own identifier for a native
    -- routine, and empty for a Wildstone one. It is how "this is the
    -- same automation you can see in Home Assistant" is expressible.
    external_id TEXT NOT NULL DEFAULT '',
    connection  TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL,
    -- revision increments on every accepted edit. A review token names
    -- one, so approving a proposal that has since been edited is
    -- refused rather than approving something nobody read.
    revision    INTEGER NOT NULL DEFAULT 1,
    state       TEXT NOT NULL CHECK (state IN ('draft', 'trial', 'enabled', 'disabled')),
    -- document is the routine itself: the YAML a household wrote.
    document    TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL,
    -- disabled_at is set the moment someone disables it, and the
    -- runner checks it before every step. "Immediate and exact" means
    -- exactly this routine stops, now, without waiting for a reload.
    disabled_at TEXT NOT NULL DEFAULT '',
    disabled_by TEXT NOT NULL DEFAULT '', butler TEXT NOT NULL DEFAULT '', output_audience TEXT REFERENCES audiences(audience_id), approved_revision INTEGER NOT NULL DEFAULT 0, resource_signature TEXT NOT NULL DEFAULT '', revoked_at TEXT NOT NULL DEFAULT '', revoked_revision INTEGER NOT NULL DEFAULT 0, deleted_at TEXT NOT NULL DEFAULT '', paused_reason TEXT NOT NULL DEFAULT '', purged_at TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (author <> ''),
    CHECK (document <> ''),
    -- A native routine has an application and an id; a Wildstone one has
    -- neither, and mixing them is the confusion this table exists to
    -- prevent.
    CHECK ((ownership = 'native' AND external_id <> '' AND connection <> '')
        OR (ownership = 'wildstone' AND external_id = '' AND connection = ''))
);

CREATE TABLE household_routine_revisions (
    routine    INTEGER NOT NULL REFERENCES household_routines(seq),
    revision   INTEGER NOT NULL,
    document   TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (routine, revision)
);

CREATE TABLE "models" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    -- kind is the boundary. A route is local — running on hardware the
    -- household owns — or cloud, meaning material sent to it leaves the
    -- house. There is no third answer, and a route cannot change kind:
    -- that would silently move the boundary.
    kind       TEXT NOT NULL CHECK (kind IN ('local', 'cloud')),
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    endpoint   TEXT NOT NULL DEFAULT '',
    -- readiness is the probe verdict for THIS route (ADR-W017 A4/A9),
    -- recorded per route rather than per deployment, because a
    -- deployment with a good cloud model and a weak local one has two
    -- different answers.
    readiness  TEXT NOT NULL CHECK (readiness IN ('unprobed', 'ready', 'qa-only', 'unavailable')),
    readiness_detail TEXT NOT NULL DEFAULT '',
    probed_at  TEXT NOT NULL DEFAULT '',
    -- max_concurrency bounds in-flight calls. A local model on a small
    -- box answers one question well and four badly.
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (provider <> '')
);

CREATE TABLE member_notice_preferences (
    member          TEXT PRIMARY KEY,
    -- Minutes from midnight in the member's own timezone. Equal values
    -- mean this person has no quiet hours of their own and follows the
    -- household's.
    quiet_start_min INTEGER NOT NULL DEFAULT 0,
    quiet_end_min   INTEGER NOT NULL DEFAULT 0,
    timezone        TEXT NOT NULL DEFAULT '',
    -- urgent_override lets a person say "wake me for anything marked
    -- urgent". Off by default: an urgent exception that every notice can
    -- claim is not an exception.
    urgent_override INTEGER NOT NULL DEFAULT 1,
    -- min_relevance drops notices below a threshold entirely. A person
    -- who only wants to hear about problems sets it high.
    min_relevance   INTEGER NOT NULL DEFAULT 0 CHECK (min_relevance BETWEEN 0 AND 100),
    updated_at      TEXT NOT NULL,

    CHECK (quiet_start_min BETWEEN 0 AND 1439),
    CHECK (quiet_end_min BETWEEN 0 AND 1439)
);

CREATE TABLE notice_kinds (
    kind        TEXT PRIMARY KEY,
    urgent      INTEGER NOT NULL DEFAULT 0,
    -- relevance is this kind's baseline importance, 0 to 100, checked
    -- against a person's threshold.
    relevance   INTEGER NOT NULL DEFAULT 50 CHECK (relevance BETWEEN 0 AND 100),
    description TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL,
    updated_by  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE deletion_journal (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- subject_kind names WHAT was removed, so reconciliation can replay
    -- it against the right store without the journal knowing their
    -- schemas.
    subject_kind TEXT NOT NULL CHECK (subject_kind IN (
                     'memory', 'member', 'link', 'connection', 'routine',
                     'grant', 'binding', 'task', 'notice', 'secret', 'index-source')),
    subject_id TEXT NOT NULL,
    action     TEXT NOT NULL CHECK (action IN ('deleted', 'revoked', 'offboarded', 'forgotten')),
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',

    CHECK (subject_id <> ''),
    CHECK (actor <> '')
);

CREATE TABLE restore_state (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    quarantined    INTEGER NOT NULL DEFAULT 0,
    entered_at     TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL DEFAULT '',
    -- snapshot_at is when the restored data was taken. Everything
    -- decided after it is a decision this database does not have, which
    -- is the whole reason for the journal.
    snapshot_at    TEXT NOT NULL DEFAULT '',
    -- journal_current records an administrator's assertion that the
    -- deletion journal has been brought up to date from a source newer
    -- than the snapshot. It is a precondition for release and NOT
    -- evidence on its own — A10 is explicit that acknowledgement alone
    -- does not recover missing deletions — which is why released_at also
    -- requires the replay below to have run.
    journal_current INTEGER NOT NULL DEFAULT 0,
    journal_asserted_by TEXT NOT NULL DEFAULT '',
    replayed_at    TEXT NOT NULL DEFAULT '',
    replayed_count INTEGER NOT NULL DEFAULT 0,
    released_at    TEXT NOT NULL DEFAULT '',
    released_by    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE butler_tool_scope (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    tool       TEXT NOT NULL,
    -- The only value. Written out rather than implied so a reader of
    -- this table sees the rule without going to find the resolver.
    state      TEXT NOT NULL DEFAULT 'disabled' CHECK (state = 'disabled'),
    reason     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,

    CHECK (tool <> ''),
    CHECK (updated_by <> '')
);

CREATE TABLE butler_connection_scope (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',

    CHECK (granted_by <> '')
);

CREATE TABLE butler_addresses (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    butler      INTEGER NOT NULL REFERENCES butlers(seq),
    transport   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    -- label is for a person reading the list: "the gym bot",
    -- "#nutrition". The system never matches on it.
    label       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (transport <> ''),
    CHECK (external_id <> ''),
    CHECK (created_by <> '')
);

CREATE TABLE butler_pool_disclosures (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    memory      INTEGER NOT NULL,
    -- butlers and models are recorded as text, as they were AT THE
    -- TIME. A join would answer today's question; this answers the one
    -- that was actually decided.
    butlers     TEXT NOT NULL,
    left_house  INTEGER NOT NULL DEFAULT 0,
    promoted_at TEXT NOT NULL,
    promoted_by TEXT NOT NULL,

    CHECK (promoted_by <> '')
);

CREATE TABLE household_banner (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    image       BLOB    NOT NULL,
    media_type  TEXT    NOT NULL,
    -- How the picture is framed, as CSS object-position percentages.
    -- A photograph is arbitrary and a banner is a wide, short crop of
    -- it, so "which part of it do you want to see" is a real question
    -- and 50/50 is only a guess at the answer.
    position_x  REAL    NOT NULL DEFAULT 50,
    position_y  REAL    NOT NULL DEFAULT 50,
    -- Who last changed it, so the household can tell.
    set_by      TEXT    NOT NULL DEFAULT '',
    updated_at  TEXT    NOT NULL
);

CREATE TABLE butler_role_changes (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    -- The subject, by identity and by the handle they had at the time.
    -- Both, because person_id is what still resolves later and the
    -- handle is what the person reading the audit will recognise.
    person_id  TEXT NOT NULL,
    member     TEXT NOT NULL,
    -- actor is the administrator who made the change.
    actor      TEXT NOT NULL,
    old_role   TEXT NOT NULL CHECK (old_role IN ('owner', 'member')),
    new_role   TEXT NOT NULL CHECK (new_role IN ('owner', 'member')),

    CHECK (person_id <> ''),
    CHECK (actor <> ''),
    -- A row that records no change is noise in an audit trail.
    CHECK (old_role <> new_role)
);

CREATE TABLE "butler_access" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    -- person_id replaces member. The grant follows the person, so a
    -- rename no longer detaches somebody from the Butlers they were
    -- admitted to.
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    granted_at TEXT NOT NULL,
    -- granted_by and revoked_by are actor columns and keep their
    -- recorded text: a Console caller's handle, or a machine
    -- principal's name. See the header.
    granted_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',

    CHECK (person_id <> ''),
    CHECK (granted_by <> '')
);

CREATE TABLE "butler_members" (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id    TEXT NOT NULL UNIQUE,
    member       TEXT NOT NULL UNIQUE,
    role         TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    profile      TEXT CHECK (profile IS NULL OR profile IN ('child')),
    admitted     INTEGER NOT NULL DEFAULT 0,
    generation   INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    admitted_at  TEXT NOT NULL DEFAULT '',
    admitted_by  TEXT NOT NULL DEFAULT '',
    disabled_at  TEXT NOT NULL DEFAULT '',
    disabled_by  TEXT NOT NULL DEFAULT '',
    offboarded_at TEXT NOT NULL DEFAULT '',
    offboarded_by TEXT NOT NULL DEFAULT '',
    -- The responsible adult, by identity. Self-referencing: a guardian
    -- is a Person in this same table.
    guardian_person_id TEXT REFERENCES butler_members(person_id),

    CHECK (member <> ''),
    CHECK (person_id <> ''),
    CHECK (NOT (profile = 'child' AND admitted = 1)),
    CHECK (profile <> 'child' OR guardian_person_id IS NOT NULL),
    CHECK (profile <> 'child' OR role = 'member')
);

CREATE TABLE "butler_identity_links" (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    link_type   TEXT NOT NULL CHECK (link_type IN ('api-principal', 'chat-binding', 'trusted-ingress')),
    -- external_id is the principal name, the chat binding id rendered as
    -- text, or the ingress subject. Stored as text so one column serves
    -- all three without a nullable pair.
    external_id TEXT NOT NULL,
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    -- generation is the member generation this link was created under.
    -- A link whose generation is behind the member's is stale and
    -- resolves to nothing.
    generation  INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    -- created_by is the administrator who approved it. Butler cannot
    -- grant itself manage authority (A2.1), and there is no code path
    -- that writes this row with the Butler as the creator.
    created_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (external_id <> ''),
    CHECK (created_by <> '')
);

CREATE TABLE "butler_conversations" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at TEXT NOT NULL,
    subject    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE "butler_handoffs" (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    from_surface TEXT NOT NULL CHECK (from_surface IN ('admin', 'family')),
    to_surface   TEXT NOT NULL CHECK (to_surface IN ('admin', 'family')),
    from_session INTEGER REFERENCES butler_sessions(seq),
    at           TEXT NOT NULL,
    -- carried is how many turns the destination was given after
    -- filtering. Recorded because "no privileged context crossed" is a
    -- claim, and a claim with a number behind it can be checked.
    carried      INTEGER NOT NULL DEFAULT 0,
    withheld     INTEGER NOT NULL DEFAULT 0,

    CHECK (from_surface <> to_surface)
);

CREATE TABLE "household_review_tokens" (
    hash       TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL,
    routine    INTEGER REFERENCES household_routines(seq),
    revision   INTEGER NOT NULL DEFAULT 0,
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NOT NULL DEFAULT '',
    used_by    TEXT NOT NULL DEFAULT '',

    CHECK (plan_id <> ''),
    CHECK (person_id <> ''),
    CHECK (expires_at <> '')
);

CREATE TABLE "offboarding_records" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    store      TEXT NOT NULL,
    affected   INTEGER NOT NULL DEFAULT 0,
    detail     TEXT NOT NULL DEFAULT '',

    CHECK (person_id <> ''),
    CHECK (actor <> ''),
    CHECK (store <> '')
);

CREATE TABLE "notices" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    recipient_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    routed_to_admin INTEGER NOT NULL DEFAULT 0,
    correlation_id TEXT NOT NULL DEFAULT '',
    outbound   INTEGER,
    dedupe_key TEXT NOT NULL,
    urgent     INTEGER NOT NULL DEFAULT 0,
    relevance  INTEGER NOT NULL DEFAULT 50,
    created_at TEXT NOT NULL,
    hold_until TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,

    delivered_at    TEXT NOT NULL DEFAULT '',
    acknowledged_at TEXT NOT NULL DEFAULT '',
    undelivered_at  TEXT NOT NULL DEFAULT '',

    asks       INTEGER NOT NULL DEFAULT 0,
    answered_at TEXT NOT NULL DEFAULT '',
    answer      TEXT NOT NULL DEFAULT '',
    answered_by TEXT NOT NULL DEFAULT '',
    -- Added by 0040: a notice that ASKS carries what it asked, because
    -- consent to an unnamed action is not consent. A rebuild that
    -- forgot it would put that back.
    question   TEXT NOT NULL DEFAULT '',

    CHECK (kind <> ''),
    CHECK (recipient_person_id <> ''),
    CHECK (dedupe_key <> ''),
    CHECK (answered_at = '' OR asks = 1)
);

CREATE TABLE "chat_bindings" (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The Connection this admission is under (B7, B9). One Agent, one
    -- transport, one bot.
    connection   INTEGER NOT NULL REFERENCES butler_addresses(seq),
    account      TEXT NOT NULL,          -- the transport's own user id
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    display_name TEXT NOT NULL DEFAULT '', -- what the transport calls them, for the operator's list
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,          -- who issued the code this redeemed
    last_used_at TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT NOT NULL DEFAULT '',
    conversation TEXT NOT NULL DEFAULT ''
);

CREATE TABLE "chat_claim_codes" (
    hash        TEXT PRIMARY KEY,
    connection  INTEGER NOT NULL REFERENCES butler_addresses(seq),
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT NOT NULL DEFAULT '',
    binding_id  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE "chat_threads" (
    connection   INTEGER NOT NULL REFERENCES butler_addresses(seq),
    conversation TEXT NOT NULL,
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    session_id   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,   -- RFC3339Nano UTC
    PRIMARY KEY (connection, conversation, person_id)
);

CREATE TABLE "chat_outbound" (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    connection     INTEGER NOT NULL REFERENCES butler_addresses(seq),
    target         TEXT NOT NULL,          -- conversation id: a direct chat or a group
    body           TEXT NOT NULL,
    kind           TEXT NOT NULL DEFAULT 'reply',  -- 'reply' | 'push'
    binding_id     INTEGER NOT NULL DEFAULT 0,
    correlation_id TEXT NOT NULL DEFAULT '',
    dedupe_key     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    delivered_at   TEXT NOT NULL DEFAULT '',
    -- Added by 0019: quiet hours hold a message rather than dropping it.
    hold_until     TEXT NOT NULL DEFAULT ''
, speech TEXT NOT NULL DEFAULT '');

CREATE TABLE "chat_questions" (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    connection     INTEGER NOT NULL REFERENCES butler_addresses(seq),
    correlation_id TEXT NOT NULL,
    target         TEXT NOT NULL,   -- conversation id: a direct chat or a group
    rule           TEXT NOT NULL DEFAULT '',
    subject        TEXT NOT NULL DEFAULT '',
    asked_at       TEXT NOT NULL,
    answered_at    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE meter_readings (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    meter       TEXT    NOT NULL,             -- metadata.name of the Meter resource
    value       REAL    NOT NULL,
    at          TEXT    NOT NULL,             -- RFC3339Nano UTC, when it was read
    recorded_at TEXT    NOT NULL,             -- RFC3339Nano UTC, stamped here
    -- Who said so: a member name, or the publishing principal.
    recorded_by TEXT    NOT NULL,
    -- What kind of caller recorded it. Audit only; see above.
    source      TEXT    NOT NULL CHECK (source IN ('person', 'publisher')),
    -- A rebase marks the point where the meter's coordinate space
    -- restarts but the obligation does not: a replaced odometer, a
    -- swapped hour counter. `prior` is what the old meter read just
    -- before it went, so the thresholds can be carried across; without
    -- it the reading before this one is used, and with neither the
    -- commitments measured here report themselves unresolved rather
    -- than inventing a number.
    rebase      INTEGER NOT NULL DEFAULT 0 CHECK (rebase IN (0, 1)),
    prior       REAL    NOT NULL DEFAULT 0,
    -- Why, for a rebase or a correction. Free text, shown to a person.
    reason      TEXT    NOT NULL DEFAULT '',
    -- The idempotency key for a publisher that retries a delivery. Empty
    -- for a person typing a number in, who is not retrying anything and
    -- may legitimately record the same value twice.
    external_id TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE commitment_completions (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    commitment  TEXT    NOT NULL,
    -- When the work was done, which is not when it was written down.
    at          TEXT    NOT NULL,
    recorded_at TEXT    NOT NULL,
    recorded_by TEXT    NOT NULL,
    -- A waive is a cycle deliberately skipped. It closes the cycle
    -- exactly as a completion does and is kept distinct so that "we
    -- decided not to" never reads later as "it was done".
    waived      INTEGER NOT NULL DEFAULT 0 CHECK (waived IN (0, 1)),
    reason      TEXT    NOT NULL DEFAULT '',
    -- meter name -> reading at completion, as a JSON object. Empty for a
    -- commitment measured only by the calendar, whose position is `at`.
    positions   TEXT    NOT NULL DEFAULT '{}'
, external_id TEXT NOT NULL DEFAULT '', approximate TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '');

CREATE TABLE commitment_bookings (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    commitment   TEXT    NOT NULL,
    -- When the appointment is.
    at           TEXT    NOT NULL,
    -- When it was made, and by whom. `booked_at` is why a replay cannot
    -- see an appointment nobody had arranged yet.
    booked_at    TEXT    NOT NULL,
    booked_by    TEXT    NOT NULL,
    note         TEXT    NOT NULL DEFAULT '',
    -- Cancelling is recorded rather than deleted: "we had an appointment
    -- and it was cancelled" and "we never made one" are different
    -- stories, and only one of them is somebody's fault.
    cancelled_at TEXT    NOT NULL DEFAULT '',
    cancelled_by TEXT    NOT NULL DEFAULT ''
, calendar_event TEXT NOT NULL DEFAULT '', calendar_id TEXT NOT NULL DEFAULT 'household');

CREATE TABLE commitment_state (
    commitment TEXT PRIMARY KEY,
    state      TEXT NOT NULL,
    cycle      INTEGER NOT NULL,
    -- Which meter decided it, so a commitment that moves from due-on-miles
    -- to due-on-months is treated as news.
    trigger    TEXT NOT NULL DEFAULT '',
    changed_at TEXT NOT NULL
);

CREATE TABLE person_groups (
    group_id   TEXT PRIMARY KEY,
    -- NOCASE because `Adults` and `adults` are one group to a person
    -- typing them, and two to a UNIQUE index that says otherwise. The
    -- case-sensitivity of handles is already an open defect
    -- (ADR-W032 A4); this does not add a second one.
    name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    seed_key   TEXT UNIQUE CHECK (seed_key IS NULL OR seed_key IN ('family', 'adults', 'kids')),
    created_at TEXT NOT NULL,
    CHECK (group_id <> ''),
    CHECK (name <> '')
);

CREATE TABLE person_group_members (
    group_id  TEXT NOT NULL REFERENCES person_groups(group_id) ON DELETE CASCADE,
    person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    added_at  TEXT NOT NULL,
    -- The Person who put them here, by handle, for the audit trail. Not
    -- a foreign key: the actor may be the installer or the host CLI
    -- rather than a Person, exactly as admitted_by already is.
    added_by  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (group_id, person_id)
);

CREATE TABLE audiences (
    audience_id TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('owner', 'household', 'set')),
    -- What a household calls this audience, for the ones it makes
    -- itself. Empty on the two seeded rows: "the owner" and "the
    -- household" are roles, not names somebody chose.
    label       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    CHECK (audience_id <> '')
);

CREATE TABLE audience_members (
    audience_id TEXT NOT NULL REFERENCES audiences(audience_id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('group', 'person')),
    ref         TEXT NOT NULL,
    PRIMARY KEY (audience_id, kind, ref),
    CHECK (ref <> '')
);

CREATE TABLE "butler_memory" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'inference', 'procedure')),
    subject    TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    audience   TEXT NOT NULL REFERENCES audiences(audience_id),
    permission_label TEXT NOT NULL,
    -- The person the record belongs to: who may correct it, forget it,
    -- and export it. NULL is the household (A1).
    owner_person_id TEXT REFERENCES butler_members(person_id),

    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',

    confidence TEXT NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated', 'inferred', 'uncertain')),
    safety_relevant INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    -- The person who stated it. Never the household and never a
    -- principal: a record nobody stated is not a record.
    author_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    superseded_by INTEGER REFERENCES butler_memory(seq),
    forgotten_at  TEXT NOT NULL DEFAULT '',
    -- forgotten_by is an actor column and keeps its recorded text.
    forgotten_by  TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL,
    butler     TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL DEFAULT 'household'
    CHECK (scope IN ('agent_local', 'personal', 'household')),

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (author_person_id <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

CREATE TABLE "butler_index" (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    memory      INTEGER REFERENCES butler_memory(seq),
    -- NULL is the household, as in butler_memory.
    owner_person_id TEXT REFERENCES butler_members(person_id),
    audience    TEXT NOT NULL REFERENCES audiences(audience_id),
    permission_label TEXT NOT NULL,
    content     TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',
    indexed_at  TEXT NOT NULL,
    volatile    INTEGER NOT NULL DEFAULT 0,
    embedding   BLOB,
    tokens      INTEGER NOT NULL DEFAULT 0,
    butler      TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
);

CREATE TABLE "butler_tasks" (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at          TEXT NOT NULL,     -- RFC3339Nano UTC
    updated_at          TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('conversation', 'routine')),
    state               TEXT NOT NULL CHECK (state IN (
                            'pending', 'running', 'waiting',
                            'completed', 'failed', 'cancelled',
                            'paused-offboarded')),

    -- The person who asked for the work, and the person who operates it
    -- now. Offboarding moves the second and never the first.
    author_person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    owner_person_id     TEXT NOT NULL REFERENCES butler_members(person_id),

    surface             TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    agent_class             TEXT NOT NULL,
    audience            TEXT NOT NULL REFERENCES audiences(audience_id),

    session             INTEGER REFERENCES butler_sessions(seq),
    origin_turn         INTEGER,

    correlation_id      TEXT NOT NULL DEFAULT '',

    objective           TEXT NOT NULL,
    completion_criteria TEXT NOT NULL,

    grants_json         TEXT NOT NULL DEFAULT '{}',
    budget_json         TEXT NOT NULL DEFAULT '{}',
    deadline_at         TEXT NOT NULL DEFAULT '',
    next_wakeup_at      TEXT NOT NULL DEFAULT '',

    lease_epoch         INTEGER NOT NULL DEFAULT 0,

    uncertainty         TEXT NOT NULL DEFAULT '',
    result_summary      TEXT NOT NULL DEFAULT '',

    retention           TEXT NOT NULL CHECK (retention IN ('manual', 'conversation', 'ephemeral')),
    envelope_version    INTEGER NOT NULL,
    butler              TEXT NOT NULL DEFAULT ''
, origin_connection TEXT NOT NULL DEFAULT '', origin_connection_ref INTEGER NOT NULL DEFAULT 0, objective_kind TEXT NOT NULL DEFAULT 'effectful'
 CHECK(objective_kind IN ('informational','effectful')), proposal_only INTEGER NOT NULL DEFAULT 0 CHECK(proposal_only IN (0,1)), conversation_work INTEGER REFERENCES conversation_work(seq), handle_hash TEXT NOT NULL DEFAULT '', as_owner INTEGER NOT NULL DEFAULT 0, system_extra TEXT NOT NULL DEFAULT '', origin_conversation TEXT NOT NULL DEFAULT '');

CREATE TABLE "butler_task_evidence" (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    recorded_at      TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('observation', 'tool-result', 'approval', 'delivery', 'external-verification')),
    description      TEXT NOT NULL,
    source_id        TEXT NOT NULL DEFAULT '',
    observed_at      TEXT NOT NULL DEFAULT '',
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL REFERENCES audiences(audience_id),
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL, receipt INTEGER REFERENCES butler_effect_receipts(seq),
    CHECK (permission_label <> ''),
    CHECK (description <> '')
);

CREATE TABLE "butler_work_trace" (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    at               TEXT NOT NULL,
    type             TEXT NOT NULL,
    step             INTEGER REFERENCES butler_task_steps(seq),
    attempt          INTEGER REFERENCES butler_task_attempts(seq),
    epoch            INTEGER NOT NULL DEFAULT 0,
    actor            TEXT NOT NULL DEFAULT '',
    -- Versions of the things that decide behaviour, so a before/after
    -- comparison in P5 knows what actually changed (A11).
    model_endpoint   TEXT NOT NULL DEFAULT '',
    model_verdict    TEXT NOT NULL DEFAULT '',
    prompt_version   TEXT NOT NULL DEFAULT '',
    policy_version   TEXT NOT NULL DEFAULT '',
    manifest_version TEXT NOT NULL DEFAULT '',
    source_ids       TEXT NOT NULL DEFAULT '',  -- comma-separated context source ids
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL REFERENCES audiences(audience_id),
    redacted         INTEGER NOT NULL DEFAULT 1,
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (type <> '')
);

CREATE TABLE "integration_connections" (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    -- adapter names the adapter implementation this connection uses.
    -- Application-specific behaviour lives there; this row is a
    -- neutral description of one instance of it.
    adapter     TEXT NOT NULL,
    endpoint    TEXT NOT NULL DEFAULT '',
    -- Ownership decides the defaults (A5). A personal connection starts
    -- private, read-only, live-query-only and local-processing-only
    -- until its owner explicitly widens it; a household one starts
    -- household-shared and read-only.
    ownership   TEXT NOT NULL CHECK (ownership IN ('personal', 'household')),
    owner       TEXT NOT NULL,
    audience    TEXT NOT NULL REFERENCES audiences(audience_id),
    -- Discovered actions stay disabled until separately enabled and
    -- granted (A5). This is that switch, and it defaults off.
    actions_enabled INTEGER NOT NULL DEFAULT 0,
    indexing_enabled INTEGER NOT NULL DEFAULT 0,
    cloud_allowed   INTEGER NOT NULL DEFAULT 0,

    state       TEXT NOT NULL CHECK (state IN ('configured', 'ready', 'degraded', 'unavailable')),
    -- lease_epoch fences adapters. Only the holder of the current epoch
    -- may claim; results from an older one are refused.
    lease_epoch INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    -- degraded_reason is why a connection is not fully working. A
    -- connection that is degraded for a reason nobody can read is a
    -- connection nobody fixes.
    degraded_reason TEXT NOT NULL DEFAULT '', executable TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (adapter <> ''),
    CHECK (owner <> ''),
    CHECK (created_by <> '')
);

CREATE TABLE service_entitlements (
    seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The capability, e.g. 'photos'. Not a foreign key: a service may be
    -- uninstalled while its entitlements go dormant (below), so the row
    -- outlives anything that could be referenced.
    service TEXT NOT NULL,

    -- Who. A group_id from slice 6's namespace, or a person_id.
    --
    -- Both are opaque ids rather than names, for the reason slice 6
    -- gives: a household that renames `Adults` must not thereby
    -- re-point every rule that named it, and it does not, because no
    -- rule ever stored the word.
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('group', 'person')),
    subject      TEXT NOT NULL,

    -- Which way it points. 'restrict' is not "no allow": it is an
    -- explicit refusal that outranks an inherited allow.
    effect TEXT NOT NULL CHECK (effect IN ('allow', 'restrict')),

    -- Set when the service was removed (§17.3, §17.4). A dormant row
    -- grants nothing and is not deleted, so reinstalling the capability
    -- restores what the household had decided rather than asking them
    -- to decide it again.
    dormant_at TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    -- Who decided, by handle, for the audit trail. Not a foreign key:
    -- the actor may be the installer or the host CLI rather than a
    -- Person, exactly as admitted_by already is.
    created_by TEXT NOT NULL DEFAULT '',

    -- One row per subject per service. A subject cannot be both allowed
    -- and restricted from the same service: that is a contradiction to
    -- resolve when it is written, not a precedence puzzle to resolve on
    -- every read.
    UNIQUE (service, subject_kind, subject),
    CHECK (service <> ''),
    CHECK (subject <> '')
);

CREATE TABLE projected_memberships (
    -- The Person, by id. Projection resolves a directory account to a
    -- Person by handle and refuses to act when that is ambiguous; what
    -- it records is the identity, never the name it matched on.
    person_id  TEXT NOT NULL REFERENCES butler_members(person_id),
    -- The DIRECTORY group's name, e.g. 'docs_users' — not a slice-6
    -- group_id. This table is a record of what exists in LLDAP, and
    -- LLDAP has never heard of a group_id.
    ldap_group TEXT NOT NULL,
    placed_at  TEXT NOT NULL,
    PRIMARY KEY (person_id, ldap_group),
    CHECK (ldap_group <> '')
);

CREATE TABLE connection_invites (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    connection    INTEGER NOT NULL REFERENCES butler_addresses(seq),
    person_id     TEXT NOT NULL REFERENCES butler_members(person_id),
    invited_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    closed_at     TEXT NOT NULL DEFAULT '',
    closed_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE connection_credentials (
    connection  INTEGER PRIMARY KEY REFERENCES butler_addresses(seq),
    fingerprint TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE "butlers" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- name is the Butler's own, given by the person who made it
    -- (A11): Alfred, Robert. "Butler" is the role; this is the
    -- instance, and it is what a person says and what a routine is
    -- assigned to.
    name       TEXT NOT NULL UNIQUE,
    -- directive is what this Butler is for, in the owner's words. It
    -- shapes how the Butler talks and grants nothing (A9): tools,
    -- connections and memory scope are separate and explicit, or
    -- "you may read whatever you need" becomes an authority grant
    -- written in prose.
    directive  TEXT NOT NULL DEFAULT '',
    -- surface is what ENFORCES the ceiling: the family gateway does not
    -- register an operator tool, so there is nothing for a control or a
    -- grant to switch on (A3).
    surface    TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    -- agent_class is what NAMES that ceiling, in §5.2's words. The pair
    -- below is what keeps the name and the enforcement the same fact.
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    -- model is the assigned model. NOT NULL and a real reference: the
    -- whole design rests on this being answerable.
    model      INTEGER NOT NULL REFERENCES "models"(seq),
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    -- retired_at makes a Butler stop without deleting what it did. Its
    -- conversations, memory and traces are still evidence.
    retired_at TEXT NOT NULL DEFAULT '',
    retired_by TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (created_by <> ''),
    -- The paired CHECK. An Owner Agent lives on the admin surface and a
    -- Member Agent does not, in both directions, so neither column can
    -- be written without the other agreeing.
    CHECK ((agent_class = 'owner') = (surface = 'admin'))
);

CREATE TABLE agent_class_resolutions (
    butler       INTEGER NOT NULL REFERENCES butlers(seq),
    former_profile TEXT NOT NULL,
    agent_class  TEXT NOT NULL,
    disabled     INTEGER NOT NULL,
    note         TEXT NOT NULL,
    resolved_at  TEXT NOT NULL
);

CREATE TABLE "butler_tool_controls" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    tool       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    CHECK (tool <> ''),
    CHECK (updated_by <> '')
);

CREATE TABLE "butler_tool_grants" (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL CHECK (scope IN ('task', 'routine')),
    -- scope_id is the task id ("bt-<n>") or the routine name. Text so
    -- one column serves both.
    scope_id   TEXT NOT NULL,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    -- butler is WHOSE grant this is.
    --
    -- Without it a grant belongs to a class, so every Agent of that
    -- class holds it, and §18.3's "downgrading an Owner Agent strips ITS
    -- tool grants" cannot be said: stripping would take the grant from
    -- every other Owner Agent too.
    --
    -- NULL is the pre-0058 shape, and is kept rather than backfilled.
    -- A grant issued before this migration was genuinely class-level —
    -- guessing an Agent for it would invent an attribution the household
    -- never made. A deployment with no Butlers at all still resolves
    -- through this path, which is why it stays supported rather than
    -- being migrated away.
    butler     INTEGER REFERENCES butlers(seq),
    tool       TEXT NOT NULL,
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    -- cleanup_plan links the Plan that must run when this grant ends —
    -- removing a credential, closing an opened path. Recorded here so a
    -- grant that needs cleanup cannot quietly expire without one.
    cleanup_plan TEXT NOT NULL DEFAULT '',
    CHECK (scope_id <> ''),
    CHECK (tool <> ''),
    CHECK (granted_by <> ''),
    CHECK (expires_at <> '')
);

CREATE TABLE "butler_capability_requests" (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('supported-tool', 'development')),
    -- requested_by is the MEMBER the asking session was acting for. The
    -- Butler asks on someone's behalf; it does not have wishes of its
    -- own, and an unattributed request is one nobody can weigh.
    requested_by TEXT NOT NULL,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    -- tool is the name for a supported-tool request, and empty for a
    -- development request, which describes something that does not exist
    -- yet.
    tool        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('open', 'approved', 'declined', 'implemented')),
    created_at  TEXT NOT NULL,
    decided_at  TEXT NOT NULL DEFAULT '',
    decided_by  TEXT NOT NULL DEFAULT '',
    decision_note TEXT NOT NULL DEFAULT '',
    CHECK (requested_by <> ''),
    CHECK (description <> ''),
    CHECK (kind <> 'supported-tool' OR tool <> '')
);

CREATE TABLE butler_tool_governance (
    butler     INTEGER PRIMARY KEY REFERENCES butlers(seq),
    -- since is when this Agent started being default-deny. Kept because
    -- "why can this Agent suddenly not do X" is answered by the date it
    -- changed model, not by the assignments it has now.
    since      TEXT NOT NULL,
    enrolled_by TEXT NOT NULL,

    CHECK (enrolled_by <> '')
);

CREATE TABLE butler_tool_assignments (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    butler      INTEGER NOT NULL REFERENCES butlers(seq),
    tool        TEXT NOT NULL,
    assigned_at TEXT NOT NULL,
    assigned_by TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (tool <> ''),
    CHECK (assigned_by <> '')
);

CREATE TABLE butler_tool_bundles (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    butler      INTEGER NOT NULL REFERENCES butlers(seq),
    -- service is the capability whose tools this covers, e.g. 'photos'.
    -- Keystone's own tools are 'system'.
    service     TEXT NOT NULL,
    agent_class TEXT NOT NULL CHECK (agent_class IN ('owner', 'member')),
    granted_at  TEXT NOT NULL,
    granted_by  TEXT NOT NULL,
    revoked_at  TEXT NOT NULL DEFAULT '',
    revoked_by  TEXT NOT NULL DEFAULT '',

    CHECK (service <> ''),
    CHECK (granted_by <> '')
);

CREATE TABLE person_theme (
    person_id  TEXT PRIMARY KEY REFERENCES butler_members(person_id) ON DELETE CASCADE,
    -- The theme's name, which is either one shipped in the binary or
    -- one in household_themes. NOT a foreign key onto household_themes,
    -- because most names are not rows there — and because a household
    -- removing an imported theme should leave the people who chose it
    -- back on the design's own look rather than failing their write.
    -- The resolver treats an unknown name as no choice at all.
    theme      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (theme <> '')
);

CREATE TABLE household_themes (
    -- The slug, validated by theme.ValidName before it is written: it
    -- ends up in a URL and in a picker.
    name       TEXT PRIMARY KEY,
    -- The colors.toml, verbatim.
    colors     TEXT NOT NULL,
    -- Who added it, so the household can tell. An actor column: it
    -- records whoever did it, which on this path is always an Owner.
    added_by   TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    CHECK (name <> ''),
    CHECK (colors <> '')
);

CREATE TABLE theme_backgrounds (
    -- The theme this picture belongs to. Validated by theme.ValidName
    -- before it is written.
    name       TEXT PRIMARY KEY,
    image      BLOB NOT NULL,
    -- Decided from the BYTES on the way in, never from what the upload
    -- claimed — see handlers_portal_banner.go, which is the other place
    -- this surface accepts an image and the reasoning it records.
    media_type TEXT NOT NULL,
    -- Who added it, so the household can tell. An actor column.
    set_by     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    CHECK (name <> ''),
    CHECK (media_type <> '')
);

CREATE TABLE connection_admission (
    -- One policy per Connection. PRIMARY KEY rather than an indexed
    -- column: two admission rows for one Connection would be two
    -- answers to "who may address this Agent here", and a resolver
    -- picking between them is a permission decided by ORDER BY.
    connection   INTEGER PRIMARY KEY REFERENCES butler_addresses(seq),

    -- admits is the class ceiling: which Person role may address the
    -- Agent over this Connection.
    --
    -- 'owners'    — Owners and nobody else (§8.3).
    -- 'household' — any admitted Person, Owner or Member.
    --
    -- It is a CEILING and not a grant. Being of an admitted role does
    -- not by itself let somebody in: they still need a live binding on
    -- this Connection, which is what chat_bindings records. This says
    -- who MAY be admitted, never who IS.
    admits       TEXT NOT NULL CHECK (admits IN ('owners', 'household')),

    -- subject_kind is §8.3's seam, present so the schema supports what
    -- the design says it supports rather than being retrofitted when
    -- group conversations arrive.
    --
    -- MVP IS 'person' AND THE RESOLVER REFUSES ANYTHING ELSE. §8.3:
    -- "Group and multi-recipient destinations fail closed in MVP...
    -- Person admission authenticates the speaker, not everyone who
    -- will receive the reply." A 'group' row is therefore legal to
    -- write and denied at admission, which is the honest way to carry
    -- a seam: the column exists, and nothing is admitted through it
    -- until the audience rules that make it safe do.
    subject_kind TEXT NOT NULL DEFAULT 'person' CHECK (subject_kind IN ('person', 'group')),

    set_at       TEXT NOT NULL,
    set_by       TEXT NOT NULL,

    CHECK (set_by <> '')
);

CREATE TABLE connection_disclosure (
    connection INTEGER PRIMARY KEY REFERENCES butler_addresses(seq),

    -- Where the bytes go. 'local' is a service this household runs;
    -- 'external' leaves the house.
    reach      TEXT NOT NULL CHECK (reach IN ('local', 'external')),

    -- What protects them in transit. 'none' is deliberately spellable:
    -- a plaintext transport should be describable rather than
    -- unrepresentable, because the policy answer for it is to refuse
    -- things, and you cannot refuse what you cannot name.
    encryption TEXT NOT NULL CHECK (encryption IN ('none', 'transport', 'end_to_end')),

    -- Who keeps a copy. 'unknown' is NOT a synonym for 'none' and is
    -- the conservative default: an operator who has not said must be
    -- treated as a provider that retains, never as one that does not.
    retention  TEXT NOT NULL CHECK (retention IN ('none', 'provider', 'unknown')),

    -- What the person is looking at. The Homepage is a surface this
    -- deployment renders; Telegram is somebody else's client on
    -- somebody else's phone.
    surface    TEXT NOT NULL CHECK (surface IN ('trusted_ui', 'external_messaging')),

    -- MAY A MESSAGE MEANT FOR ANOTHER CONNECTION BE DELIVERED HERE.
    --
    -- §8.3: "Cross-Connection delivery fallback occurs only when
    -- explicitly configured -- never as a default." So the default is
    -- 0, and it is a column rather than an inference because the
    -- alternative is what the code did before: ChatRecipientFor walked
    -- every live binding on a transport and returned the first match,
    -- so a household with two bots on Telegram delivered a rule's
    -- message to whichever Connection sorted first. That is a
    -- cross-Connection fallback nobody configured and nobody could see.
    accepts_fallback INTEGER NOT NULL DEFAULT 0 CHECK (accepts_fallback IN (0, 1)),

    set_at TEXT NOT NULL,
    set_by TEXT NOT NULL,
    CHECK (set_by <> '')
);

CREATE TABLE chat_rooms (
    connection    INTEGER NOT NULL REFERENCES butler_addresses(seq),
    conversation  TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    PRIMARY KEY (connection, conversation),
    CHECK (conversation <> '')
);

CREATE TABLE butler_session_connections (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    session         INTEGER NOT NULL REFERENCES butler_sessions(seq),
    direction       TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    connection_ref  INTEGER NOT NULL DEFAULT 0,
    connection_name TEXT NOT NULL,
    recorded_at     TEXT NOT NULL,
    CHECK (connection_name <> '')
);

CREATE TABLE chat_thread_breaks (
    connection   INTEGER NOT NULL REFERENCES butler_addresses(seq),
    conversation TEXT NOT NULL,
    person_id    TEXT NOT NULL REFERENCES butler_members(person_id),
    broken_at    TEXT NOT NULL,
    PRIMARY KEY (connection, conversation, person_id)
);

CREATE TABLE session_resume_offers (
    token_hash      TEXT PRIMARY KEY,
    person_id       TEXT NOT NULL REFERENCES butler_members(person_id),
    agent_id        INTEGER NOT NULL REFERENCES butlers(seq),
    connection      INTEGER NOT NULL REFERENCES butler_addresses(seq),
    session         INTEGER NOT NULL REFERENCES butler_sessions(seq),
    created_at      TEXT NOT NULL,
    expires_at      TEXT NOT NULL,
    consumed_at     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE memory_constraints (
    memory INTEGER NOT NULL REFERENCES butler_memory(seq),
    audience TEXT NOT NULL REFERENCES audiences(audience_id),
    owner_person_id TEXT REFERENCES butler_members(person_id)
);

CREATE TABLE memory_agent_restrictions (
    agent INTEGER NOT NULL REFERENCES butlers(seq),
    scope TEXT NOT NULL CHECK(scope IN ('personal', 'household')),
    PRIMARY KEY(agent, scope)
);

CREATE TABLE memory_scope_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    memory INTEGER NOT NULL REFERENCES butler_memory(seq),
    kind TEXT NOT NULL CHECK(kind IN ('promotion', 'transfer')),
    source_memory INTEGER REFERENCES butler_memory(seq),
    source_agent TEXT NOT NULL DEFAULT '',
    destination_agent TEXT NOT NULL DEFAULT '',
    actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    recorded_at TEXT NOT NULL
);

CREATE TABLE memory_disclosure_origins (
    memory INTEGER NOT NULL REFERENCES butler_memory(seq),
    connection_ref INTEGER NOT NULL,
    connection_name TEXT NOT NULL,
    reach TEXT NOT NULL, encryption TEXT NOT NULL,
    retention TEXT NOT NULL, surface TEXT NOT NULL
);

CREATE TABLE household_secrets (
 id TEXT PRIMARY KEY,
 scope TEXT NOT NULL CHECK(scope IN ('personal','household')),
 owner_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 label TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','active','quarantine','disabled','deleted')),
 version INTEGER NOT NULL DEFAULT 1,
 ciphertext BLOB NOT NULL,
 created_at TEXT NOT NULL
);

CREATE TABLE household_secret_grants (
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 person_id TEXT REFERENCES butler_members(person_id),
 group_id TEXT REFERENCES person_groups(group_id),
 verb TEXT NOT NULL CHECK(verb IN ('use','reveal')),
 CHECK((person_id IS NULL) != (group_id IS NULL))
);

CREATE TABLE household_secret_agents (
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 agent INTEGER NOT NULL REFERENCES butlers(seq),
 purpose TEXT NOT NULL CHECK(purpose IN ('calendar.get_agenda','reveal')),
 PRIMARY KEY(secret_id,agent,purpose)
);

CREATE TABLE household_secret_audit (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 action TEXT NOT NULL,
 at TEXT NOT NULL
);

CREATE TABLE household_secret_bindings (
 purpose TEXT NOT NULL CHECK(purpose='calendar.get_agenda'),
 principal TEXT NOT NULL,
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 PRIMARY KEY(purpose,principal)
);

CREATE TABLE household_secret_origins (
 secret_id TEXT PRIMARY KEY REFERENCES household_secrets(id),
 connection_ref INTEGER NOT NULL,
 disclosed_externally INTEGER NOT NULL CHECK(disclosed_externally IN (0,1)),
 recorded_at TEXT NOT NULL
);

CREATE TABLE household_secret_disclosures (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 connection_ref INTEGER NOT NULL,
 recorded_at TEXT NOT NULL
);

CREATE TABLE content_privacy (
 object TEXT PRIMARY KEY,
 local_only INTEGER NOT NULL DEFAULT 0 CHECK(local_only IN (0,1)),
 trusted_only INTEGER NOT NULL DEFAULT 0 CHECK(trusted_only IN (0,1))
);

CREATE TABLE session_memory_sources (
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 memory INTEGER NOT NULL REFERENCES butler_memory(seq) ON DELETE CASCADE,
 PRIMARY KEY(session,memory)
);

CREATE TABLE task_memory_sources (
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 memory INTEGER NOT NULL REFERENCES butler_memory(seq) ON DELETE CASCADE,
 PRIMARY KEY(task,memory)
);

CREATE TABLE session_task_sources (
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 PRIMARY KEY(session,task)
);

CREATE TABLE memory_origin_sessions (
 memory INTEGER NOT NULL REFERENCES butler_memory(seq) ON DELETE CASCADE,
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 PRIMARY KEY(memory,session)
);

CREATE TABLE chat_outbound_sessions (
 outbound INTEGER NOT NULL REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 PRIMARY KEY(outbound,session)
);

CREATE TABLE chat_outbound_tasks (
 outbound INTEGER NOT NULL REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 PRIMARY KEY(outbound,task)
);

CREATE TABLE chat_outbound_memories (
 outbound INTEGER NOT NULL REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 memory INTEGER NOT NULL REFERENCES butler_memory(seq) ON DELETE CASCADE,
 PRIMARY KEY(outbound,memory)
);

CREATE TABLE plan_private_origins (
 plan INTEGER PRIMARY KEY REFERENCES plans(seq) ON DELETE CASCADE
);

CREATE TABLE plan_session_sources (
 plan INTEGER NOT NULL REFERENCES plans(seq) ON DELETE CASCADE,
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 PRIMARY KEY(plan,session)
);

CREATE TABLE owner_review_queue (
 plan INTEGER PRIMARY KEY REFERENCES plans(seq),
 proposer_person_id TEXT REFERENCES butler_members(person_id),
 proposing_agent INTEGER REFERENCES butlers(seq),
 queued_at TEXT NOT NULL
);

CREATE TABLE routine_plan_versions (
 plan INTEGER PRIMARY KEY REFERENCES plans(seq),
 routine INTEGER NOT NULL REFERENCES household_routines(seq),
 revision INTEGER NOT NULL,
 document TEXT NOT NULL,
 output_audience TEXT NOT NULL REFERENCES audiences(audience_id),
 delivery TEXT NOT NULL,
 assigned_butler TEXT NOT NULL DEFAULT '',
 approved_by TEXT REFERENCES butler_members(person_id)
, secret_ids TEXT NOT NULL DEFAULT '[]', memory_ids TEXT NOT NULL DEFAULT '[]');

CREATE TABLE owner_review_preference (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 binding_id INTEGER REFERENCES chat_bindings(id),
 console_url TEXT NOT NULL DEFAULT '',
 generation INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE butler_effect_receipts (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 recorded_at TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('tool','scheduler','executor')),
 source_id TEXT NOT NULL CHECK(source_id<>''),
 step INTEGER REFERENCES butler_task_steps(seq) ON DELETE CASCADE,
 proposal_only INTEGER NOT NULL DEFAULT 0 CHECK(proposal_only IN (0,1)),
 outcome TEXT NOT NULL CHECK(outcome IN ('succeeded','failed','uncertain')),
 permission_label TEXT NOT NULL CHECK(permission_label<>''),
 audience TEXT NOT NULL REFERENCES audiences(audience_id),
 CHECK(kind<>'tool' OR step IS NOT NULL),
 UNIQUE(task,kind,source_id)
);

CREATE TABLE butler_required_effects (
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL,
 description TEXT NOT NULL CHECK(description<>''),
 source_key TEXT NOT NULL DEFAULT '',
 proposal_only INTEGER NOT NULL DEFAULT 0 CHECK(proposal_only IN (0,1)),
 PRIMARY KEY(task,ordinal)
);

CREATE TABLE chat_outbound_effects (
 outbound INTEGER PRIMARY KEY REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 step INTEGER NOT NULL REFERENCES butler_task_steps(seq) ON DELETE CASCADE,
 permission_label TEXT NOT NULL CHECK(permission_label<>''),
 audience TEXT NOT NULL REFERENCES audiences(audience_id),
 expected_body TEXT NOT NULL
);

CREATE TABLE household_secret_routine_grants (
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 verb TEXT NOT NULL DEFAULT 'use' CHECK(verb='use'),
 PRIMARY KEY(plan,secret_id,verb)
);

CREATE TABLE routine_memory_grants (
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 memory INTEGER NOT NULL REFERENCES butler_memory(seq),
 PRIMARY KEY(plan,memory)
);

CREATE TABLE household_secret_routine_audit (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 secret_id TEXT NOT NULL REFERENCES household_secrets(id),
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 action TEXT NOT NULL CHECK(action='use'),
 at TEXT NOT NULL
);

CREATE TABLE routine_execution_state (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 cursor INTEGER NOT NULL DEFAULT 0 CHECK(cursor>=0),
 initialized INTEGER NOT NULL DEFAULT 0 CHECK(initialized IN (0,1))
);

CREATE TABLE routine_runs (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 plan INTEGER NOT NULL REFERENCES routine_plan_versions(plan),
 trigger_event INTEGER NOT NULL REFERENCES household_events(seq),
 state TEXT NOT NULL CHECK(state IN ('pending','running','blocked','completed','failed','uncertain','stopped')),
 reason TEXT NOT NULL DEFAULT '',
 effect_state TEXT NOT NULL DEFAULT 'none' CHECK(effect_state IN ('none','claimed','pending','confirmed','failed','uncertain')),
 effect_summary TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(plan,trigger_event)
);

CREATE TABLE routine_effect_receipts (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 run INTEGER NOT NULL REFERENCES routine_runs(seq),
 recorded_at TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('pending','confirmed','failed','uncertain')),
 summary TEXT NOT NULL CHECK(summary<>''),
 UNIQUE(run)
);

CREATE TABLE routine_run_transitions (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 run INTEGER NOT NULL REFERENCES routine_runs(seq),
 at TEXT NOT NULL,
 from_state TEXT NOT NULL,
 to_state TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE household_execution_stop (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 active INTEGER NOT NULL DEFAULT 0 CHECK(active IN (0,1)),
 generation INTEGER NOT NULL DEFAULT 0,
 entered_at TEXT NOT NULL DEFAULT '',
 entered_by TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL DEFAULT '',
 released_at TEXT NOT NULL DEFAULT '',
 released_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE household_execution_stop_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 generation INTEGER NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('stop','resume')),
 at TEXT NOT NULL,
 actor TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE conversation_work (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 session INTEGER UNIQUE REFERENCES butler_sessions(seq),
 owner_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 tool_calls INTEGER NOT NULL DEFAULT 0 CHECK(tool_calls>=0),
 tool_limit INTEGER NOT NULL DEFAULT 64 CHECK(tool_limit>0),
 uncertain_outcome INTEGER NOT NULL DEFAULT 0 CHECK(uncertain_outcome IN (0,1)),
 uncertain_task INTEGER REFERENCES butler_tasks(seq),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);

CREATE TABLE conversation_work_reviews (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 work INTEGER NOT NULL REFERENCES conversation_work(seq),
 actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 action TEXT NOT NULL CHECK(action IN ('resolve-outcome','extend-budget')),
 note TEXT NOT NULL CHECK(note<>''),
 at TEXT NOT NULL
);

CREATE TABLE offboarding_resource_dispositions (
 person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 resource_kind TEXT NOT NULL,
 resource_ref TEXT NOT NULL,
 disposition TEXT NOT NULL CHECK(disposition IN ('transferred','retained','retained-paused','retained-quarantined','blocked','retired','deleted')),
 decided_at TEXT NOT NULL,
 decided_by TEXT NOT NULL,
 PRIMARY KEY(person_id,resource_kind,resource_ref)
);

CREATE TABLE butler_runtimes (
    butler            INTEGER PRIMARY KEY REFERENCES butlers(seq) ON DELETE CASCADE,
    -- type is the closed set the ADR names; each addition is a reviewed
    -- change here and in keystone/agentapi, which must agree.
    type              TEXT NOT NULL DEFAULT 'native'
                      CHECK (type IN ('native', 'hermes', 'openclaw')),
    -- endpoint is empty for native (in-process); a Route host or
    -- container/<name>:<port> for a packaged runtime.
    endpoint          TEXT NOT NULL DEFAULT '',
    -- principal is the CURRENT credential's name — the credentialRef of
    -- §3.3. It is a reference into runtime_principals below, not a
    -- foreign key, because a row exists before its first credential is
    -- minted and '' is the honest value then.
    principal         TEXT NOT NULL DEFAULT '',
    health            TEXT NOT NULL DEFAULT 'unregistered'
                      CHECK (health IN ('unregistered', 'connected', 'degraded', 'offline', 'revoked')),
    last_heartbeat    TEXT NOT NULL DEFAULT '',
    -- delivery and processing_class are the negotiated pair of §3.3.
    -- The processing class is declared by the Owner at registration and
    -- is never a claim the runtime makes; '' means undeclared, which for
    -- native means "read the assigned model's kind".
    delivery          TEXT NOT NULL DEFAULT 'push'
                      CHECK (delivery IN ('push', 'pull')),
    processing_class  TEXT NOT NULL DEFAULT ''
                      CHECK (processing_class IN ('', 'local_only', 'cloud')),
    -- execution_profile is #334's vocabulary and a reference here, not
    -- an enum: platform-only | sandboxed | host-admin today.
    execution_profile TEXT NOT NULL DEFAULT 'platform-only',
    updated_at        TEXT NOT NULL,
    updated_by        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE runtime_principals (
    principal  TEXT PRIMARY KEY REFERENCES principals(name),
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    created_at TEXT NOT NULL
);

CREATE TABLE agent_deliveries (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The Agent this delivery is FOR, never a runtime principal
    -- directly: a principal rotates: the Agent it is bound to does not
    -- (ADR-W039 §3.7 "runtime change ... replaces the runtime field").
    butler     INTEGER NOT NULL REFERENCES butlers(seq) ON DELETE CASCADE,
    -- kind mirrors agentapi.DeliveryKind's closed set.
    kind       TEXT NOT NULL DEFAULT 'proposal_outcome'
               CHECK (kind IN ('turn', 'subscription', 'escalation', 'proposal_outcome')),
    -- cursor is the per-subscription position this delivery advances
    -- to (§3.6); empty for a kind with no subscription behind it, which
    -- proposal_outcome — filed synchronously when a Plan is decided,
    -- not matched off the household stream — always is until a future
    -- kind needs it.
    cursor     TEXT NOT NULL DEFAULT '',
    -- payload is the enveloped event, turn or outcome, exactly as
    -- agentapi.Delivery.Payload carries it to a runtime: opaque JSON
    -- this table does not interpret.
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    -- expires_at is when an unacknowledged delivery is recorded as
    -- expired rather than redelivered forever (§3.6); empty means no
    -- expiry has been set (Phase 4's consumer decides the window).
    expires_at TEXT NOT NULL DEFAULT '',
    -- acked_at is set once a runtime acknowledges the delivery by ID
    -- (Phase 4); empty means still pending.
    acked_at   TEXT NOT NULL DEFAULT ''
, expired_at TEXT NOT NULL DEFAULT '');

CREATE TABLE "butler_task_waits" (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    task        INTEGER NOT NULL REFERENCES butler_tasks(seq),
    created_at  TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN (
                    'reply', 'approval', 'schedule', 'model-unavailable',
                    'budget-exhausted', 'clarification', 'source-unavailable',
                    'uncertain-outcome', 'runtime-offline')),
    detail      TEXT NOT NULL DEFAULT '',
    resume_at   TEXT NOT NULL DEFAULT '',
    resolved_at TEXT NOT NULL DEFAULT '',
    resolved_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE agent_subscriptions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The Agent this subscription is FOR. A bounded Task it spawns runs
    -- as this Agent, exactly as a routine occurrence runs as its
    -- assigned Butler (routine_plan_versions.assigned_butler).
    butler         INTEGER NOT NULL REFERENCES butlers(seq) ON DELETE CASCADE,
    -- match_kind names the matcher keystone/automation/subscriptions.go
    -- runs; match_json is that matcher's own parameters, opaque here.
    match_kind     TEXT NOT NULL,
    match_json     TEXT NOT NULL DEFAULT '{}',
    -- audience is who a matched Task's output may be disclosed to.
    audience       TEXT NOT NULL DEFAULT '',
    -- capability_set is a JSON array of tool names a matched Task may
    -- use. It can only ever NARROW what the Agent already holds — never
    -- widen it, however it is proposed (§3.6) — and it is re-read live
    -- from this row at every use, not cached on the Task, so narrowing
    -- or disabling a subscription after it has already matched still
    -- takes effect on the Task it spawned.
    capability_set TEXT NOT NULL DEFAULT '[]',
    budget_json    TEXT NOT NULL DEFAULT '{}',
    -- approved_version, approved_by and approved_at are the Owner
    -- approval this subscription cannot run without. version is a plain
    -- counter (1 the first time this row is approved); a later change
    -- to an approved subscription is a new proposal against a new row,
    -- the same shape a routine's own revisions take.
    approved_version INTEGER NOT NULL DEFAULT 0,
    approved_by    TEXT NOT NULL DEFAULT '',
    approved_at    TEXT NOT NULL DEFAULT '',
    -- enabled is the one bit the evaluator actually reads before it
    -- ever matches anything. 0 until approval; an Owner can also turn
    -- an approved subscription back off without deleting the row.
    enabled        INTEGER NOT NULL DEFAULT 0,
    -- cursor is this subscription's own position in the household event
    -- stream (§3.6 "per-subscription cursor"), the same cursor-based
    -- pattern automation.Engine already keeps for its own tick.
    cursor         INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
, trigger_kind TEXT NOT NULL DEFAULT 'event'
    CHECK (trigger_kind IN ('event', 'schedule')), cadence_seconds INTEGER NOT NULL DEFAULT 0, source_connection TEXT NOT NULL DEFAULT '', person_id TEXT REFERENCES butler_members(person_id), version INTEGER NOT NULL DEFAULT 1);

CREATE TABLE household_lists (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL CHECK (name <> '' AND length(name) <= 60),
    name_key   TEXT NOT NULL UNIQUE CHECK (name_key <> ''),
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

CREATE TABLE household_list_items (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    list       INTEGER NOT NULL REFERENCES household_lists(seq) ON DELETE CASCADE,
    text       TEXT NOT NULL CHECK (text <> '' AND length(text) <= 200),
    text_key   TEXT NOT NULL CHECK (text_key <> ''),
    checked    INTEGER NOT NULL DEFAULT 0 CHECK (checked IN (0, 1)),
    added_at   TEXT NOT NULL,
    added_by   TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL DEFAULT '',
    checked_by TEXT NOT NULL DEFAULT '',
    UNIQUE (list, text_key)
);

CREATE TABLE integration_pairings (
    capability     TEXT PRIMARY KEY CHECK (capability <> '' AND length(capability) <= 52),
    endpoint       TEXT NOT NULL CHECK (endpoint <> '' AND length(endpoint) <= 512),
    key_ciphertext BLOB NOT NULL,
    paired_at      TEXT NOT NULL,
    paired_by      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE person_handles (
    handle      TEXT PRIMARY KEY,
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    recorded_at TEXT NOT NULL,
    CHECK (handle <> '')
);

CREATE TABLE "calendar_visibility" (
    person_id   TEXT NOT NULL REFERENCES butler_members(person_id),
    calendar_id TEXT NOT NULL,
    visible     INTEGER NOT NULL CHECK (visible IN (0, 1)),
    PRIMARY KEY (person_id, calendar_id)
);

CREATE TABLE person_file_dirs (
    person_id  TEXT PRIMARY KEY REFERENCES butler_members(person_id),
    dir        TEXT NOT NULL,
    created_at TEXT NOT NULL,
    CHECK (dir <> '')
);

CREATE TABLE backup_source_samples (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_name    TEXT NOT NULL,
    measured_at  TEXT NOT NULL,
    source_bytes INTEGER NOT NULL CHECK (source_bytes >= 0)
);

CREATE TABLE agent_workspaces (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 agent_id INTEGER NOT NULL REFERENCES butlers(seq),
 created_at TEXT NOT NULL,
 visibility TEXT NOT NULL DEFAULT 'private' CHECK(visibility IN ('private','shared')),
 UNIQUE(person_id,agent_id,visibility)
);

CREATE TABLE workspace_artifacts (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 workspace_id TEXT NOT NULL REFERENCES agent_workspaces(id),
 path TEXT NOT NULL CHECK(path<>''),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','committed','trashed')),
 current_revision TEXT,
 created_at TEXT NOT NULL,
 trashed_at TEXT NOT NULL DEFAULT '',
 UNIQUE(id,workspace_id),
 FOREIGN KEY(current_revision,id) REFERENCES workspace_revisions(id,artifact_id),
 CHECK((state='pending' AND current_revision IS NULL) OR (state IN ('committed','trashed') AND current_revision IS NOT NULL)),
 CHECK((state='trashed' AND trashed_at<>'') OR (state<>'trashed' AND trashed_at=''))
);

CREATE TABLE workspace_revisions (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 artifact_id TEXT NOT NULL REFERENCES workspace_artifacts(id),
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 hash TEXT NOT NULL CHECK(length(hash)=64 AND hash NOT GLOB '*[^0-9a-f]*'),
 size_bytes INTEGER NOT NULL CHECK(size_bytes BETWEEN 0 AND 10485760),
 media_type TEXT NOT NULL CHECK(media_type IN ('text/plain','text/markdown','application/json','text/csv')),
 privacy TEXT NOT NULL CHECK(privacy IN ('normal','local-only')),
 trusted_only INTEGER NOT NULL DEFAULT 0 CHECK(trusted_only IN (0,1)),
 created_at TEXT NOT NULL,
 UNIQUE(id,artifact_id)
);

CREATE TABLE workspace_reservations (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 workspace_id TEXT NOT NULL REFERENCES agent_workspaces(id),
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 size_bytes INTEGER NOT NULL CHECK(size_bytes>=0 AND size_bytes<=1073741824),
 purpose TEXT NOT NULL CHECK(purpose IN ('publication','scratch')),
 state TEXT NOT NULL DEFAULT 'held' CHECK(state IN ('held','consumed','released')),
 created_at TEXT NOT NULL,
 UNIQUE(id,workspace_id,task_id)
);

CREATE TABLE workspace_publications (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 workspace_id TEXT NOT NULL,
 artifact_id TEXT NOT NULL,
 revision_id TEXT NOT NULL UNIQUE,
 base_revision TEXT,
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 reservation_id TEXT NOT NULL UNIQUE,
 effect_key TEXT NOT NULL CHECK(length(CAST(effect_key AS BLOB)) BETWEEN 1 AND 128),
 operation TEXT NOT NULL DEFAULT 'workspace_write_commit' CHECK(operation IN ('workspace_write_commit','workspace_patch')),
 state TEXT NOT NULL DEFAULT 'staging' CHECK(state IN ('staging','prepared','bytes_published','committed','aborted')),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(task_id,operation,effect_key),
 FOREIGN KEY(artifact_id,workspace_id) REFERENCES workspace_artifacts(id,workspace_id),
 FOREIGN KEY(revision_id,artifact_id) REFERENCES workspace_revisions(id,artifact_id),
 FOREIGN KEY(base_revision,artifact_id) REFERENCES workspace_revisions(id,artifact_id),
 FOREIGN KEY(reservation_id,workspace_id,task_id) REFERENCES workspace_reservations(id,workspace_id,task_id)
);

CREATE TABLE workspace_provenance (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 revision_id TEXT NOT NULL REFERENCES workspace_revisions(id),
 source_revision TEXT REFERENCES workspace_revisions(id),
 source_id TEXT NOT NULL DEFAULT '',
 source_version TEXT NOT NULL DEFAULT '',
 privacy TEXT NOT NULL CHECK(privacy IN ('normal','local-only')),
 trusted_only INTEGER NOT NULL DEFAULT 0 CHECK(trusted_only IN (0,1)),
 audience TEXT NOT NULL CHECK(audience<>''),
 created_at TEXT NOT NULL,
 CHECK((source_revision IS NOT NULL AND source_id='' AND source_version='') OR (source_revision IS NULL AND source_id<>'' AND source_version<>'')),
 CHECK(source_revision IS NULL OR source_revision<>revision_id)
);

CREATE TABLE workspace_receipts (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 publication_id TEXT NOT NULL UNIQUE REFERENCES workspace_publications(id),
 recorded_at TEXT NOT NULL
);

CREATE TABLE integration_secret_copies (
    secret         INTEGER NOT NULL REFERENCES integration_secrets(seq),
    node           TEXT NOT NULL,
    provision_state TEXT NOT NULL CHECK (provision_state IN ('sending', 'provisioned', 'failed')),
    -- The store version the node reported for this name; the erase is
    -- fenced to it so it can never destroy a newer write.
    node_version   INTEGER NOT NULL DEFAULT 0,
    sent_at        TEXT NOT NULL,
    erase_state    TEXT NOT NULL DEFAULT '' CHECK (erase_state IN ('', 'pending', 'erased', 'unverified')),
    -- The connection lease epoch that fenced this copy. Recorded so the
    -- record shows the fence came first.
    fenced_epoch   INTEGER NOT NULL DEFAULT 0,
    erase_requested_at TEXT NOT NULL DEFAULT '',
    erased_at      TEXT NOT NULL DEFAULT '',
    -- hex(nonce) ":" hex(evidence) of the verified erase. Evidence, not
    -- a value: it is a digest over the name, the nonce and "absent".
    evidence       TEXT NOT NULL DEFAULT '',
    erase_detail   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (secret, node),
    CHECK (node <> '')
);

CREATE TABLE personal_background_authorizations (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    connection          INTEGER NOT NULL REFERENCES integration_connections(seq),
    -- The Person who authorized. Always the connection's owner: nobody
    -- else's authorization means anything for a Personal credential.
    person              TEXT NOT NULL,
    credential          INTEGER NOT NULL REFERENCES integration_secrets(seq),
    credential_version  INTEGER NOT NULL,
    -- Canonical JSON of the source scope (folders, sender and subject
    -- filters), sorted and de-duplicated before it is stored.
    source_scope        TEXT NOT NULL,
    processing_location TEXT NOT NULL CHECK (processing_location IN ('local', 'cloud')),
    -- Who may receive or learn from results: the owning Person alone.
    result_audience     TEXT NOT NULL CHECK (result_audience = 'aud_owner'),
    -- The Agent (butlers.seq) that runs it.
    agent               INTEGER NOT NULL,
    target_calendar     TEXT NOT NULL,
    executable          TEXT NOT NULL,
    -- The Owner-approved subscription this is for, and a digest of its
    -- content at authorization time (approval fields excluded, since the
    -- Owner approves after the Person authorizes).
    subscription        INTEGER NOT NULL,
    subscription_digest TEXT NOT NULL,
    -- sha256 over every material field above except the subscription
    -- binding. This is what the subscription names.
    digest              TEXT NOT NULL,
    created_at          TEXT NOT NULL,
    revoked_at          TEXT NOT NULL DEFAULT '',
    revoked_by          TEXT NOT NULL DEFAULT '',
    revoked_reason      TEXT NOT NULL DEFAULT '',
    CHECK (person <> ''),
    CHECK (source_scope <> ''),
    CHECK (target_calendar <> ''),
    CHECK (executable <> ''),
    CHECK (digest <> ''),
    CHECK (revoked_at = '' OR revoked_reason <> '')
);

CREATE TABLE search_providers (
 id TEXT PRIMARY KEY,
 config_json TEXT NOT NULL,
 config_revision TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('unconfigured','untested','ready','unavailable')),
 test_revision TEXT NOT NULL DEFAULT '',
 tested_config_revision TEXT NOT NULL DEFAULT ''
);

CREATE TABLE research_selections (
 scope TEXT PRIMARY KEY,
 provider_id TEXT,
 config_revision TEXT NOT NULL
);

CREATE TABLE agent_subscription_schedules (
    subscription         INTEGER PRIMARY KEY REFERENCES agent_subscriptions(id) ON DELETE CASCADE,
    -- next_due_at: '' means due at the next tick (a freshly approved
    -- subscription). A claim sets it to claim-time + cadence, so however
    -- long Keystone was down, the missed intervals collapse into at most
    -- ONE catch-up run rather than a backlog replay.
    next_due_at          TEXT NOT NULL DEFAULT '',
    -- hold is a stop a person (or a permanent failure) put on the
    -- schedule: '' | paused | auth_failed | config_failed. It survives
    -- restarts and is lifted only by an explicit resume.
    hold                 TEXT NOT NULL DEFAULT ''
        CHECK (hold IN ('', 'paused', 'auth_failed', 'config_failed')),
    hold_reason          TEXT NOT NULL DEFAULT '',
    held_at              TEXT NOT NULL DEFAULT '',
    held_by              TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    -- Health, for a Person reading why nothing happened.
    last_run_at          TEXT NOT NULL DEFAULT '',
    last_success_at      TEXT NOT NULL DEFAULT '',
    last_failure_at      TEXT NOT NULL DEFAULT '',
    last_outcome         TEXT NOT NULL DEFAULT '',
    last_error           TEXT NOT NULL DEFAULT '',
    -- blocked_* is the dispatch gate's most recent refusal, cleared by
    -- the next successful claim. A block is not a failure: it does not
    -- count toward backoff and it does not move next_due_at.
    blocked_at           TEXT NOT NULL DEFAULT '',
    blocked_reason       TEXT NOT NULL DEFAULT '',
    updated_at           TEXT NOT NULL
);

CREATE TABLE agent_subscription_runs (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    subscription            INTEGER NOT NULL REFERENCES agent_subscriptions(id) ON DELETE CASCADE,
    -- The exact approved version, source and fences this run was
    -- dispatched under. Completion re-checks every one of them live.
    subscription_version    INTEGER NOT NULL,
    source_connection       TEXT NOT NULL DEFAULT '',
    connection_epoch        INTEGER NOT NULL DEFAULT 0,
    person_id               TEXT,
    coordinator_epoch       INTEGER NOT NULL DEFAULT 0,
    -- The checkpoint version the run was handed. Its commit is a
    -- compare-and-swap against this number.
    base_checkpoint_version INTEGER NOT NULL DEFAULT 0,
    state                   TEXT NOT NULL
        CHECK (state IN ('running', 'succeeded', 'failed', 'abandoned', 'fenced')),
    outcome                 TEXT NOT NULL DEFAULT '',
    task_id                 TEXT NOT NULL DEFAULT '',
    error                   TEXT NOT NULL DEFAULT '',
    due_at                  TEXT NOT NULL DEFAULT '',
    started_at              TEXT NOT NULL,
    lease_expires_at        TEXT NOT NULL,
    finished_at             TEXT NOT NULL DEFAULT ''
);

CREATE TABLE agent_subscription_checkpoints (
    subscription    INTEGER NOT NULL REFERENCES agent_subscriptions(id) ON DELETE CASCADE,
    -- source is the source_connection the cursor is FOR ('' for none).
    -- A changed source starts from no checkpoint rather than inheriting
    -- another mailbox's position.
    source          TEXT NOT NULL,
    -- version increments on every commit; a run commits only if it
    -- still equals the version it was handed.
    version         INTEGER NOT NULL,
    -- cursor_json is opaque here (an IMAP cursor is UIDVALIDITY + UID
    -- per folder; nothing in runtimedb parses it).
    cursor_json     TEXT NOT NULL,
    committed_at    TEXT NOT NULL,
    committed_by_run INTEGER NOT NULL,
    PRIMARY KEY (subscription, source)
);

CREATE TABLE workspace_task_sources (
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 PRIMARY KEY(task,revision)
);

CREATE TABLE workspace_session_sources (
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 PRIMARY KEY(session,revision)
);

CREATE TABLE workspace_revision_sessions (
 revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 session INTEGER NOT NULL REFERENCES butler_sessions(seq),
 PRIMARY KEY(revision,session)
);

CREATE TABLE workspace_revision_memories (
 revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 memory INTEGER NOT NULL REFERENCES butler_memory(seq),
 PRIMARY KEY(revision,memory)
);

CREATE TABLE workspace_revision_inputs (
 revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 source_revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 PRIMARY KEY(revision,source_revision),
 CHECK(revision<>source_revision)
);

CREATE TABLE mail_monitor_messages (
    connection    INTEGER NOT NULL REFERENCES integration_connections(seq) ON DELETE CASCADE,
    -- fingerprint is the adapter's keyed, per-connection identity
    -- ("mfp1_…"): stable across reconnects, duplicate deliveries and
    -- UIDVALIDITY resets, meaningless to any other connection.
    fingerprint   TEXT NOT NULL CHECK (fingerprint LIKE 'mfp1\_%' ESCAPE '\' AND length(fingerprint) <= 128),
    person_id     TEXT NOT NULL REFERENCES butler_members(person_id),
    -- state: candidate (produced or updated a candidate), duplicate_event
    -- (an event already on record), cancelled (an iTIP CANCEL applied),
    -- no_event, skipped (with reason), extraction_failed, or retrying (a
    -- transient model failure; attempts bounds it).
    state         TEXT NOT NULL CHECK (state IN ('candidate', 'duplicate_event', 'cancelled', 'no_event',
                      'skipped', 'extraction_failed', 'retrying')),
    -- reason is a fixed category (no_local_model, oversized, past_event,
    -- ics_unusable, …), never message text.
    reason        TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 64),
    -- source is how the message was read: ics, model or '' (neither).
    source        TEXT NOT NULL DEFAULT '' CHECK (source IN ('', 'ics', 'model')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    run           INTEGER NOT NULL DEFAULT 0,
    first_seen_at TEXT NOT NULL,
    processed_at  TEXT NOT NULL,
    PRIMARY KEY (connection, fingerprint)
);

CREATE TABLE mail_candidates (
    id                TEXT PRIMARY KEY CHECK (id LIKE 'mc\_%' ESCAPE '\' AND length(id) <= 40),
    person_id         TEXT NOT NULL REFERENCES butler_members(person_id),
    connection        INTEGER NOT NULL REFERENCES integration_connections(seq) ON DELETE CASCADE,
    -- The source message: its opaque, connection-bound provider
    -- reference, its fingerprint and folder. Nothing else about it.
    source_ref        TEXT NOT NULL CHECK (length(source_ref) <= 512),
    fingerprint       TEXT NOT NULL CHECK (length(fingerprint) <= 128),
    source            TEXT NOT NULL CHECK (source IN ('ics', 'model')),
    -- Correlation keys, each a sha256 hex digest scoped to the
    -- connection: the invitation UID, the message's own Message-ID, its
    -- sender address and its normalized subject. They let a correction
    -- or an iTIP CANCEL find the candidate it corrects without the
    -- database holding the header values themselves.
    uid_key           TEXT NOT NULL DEFAULT '',
    ics_sequence      INTEGER NOT NULL DEFAULT 0,
    message_key       TEXT NOT NULL DEFAULT '',
    sender_key        TEXT NOT NULL DEFAULT '',
    subject_key       TEXT NOT NULL DEFAULT '',
    -- event_key digests the event itself (title, times, all-day) so a
    -- reminder or resend of an event already asked about — including a
    -- dismissed one, whose fields are blanked — is not asked again.
    event_key         TEXT NOT NULL DEFAULT '',
    target_calendar   TEXT NOT NULL CHECK (length(target_calendar) <= 255),
    -- revision is the current field revision; each Edit or correction
    -- is a new row in mail_candidate_revisions. A review is bound to
    -- (id, revision).
    revision          INTEGER NOT NULL DEFAULT 1,
    state             TEXT NOT NULL CHECK (state IN ('proposed', 'needs_details', 'accepted', 'created',
                          'dismissed', 'superseded', 'expired', 'cancelled')),
    state_reason      TEXT NOT NULL DEFAULT '' CHECK (length(state_reason) <= 200),
    -- supersedes / superseded_by link a correction to what it corrects.
    supersedes        TEXT NOT NULL DEFAULT '',
    superseded_by     TEXT NOT NULL DEFAULT '',
    -- update_pending: '' | update | cancel. Set on a candidate that
    -- already created (or was accepted to create) an event when a later
    -- correction or cancellation arrives; S06 proposes the exact
    -- calendar change. Nothing edits the event silently.
    update_pending    TEXT NOT NULL DEFAULT '' CHECK (update_pending IN ('', 'update', 'cancel')),
    -- announced_at: when the candidate-ready hook accepted it. '' means
    -- the private review has not been handed off yet.
    announced_at      TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    -- expires_at: an unresolved candidate expires at its start or after
    -- the unresolved window, whichever is earlier.
    expires_at        TEXT NOT NULL,
    resolved_at       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE mail_candidate_revisions (
    candidate       TEXT NOT NULL REFERENCES mail_candidates(id) ON DELETE CASCADE,
    revision        INTEGER NOT NULL,
    -- The minimal event: bounded, sanitized, calendar-safe.
    title           TEXT NOT NULL CHECK (length(title) <= 200),
    -- start_at / end_at are RFC3339 instants, '' when unknown.
    -- duration_minutes is set when only a duration is known.
    start_at        TEXT NOT NULL DEFAULT '',
    end_at          TEXT NOT NULL DEFAULT '',
    duration_minutes INTEGER NOT NULL DEFAULT 0 CHECK (duration_minutes BETWEEN 0 AND 20160),
    all_day         INTEGER NOT NULL DEFAULT 0 CHECK (all_day IN (0, 1)),
    tz              TEXT NOT NULL DEFAULT '' CHECK (length(tz) <= 64),
    location        TEXT NOT NULL DEFAULT '' CHECK (length(location) <= 200),
    notes           TEXT NOT NULL DEFAULT '' CHECK (length(notes) <= 500),
    confidence      REAL NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
    -- JSON arrays of a closed field vocabulary.
    missing_json    TEXT NOT NULL DEFAULT '[]',
    ambiguous_json  TEXT NOT NULL DEFAULT '[]',
    -- author: 'monitor' for extraction, otherwise the Person's handle.
    author          TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (candidate, revision)
);

CREATE TABLE mail_candidate_excerpts (
    candidate   TEXT PRIMARY KEY REFERENCES mail_candidates(id) ON DELETE CASCADE,
    sealed      BLOB NOT NULL CHECK (length(sealed) <= 2048),
    expires_at  TEXT NOT NULL
);

CREATE TABLE mail_candidate_audit (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    candidate   TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    action      TEXT NOT NULL,
    from_state  TEXT NOT NULL DEFAULT '',
    to_state    TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    at          TEXT NOT NULL
);

CREATE TABLE workspace_write_sessions (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 workspace_id TEXT NOT NULL REFERENCES agent_workspaces(id),
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 artifact_id TEXT NOT NULL,
 path TEXT NOT NULL,
 base_revision TEXT NOT NULL,
 begin_key TEXT NOT NULL CHECK(length(CAST(begin_key AS BLOB)) BETWEEN 1 AND 128),
 request_hash TEXT NOT NULL CHECK(length(request_hash)=64),
 config_revision TEXT NOT NULL CHECK(config_revision<>''),
 max_bytes INTEGER NOT NULL CHECK(max_bytes BETWEEN 1 AND 10485760),
 privacy TEXT NOT NULL CHECK(privacy IN ('normal','local-only')),
 trusted_only INTEGER NOT NULL CHECK(trusted_only IN (0,1)),
 provenance TEXT NOT NULL CHECK(json_valid(provenance)),
 state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open','sealed','committed','aborted')),
 created_at TEXT NOT NULL,
 UNIQUE(task_id,begin_key),
 UNIQUE(id,workspace_id,task_id)
);

CREATE TABLE workspace_write_chunks (
 session_id TEXT NOT NULL REFERENCES workspace_write_sessions(id),
 chunk_index INTEGER NOT NULL CHECK(chunk_index BETWEEN 0 AND 10239),
 object_id TEXT NOT NULL UNIQUE CHECK(length(object_id)=32 AND object_id NOT GLOB '*[^0-9a-f]*'),
 hash TEXT NOT NULL CHECK(length(hash)=64 AND hash NOT GLOB '*[^0-9a-f]*'),
 size_bytes INTEGER NOT NULL CHECK(size_bytes BETWEEN 0 AND 1024),
 reservation_id TEXT NOT NULL UNIQUE REFERENCES workspace_reservations(id),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','durable')),
 PRIMARY KEY(session_id,chunk_index)
);

CREATE TABLE workspace_write_commit_keys (
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 effect_key TEXT NOT NULL CHECK(length(CAST(effect_key AS BLOB)) BETWEEN 1 AND 128),
 session_id TEXT NOT NULL REFERENCES workspace_write_sessions(id),
 PRIMARY KEY(task_id,effect_key)
);

CREATE TABLE workspace_write_chunk_keys (
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 effect_key TEXT NOT NULL CHECK(length(CAST(effect_key AS BLOB)) BETWEEN 1 AND 128),
 session_id TEXT NOT NULL,
 chunk_index INTEGER NOT NULL,
 PRIMARY KEY(task_id,effect_key),
 FOREIGN KEY(session_id,chunk_index) REFERENCES workspace_write_chunks(session_id,chunk_index)
);

CREATE TABLE mail_reviews (
    id                    TEXT PRIMARY KEY CHECK (id LIKE 'mr\_%' ESCAPE '\' AND length(id) <= 40),
    candidate             TEXT NOT NULL REFERENCES mail_candidates(id) ON DELETE CASCADE,
    person_id             TEXT NOT NULL REFERENCES butler_members(person_id),
    revision              INTEGER NOT NULL,
    -- kind: create (Add a new event), update (apply a correction to the
    -- event an earlier candidate created) or cancel (remove it).
    kind                  TEXT NOT NULL CHECK (kind IN ('create', 'update', 'cancel')),
    target_calendar       TEXT NOT NULL CHECK (length(target_calendar) <= 255),
    -- The existing event an update or cancel acts on: its deterministic
    -- UID. '' for create, whose UID is derived from the candidate.
    event_uid             TEXT NOT NULL DEFAULT '' CHECK (length(event_uid) <= 128),
    fields_digest         TEXT NOT NULL CHECK (length(fields_digest) = 64),
    authorization_digest  TEXT NOT NULL,
    subscription_version  INTEGER NOT NULL,
    -- route: how the review reached the Person — 'chat:<binding id>' for
    -- their one-to-one Connection, or 'notice' for the in-app notice
    -- alone. A chat reply must arrive on the same binding.
    route                 TEXT NOT NULL CHECK (length(route) <= 64),
    -- availability at issue: free, busy (conflicts named below),
    -- unavailable (the calendar did not answer) or not_checked.
    availability          TEXT NOT NULL CHECK (availability IN ('free', 'busy', 'unavailable', 'not_checked')),
    -- The conflicts this Person may see: calendar id, title and times,
    -- read from calendars they can read now. Bounded; no other content.
    conflicts_json        TEXT NOT NULL DEFAULT '[]' CHECK (length(conflicts_json) <= 8192),
    checked_calendars     TEXT NOT NULL DEFAULT '[]' CHECK (length(checked_calendars) <= 4096),
    notice                INTEGER NOT NULL DEFAULT 0,
    issued_at             TEXT NOT NULL,
    expires_at            TEXT NOT NULL,
    state                 TEXT NOT NULL CHECK (state IN ('open', 'used', 'revoked', 'expired')),
    state_reason          TEXT NOT NULL DEFAULT '' CHECK (length(state_reason) <= 200),
    used_action           TEXT NOT NULL DEFAULT '',
    used_by               TEXT NOT NULL DEFAULT '',
    used_via              TEXT NOT NULL DEFAULT '',
    closed_at             TEXT NOT NULL DEFAULT ''
);

CREATE TABLE mail_review_tokens (
    -- sha256 hex of the token. The token itself is never stored.
    token_hash  TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    review      TEXT NOT NULL REFERENCES mail_reviews(id) ON DELETE CASCADE,
    action      TEXT NOT NULL CHECK (action IN ('add', 'edit', 'dismiss', 'apply')),
    UNIQUE (review, action)
);

CREATE TABLE mail_candidate_effects (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    candidate     TEXT NOT NULL REFERENCES mail_candidates(id) ON DELETE CASCADE,
    revision      INTEGER NOT NULL,
    review        TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('create', 'update', 'cancel')),
    -- dispatching: recorded, the write may or may not have reached the
    -- calendar. confirmed: the calendar answered. reconciled: an
    -- indeterminate write was found (or found absent, for a cancel) by
    -- looking the UID up. returned: found absent and handed back to
    -- review. failed: refused by the calendar; nothing changed.
    state         TEXT NOT NULL CHECK (state IN ('dispatching', 'confirmed', 'reconciled', 'returned', 'failed')),
    calendar      TEXT NOT NULL,
    uid           TEXT NOT NULL CHECK (length(uid) <= 128),
    href          TEXT NOT NULL DEFAULT '' CHECK (length(href) <= 1024),
    etag          TEXT NOT NULL DEFAULT '' CHECK (length(etag) <= 256),
    fields_digest TEXT NOT NULL,
    actor         TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    started_at    TEXT NOT NULL,
    finished_at   TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '' CHECK (length(detail) <= 200)
);

CREATE TABLE research_budgets (
    task INTEGER PRIMARY KEY REFERENCES butler_tasks(seq),
    searches_used INTEGER NOT NULL DEFAULT 0 CHECK(searches_used >= 0),
    fetches_used INTEGER NOT NULL DEFAULT 0 CHECK(fetches_used >= 0),
    searches_allowed INTEGER NOT NULL CHECK(searches_allowed BETWEEN 1 AND 100),
    fetches_allowed INTEGER NOT NULL CHECK(fetches_allowed BETWEEN 1 AND 200),
    config_searches INTEGER NOT NULL CHECK(config_searches BETWEEN 1 AND 100),
    config_fetches INTEGER NOT NULL CHECK(config_fetches BETWEEN 1 AND 200),
    revision INTEGER NOT NULL DEFAULT 1,
    wait_seq INTEGER REFERENCES butler_task_waits(seq)
);

CREATE TABLE research_attempts (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    task INTEGER NOT NULL REFERENCES research_budgets(task),
    ledger_attempt INTEGER NOT NULL REFERENCES butler_task_attempts(seq),
    kind TEXT NOT NULL CHECK(kind IN ('search','fetch')),
    state TEXT NOT NULL DEFAULT 'reserved' CHECK(state IN ('reserved','succeeded','failed','cancelled')),
    created_at TEXT NOT NULL,
    ended_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE research_checkpoints (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    task INTEGER NOT NULL REFERENCES butler_tasks(seq),
    created_at TEXT NOT NULL,
    steps_json TEXT NOT NULL,
    next_action TEXT NOT NULL CHECK(next_action IN ('search','fetch','read','report'))
);

CREATE TABLE research_continuations (
    task INTEGER NOT NULL REFERENCES research_budgets(task),
    revision INTEGER NOT NULL,
    actor_person_id TEXT NOT NULL REFERENCES butler_members(person_id),
    created_at TEXT NOT NULL,
    searches_allowed INTEGER NOT NULL,
    fetches_allowed INTEGER NOT NULL,
    PRIMARY KEY(task,revision)
);

CREATE TABLE workspace_quota_policy (
 agent_id INTEGER PRIMARY KEY REFERENCES butlers(seq),
 limit_bytes INTEGER NOT NULL CHECK(limit_bytes BETWEEN 1 AND 1099511627776),
 config_revision TEXT NOT NULL CHECK(config_revision<>''),
 updated_at TEXT NOT NULL
);

CREATE TABLE workspace_directories (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 workspace_id TEXT NOT NULL REFERENCES agent_workspaces(id),
 path TEXT NOT NULL CHECK(path<>''),
 revision TEXT NOT NULL UNIQUE CHECK(length(revision)=32 AND revision NOT GLOB '*[^0-9a-f]*'),
 state TEXT NOT NULL DEFAULT 'committed' CHECK(state IN ('committed','removed')),
 created_at TEXT NOT NULL,
 UNIQUE(id,workspace_id)
);

CREATE TABLE workspace_namespace_intents (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 workspace_id TEXT NOT NULL REFERENCES agent_workspaces(id),
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 operation TEXT NOT NULL CHECK(operation IN ('workspace_mkdir','workspace_rmdir','workspace_move')),
 effect_key TEXT NOT NULL CHECK(length(CAST(effect_key AS BLOB)) BETWEEN 1 AND 128),
 request_hash TEXT NOT NULL CHECK(length(request_hash)=64),
 path TEXT NOT NULL CHECK(path<>''),
 destination TEXT NOT NULL DEFAULT '',
 artifact_id TEXT REFERENCES workspace_artifacts(id),
 expected_revision TEXT NOT NULL DEFAULT '',
 directory_id TEXT NOT NULL DEFAULT '',
 directory_revision TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'prepared' CHECK(state IN ('prepared','committed','aborted')),
 created_at TEXT NOT NULL,
 UNIQUE(task_id,operation,effect_key),
 CHECK((operation='workspace_move' AND artifact_id IS NOT NULL AND destination<>'' AND expected_revision<>'' AND directory_id='' AND directory_revision='') OR
       (operation IN ('workspace_mkdir','workspace_rmdir') AND artifact_id IS NULL AND destination='' AND expected_revision='' AND length(directory_id)=32 AND length(directory_revision)=32))
);

INSERT INTO "notice_kinds" ("kind", "urgent", "relevance", "description", "updated_at", "updated_by") VALUES ('owner.review', 0, 75, 'A Plan is waiting for Owner review', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'migration');
INSERT INTO "person_groups" ("group_id", "name", "seed_key", "created_at") VALUES ('grp_0000000000000001', 'Family', 'family', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
INSERT INTO "person_groups" ("group_id", "name", "seed_key", "created_at") VALUES ('grp_0000000000000002', 'Adults', 'adults', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
INSERT INTO "person_groups" ("group_id", "name", "seed_key", "created_at") VALUES ('grp_0000000000000003', 'Kids', 'kids', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
INSERT INTO "audiences" ("audience_id", "kind", "label", "created_at") VALUES ('aud_owner', 'owner', '', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
INSERT INTO "audiences" ("audience_id", "kind", "label", "created_at") VALUES ('aud_household', 'household', '', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
INSERT INTO "owner_review_preference" ("singleton", "binding_id", "console_url", "generation") VALUES (1, NULL, '', 0);
INSERT INTO "routine_execution_state" ("singleton", "cursor", "initialized") VALUES (1, 0, 0);
INSERT INTO "household_execution_stop" ("singleton", "active", "generation", "entered_at", "entered_by", "reason", "released_at", "released_by") VALUES (1, 0, 0, '', '', '', '', '');

CREATE VIEW memory_source_connections AS
SELECT m.seq AS memory, COALESCE(t.origin_connection_ref, 0) AS connection_ref,
       COALESCE(t.origin_connection, '') AS connection_name
FROM butler_memory m LEFT JOIN butler_tasks t
  ON m.source_kind='conversation' AND m.source_id='bt-' || t.seq
WHERE m.source_id NOT LIKE 'bs-%' OR m.source_kind <> 'conversation'
UNION
SELECT m.seq, COALESCE(p.connection_ref, 0), COALESCE(p.connection_name, '')
FROM butler_memory m JOIN butler_tasks t
  ON m.source_kind='conversation' AND m.source_id='bt-' || t.seq
LEFT JOIN butler_session_connections p ON p.session=t.session AND p.direction='inbound'
WHERE t.session IS NOT NULL
UNION
SELECT m.seq, COALESCE(p.connection_ref, 0), COALESCE(p.connection_name, '')
FROM butler_memory m LEFT JOIN butler_sessions s ON m.source_id='bs-' || s.seq
LEFT JOIN butler_session_connections p ON p.session=s.seq AND p.direction='inbound'
WHERE m.source_kind='conversation' AND m.source_id LIKE 'bs-%';

CREATE VIEW privacy_edges AS
 SELECT 'm:'||derivative AS child, 'm:'||source AS parent FROM butler_memory_derivations
 UNION SELECT 'm:'||memory, 's:'||session FROM memory_origin_sessions
 UNION SELECT 's:'||l.session, 'm:'||u.memory FROM session_task_sources l JOIN task_memory_sources u ON u.task=l.task
 UNION SELECT 's:'||l.session, 'm:'||m.seq FROM session_task_sources l JOIN butler_memory m ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||l.task
 UNION SELECT 'm:'||m.seq, 's:'||l.session FROM session_task_sources l JOIN butler_memory m ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||l.task
 UNION SELECT 'm:'||superseded_by, 'm:'||seq FROM butler_memory WHERE superseded_by IS NOT NULL
 UNION SELECT 's:'||session, 'm:'||memory FROM session_memory_sources
 UNION SELECT 'm:'||m.seq, 'm:'||u.memory FROM butler_memory m JOIN butler_tasks t ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||t.seq JOIN task_memory_sources u ON u.task=t.seq
 UNION SELECT 's:'||t.session, 'm:'||u.memory FROM task_memory_sources u JOIN butler_tasks t ON t.seq=u.task WHERE t.session IS NOT NULL
 UNION SELECT 'm:'||m.seq, 's:'||t.session FROM butler_memory m JOIN butler_tasks t
 ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||t.seq WHERE t.session IS NOT NULL
 UNION SELECT 'm:'||m.seq, 's:'||s.seq FROM butler_memory m JOIN butler_sessions s
 ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bs-'||s.seq
 -- A remembered fact remains present in the conversation it came from.
 -- Reverse provenance makes later restrictions apply to resumed history too.
 UNION SELECT 's:'||t.session, 'm:'||m.seq FROM butler_memory m JOIN butler_tasks t
 ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||t.seq WHERE t.session IS NOT NULL
 UNION SELECT 's:'||s.seq, 'm:'||m.seq FROM butler_memory m JOIN butler_sessions s
 ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bs-'||s.seq;

CREATE VIEW workspace_privacy_edges AS
 SELECT 'wr:'||revision_id AS child,'wr:'||source_revision AS parent FROM workspace_provenance WHERE source_revision IS NOT NULL
 UNION SELECT 'wr:'||revision_id,'wr:'||base_revision FROM workspace_publications WHERE base_revision IS NOT NULL
 UNION SELECT 'wr:'||id,'wa:'||artifact_id FROM workspace_revisions
 UNION SELECT 'wr:'||id,'t:'||task_id FROM workspace_revisions
 -- A writer has already used its revision's sources even without a read.
 UNION SELECT 't:'||task_id,'wr:'||id FROM workspace_revisions
 UNION SELECT 'wr:'||revision,'s:'||session FROM workspace_revision_sessions
 UNION SELECT 't:'||task,'wr:'||revision FROM workspace_task_sources
 UNION SELECT 's:'||session,'wr:'||revision FROM workspace_session_sources
 UNION SELECT 's:'||link.session,'wr:'||source.revision FROM session_task_sources link JOIN workspace_task_sources source ON source.task=link.task
 UNION SELECT 't:'||task,'m:'||memory FROM task_memory_sources
 UNION SELECT 't:'||seq,'s:'||session FROM butler_tasks WHERE session IS NOT NULL
 UNION SELECT 't:'||task,'s:'||session FROM session_task_sources
 UNION SELECT 't:'||t.seq,'m:'||m.seq FROM butler_tasks t JOIN butler_memory m ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||t.seq
 UNION SELECT 'm:'||m.seq,'t:'||t.seq FROM butler_memory m JOIN butler_tasks t ON m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||t.seq
 UNION SELECT 'wr:'||revision,'m:'||memory FROM workspace_revision_memories
 UNION SELECT 'wr:'||revision,'wr:'||source_revision FROM workspace_revision_inputs;

CREATE VIEW workspace_content_privacy AS
 SELECT 'wr:'||id AS object,privacy='local-only' AS local_only,trusted_only FROM workspace_revisions
 UNION ALL SELECT 'wr:'||revision_id,privacy='local-only',trusted_only FROM workspace_provenance;

CREATE INDEX backup_runs_by_plan ON backup_runs (plan_name, run_kind, seq);

CREATE INDEX butler_sessions_by_caller ON butler_sessions (acting_for, seq);

CREATE INDEX household_events_by_type ON household_events (type, seq);

CREATE INDEX household_events_by_subject ON household_events (subject, seq);

CREATE INDEX butler_sessions_by_surface ON butler_sessions (surface, seq);

CREATE UNIQUE INDEX butler_session_turns_order ON butler_session_turns (session, ordinal);

CREATE UNIQUE INDEX butler_task_steps_order ON butler_task_steps (task, ordinal);

CREATE UNIQUE INDEX butler_task_attempts_order ON butler_task_attempts (step, attempt);

CREATE INDEX butler_task_attempts_by_task ON butler_task_attempts (task, seq);

CREATE INDEX butler_task_transitions_by_task ON butler_task_transitions (task, seq);

CREATE INDEX butler_task_feedback_by_task ON butler_task_feedback (task, seq);

CREATE INDEX butler_task_feedback_by_label ON butler_task_feedback (label, seq);

CREATE UNIQUE INDEX butler_evaluations_name ON butler_evaluations (name);

CREATE INDEX butler_debug_capture_by_task ON butler_debug_capture (task, seq);

CREATE INDEX butler_memory_derivations_by_source ON butler_memory_derivations (source);

CREATE INDEX butler_memory_digest_pending ON butler_memory_digest (delivered_at, seq);

CREATE INDEX butler_index_jobs_queue ON butler_index_jobs (state, priority, seq);

CREATE UNIQUE INDEX integration_capabilities_key ON integration_capabilities (connection, kind, name);

CREATE INDEX integration_work_queue ON integration_work (connection, state, seq);

CREATE INDEX integration_work_correlation ON integration_work (correlation_id);

CREATE UNIQUE INDEX integration_placements_node ON integration_placements (connection, node);

CREATE UNIQUE INDEX integration_placements_holder
    ON integration_placements (connection) WHERE holder = 1;

CREATE UNIQUE INDEX integration_secrets_ref ON integration_secrets (connection, member, secret_ref);

CREATE INDEX deletion_journal_subject ON deletion_journal (subject_kind, subject_id);

CREATE UNIQUE INDEX butler_tool_scope_key ON butler_tool_scope (butler, tool);

CREATE UNIQUE INDEX butler_connection_scope_live
    ON butler_connection_scope (butler, connection) WHERE revoked_at = '';

CREATE INDEX household_routines_butler ON household_routines (butler);

CREATE UNIQUE INDEX butler_addresses_live
    ON butler_addresses (transport, external_id) WHERE revoked_at = '';

CREATE INDEX butler_addresses_butler ON butler_addresses (butler, revoked_at);

CREATE INDEX butler_pool_disclosures_memory ON butler_pool_disclosures (memory, seq);

CREATE INDEX butler_sessions_butler ON butler_sessions (butler, seq);

CREATE INDEX models_assignable ON models (enabled, kind, name);

CREATE INDEX butler_role_changes_by_person ON butler_role_changes (person_id, seq);

CREATE UNIQUE INDEX butler_access_live ON butler_access (butler, person_id)
    WHERE revoked_at = '';

CREATE INDEX butler_access_person ON butler_access (person_id, revoked_at);

CREATE INDEX butler_members_admitted ON butler_members (admitted, member);

CREATE INDEX butler_members_active_owners
    ON butler_members (role, admitted, disabled_at, offboarded_at);

CREATE INDEX butler_identity_links_by_person ON butler_identity_links (person_id, seq);

CREATE UNIQUE INDEX butler_identity_links_live
    ON butler_identity_links (link_type, external_id) WHERE revoked_at = '';

CREATE INDEX butler_conversations_by_person ON butler_conversations (person_id, seq);

CREATE INDEX butler_handoffs_by_conversation ON butler_handoffs (conversation, seq);

CREATE INDEX household_review_tokens_plan ON household_review_tokens (plan_id);

CREATE INDEX offboarding_records_person ON offboarding_records (person_id, seq);

CREATE INDEX notices_by_recipient ON notices (recipient_person_id, seq);

CREATE UNIQUE INDEX notices_dedupe ON notices (dedupe_key);

CREATE UNIQUE INDEX chat_bindings_live
    ON chat_bindings (connection, account) WHERE revoked_at = '';

CREATE INDEX chat_bindings_by_person ON chat_bindings (person_id, id);

CREATE INDEX chat_bindings_by_connection ON chat_bindings (connection, id);

CREATE UNIQUE INDEX chat_outbound_dedupe ON chat_outbound (dedupe_key);

CREATE INDEX chat_outbound_pending ON chat_outbound (connection, seq);

CREATE INDEX chat_questions_open ON chat_questions (connection, target, answered_at, id);

CREATE INDEX meter_readings_by_meter ON meter_readings (meter, at);

CREATE UNIQUE INDEX meter_readings_external ON meter_readings (meter, recorded_by, external_id)
    WHERE external_id <> '';

CREATE INDEX commitment_completions_by_commitment ON commitment_completions (commitment, at);

CREATE INDEX commitment_bookings_by_commitment ON commitment_bookings (commitment, booked_at);

CREATE UNIQUE INDEX commitment_completions_by_external
    ON commitment_completions (commitment, external_id)
    WHERE external_id <> '';

CREATE INDEX person_group_members_by_person ON person_group_members(person_id);

CREATE INDEX audience_members_by_audience ON audience_members (audience_id);

CREATE INDEX butler_memory_by_owner ON butler_memory (owner_person_id, seq);

CREATE INDEX butler_memory_by_source ON butler_memory (source_kind, source_id);

CREATE INDEX butler_memory_eligible ON butler_memory (subject, forgotten_at, seq);

CREATE INDEX butler_memory_scope ON butler_memory (butler, audience, owner_person_id, seq);

CREATE INDEX butler_index_permission ON butler_index (audience, owner_person_id, seq);

CREATE INDEX butler_index_scope ON butler_index (butler, audience, owner_person_id, seq);

CREATE INDEX butler_tasks_butler ON butler_tasks (butler, state);

CREATE INDEX butler_tasks_by_owner ON butler_tasks (owner_person_id, seq);

CREATE INDEX butler_tasks_by_session ON butler_tasks (session, seq);

CREATE INDEX butler_tasks_by_state ON butler_tasks (state, seq);

CREATE INDEX butler_tasks_by_wakeup ON butler_tasks (next_wakeup_at) WHERE next_wakeup_at <> '';

CREATE INDEX butler_task_evidence_by_task ON butler_task_evidence (task, seq);

CREATE INDEX butler_work_trace_by_task ON butler_work_trace (task, seq);

CREATE INDEX service_entitlements_by_service ON service_entitlements (service, dormant_at);

CREATE INDEX service_entitlements_by_subject ON service_entitlements (subject_kind, subject);

CREATE INDEX projected_memberships_by_group ON projected_memberships (ldap_group);

CREATE UNIQUE INDEX connection_invites_open
    ON connection_invites (connection, person_id) WHERE closed_at = '';

CREATE INDEX connection_credentials_fingerprint ON connection_credentials (fingerprint);

CREATE INDEX butlers_live ON butlers (enabled, retired_at, surface);

CREATE UNIQUE INDEX butler_tool_controls_key ON butler_tool_controls (agent_class, tool);

CREATE INDEX butler_tool_grants_live ON butler_tool_grants (scope, scope_id, tool);

CREATE INDEX butler_capability_requests_state ON butler_capability_requests (state, seq);

CREATE UNIQUE INDEX butler_tool_assignments_live
    ON butler_tool_assignments (butler, tool) WHERE revoked_at = '';

CREATE UNIQUE INDEX butler_tool_bundles_live
    ON butler_tool_bundles (butler, service, agent_class) WHERE revoked_at = '';

CREATE INDEX butler_sessions_person_agent
    ON butler_sessions (person_id, agent_id, seq DESC);

CREATE INDEX butler_session_connections_latest
    ON butler_session_connections (session, direction, seq DESC);

CREATE INDEX session_resume_offers_expiry
    ON session_resume_offers (expires_at, consumed_at);

CREATE INDEX memory_constraints_record ON memory_constraints(memory);

CREATE INDEX memory_scope_events_source ON memory_scope_events(source_memory, kind);

CREATE INDEX memory_disclosure_origins_memory ON memory_disclosure_origins(memory);

CREATE UNIQUE INDEX household_secret_person_grant ON household_secret_grants(secret_id,person_id,verb) WHERE person_id IS NOT NULL;

CREATE UNIQUE INDEX household_secret_group_grant ON household_secret_grants(secret_id,group_id,verb) WHERE group_id IS NOT NULL;

CREATE INDEX routine_runs_by_state ON routine_runs(state,seq);

CREATE INDEX offboarding_resource_dispositions_person ON offboarding_resource_dispositions(person_id,resource_kind);

CREATE INDEX runtime_principals_by_butler ON runtime_principals (butler, created_at);

CREATE INDEX agent_deliveries_by_butler ON agent_deliveries (butler, acked_at, created_at);

CREATE INDEX butler_task_waits_by_task ON butler_task_waits (task, seq);

CREATE UNIQUE INDEX butler_task_waits_open ON butler_task_waits (task) WHERE resolved_at = '';

CREATE INDEX agent_subscriptions_by_butler ON agent_subscriptions (butler, enabled);

CREATE INDEX agent_subscriptions_enabled ON agent_subscriptions (enabled) WHERE enabled = 1;

CREATE INDEX household_list_items_by_list ON household_list_items (list, seq);

CREATE INDEX person_handles_by_person ON person_handles(person_id);

CREATE INDEX person_file_dirs_by_dir ON person_file_dirs(dir);

CREATE INDEX backup_source_samples_plan_time
    ON backup_source_samples(plan_name, seq DESC);

CREATE UNIQUE INDEX workspace_artifacts_live_path ON workspace_artifacts(workspace_id,path) WHERE state<>'trashed';

CREATE INDEX workspace_reservations_accounting ON workspace_reservations(workspace_id,state);

CREATE UNIQUE INDEX workspace_publications_pending ON workspace_publications(artifact_id) WHERE state NOT IN ('committed','aborted');

CREATE INDEX workspace_provenance_revision ON workspace_provenance(revision_id);

CREATE UNIQUE INDEX integration_secrets_one_active
    ON integration_secrets (connection, need) WHERE state = 'active';

CREATE UNIQUE INDEX personal_background_authorizations_live
    ON personal_background_authorizations (connection) WHERE revoked_at = '';

CREATE UNIQUE INDEX agent_subscription_runs_one_running
    ON agent_subscription_runs (subscription) WHERE state = 'running';

CREATE INDEX agent_subscription_runs_by_subscription
    ON agent_subscription_runs (subscription, id);

CREATE INDEX mail_monitor_messages_by_age ON mail_monitor_messages (processed_at);

CREATE INDEX mail_candidates_by_person ON mail_candidates (person_id, state, created_at);

CREATE INDEX mail_candidates_by_connection ON mail_candidates (connection, state);

CREATE INDEX mail_candidates_by_uid ON mail_candidates (connection, uid_key) WHERE uid_key <> '';

CREATE INDEX mail_candidates_by_event ON mail_candidates (connection, event_key) WHERE event_key <> '';

CREATE INDEX mail_candidate_audit_by_candidate ON mail_candidate_audit (candidate, seq);

CREATE UNIQUE INDEX mail_reviews_one_open ON mail_reviews (candidate) WHERE state = 'open';

CREATE INDEX mail_reviews_by_person ON mail_reviews (person_id, state);

CREATE UNIQUE INDEX mail_candidate_effects_one_inflight ON mail_candidate_effects (candidate) WHERE state = 'dispatching';

CREATE INDEX mail_candidate_effects_by_candidate ON mail_candidate_effects (candidate, seq);

CREATE INDEX research_attempts_task ON research_attempts(task,seq);

CREATE INDEX research_checkpoints_task ON research_checkpoints(task,seq);

CREATE UNIQUE INDEX workspace_directories_live_path ON workspace_directories(workspace_id,path) WHERE state='committed';

CREATE TRIGGER butler_task_transitions_immutable
BEFORE UPDATE ON butler_task_transitions
BEGIN
    SELECT RAISE(ABORT, 'butler task transitions are append-only (ADR-W030 A11)');
END;

CREATE TRIGGER coordinator_lease_epoch_monotonic
BEFORE UPDATE OF epoch ON coordinator_lease
WHEN NEW.epoch < OLD.epoch
BEGIN
    SELECT RAISE(ABORT, 'coordinator lease epoch cannot move backwards (ADR-W030 A11)');
END;

CREATE TRIGGER butler_task_feedback_immutable
BEFORE UPDATE ON butler_task_feedback
BEGIN
    SELECT RAISE(ABORT, 'butler feedback is append-only (ADR-W030 A11)');
END;

CREATE TRIGGER integration_work_no_reopen
BEFORE UPDATE OF state ON integration_work
WHEN OLD.state IN ('done', 'cancelled', 'expired') AND NEW.state != OLD.state
BEGIN
    SELECT RAISE(ABORT, 'integration work that has finished cannot be reopened (ADR-W030 A5.1)');
END;

CREATE TRIGGER household_routines_ownership_immutable
BEFORE UPDATE OF ownership ON household_routines
WHEN NEW.ownership <> OLD.ownership
BEGIN
    SELECT RAISE(ABORT, 'a routine cannot change between native and Wildstone ownership (ADR-W030 A6)');
END;

CREATE TRIGGER household_routines_revision_monotonic
BEFORE UPDATE OF revision ON household_routines
WHEN NEW.revision < OLD.revision
BEGIN
    SELECT RAISE(ABORT, 'a routine revision cannot move backwards (ADR-W030 A8)');
END;

CREATE TRIGGER restore_state_release_needs_replay
BEFORE UPDATE OF released_at ON restore_state
WHEN NEW.released_at <> '' AND (NEW.replayed_at = '' OR NEW.journal_current = 0)
BEGIN
    SELECT RAISE(ABORT, 'quarantine is released only after the deletion journal is asserted current AND replayed (ADR-W030 A10)');
END;

CREATE TRIGGER models_kind_immutable
BEFORE UPDATE OF kind ON models
WHEN NEW.kind <> OLD.kind
BEGIN
    SELECT RAISE(ABORT, 'a model cannot change kind: local and cloud are different destinations, and a butler was assigned to it on the strength of which one (ADR-W031 A2)');
END;

CREATE TRIGGER butler_role_changes_immutable
BEFORE UPDATE ON butler_role_changes
BEGIN
    SELECT RAISE(ABORT, 'role change records are append-only (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_role_changes_not_deleted
BEFORE DELETE ON butler_role_changes
BEGIN
    SELECT RAISE(ABORT, 'role change records are append-only and cannot be deleted (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_access_requires_admission
BEFORE INSERT ON butler_access
WHEN (SELECT COUNT(*) FROM butler_members
      WHERE person_id = NEW.person_id AND admitted = 1 AND offboarded_at = '') = 0
BEGIN
    SELECT RAISE(ABORT, 'that person is not an admitted butler member, so they cannot be given access to a butler (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_members_generation_monotonic
BEFORE UPDATE OF generation ON butler_members
WHEN NEW.generation < OLD.generation
BEGIN
    SELECT RAISE(ABORT, 'butler member generation cannot move backwards (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_members_person_id_immutable
BEFORE UPDATE OF person_id ON butler_members
WHEN NEW.person_id <> OLD.person_id
BEGIN
    SELECT RAISE(ABORT, 'person_id is the stable identity and cannot be changed (design §2.2)');
END;

CREATE TRIGGER butler_members_offboarded_stays_denied
BEFORE UPDATE OF admitted ON butler_members
WHEN NEW.admitted = 1 AND OLD.offboarded_at <> '' AND NEW.offboarded_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an offboarded member cannot be re-admitted without a new admission (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_members_last_owner_stays
BEFORE UPDATE ON butler_members
WHEN OLD.role = 'owner'
 AND OLD.admitted = 1 AND OLD.disabled_at = '' AND OLD.offboarded_at = ''
 AND NOT (NEW.role = 'owner' AND NEW.admitted = 1
          AND NEW.disabled_at = '' AND NEW.offboarded_at = '')
 AND (SELECT COUNT(*) FROM butler_members
      WHERE role = 'owner' AND admitted = 1
        AND disabled_at = '' AND offboarded_at = ''
        AND seq <> OLD.seq) = 0
BEGIN
    SELECT RAISE(ABORT, 'the last active owner cannot be demoted, disabled, or offboarded while no other active owner exists (design §2.5)');
END;

CREATE TRIGGER butler_members_last_owner_not_deleted
BEFORE DELETE ON butler_members
WHEN OLD.role = 'owner'
 AND OLD.admitted = 1 AND OLD.disabled_at = '' AND OLD.offboarded_at = ''
 AND (SELECT COUNT(*) FROM butler_members
      WHERE role = 'owner' AND admitted = 1
        AND disabled_at = '' AND offboarded_at = ''
        AND seq <> OLD.seq) = 0
BEGIN
    SELECT RAISE(ABORT, 'the last active owner cannot be deleted while no other active owner exists (design §2.5)');
END;

CREATE TRIGGER butler_handoffs_immutable
BEFORE UPDATE ON butler_handoffs
BEGIN
    SELECT RAISE(ABORT, 'butler handoffs are append-only (ADR-W030 A2.1)');
END;

CREATE TRIGGER household_review_tokens_single_use
BEFORE UPDATE OF used_at ON household_review_tokens
WHEN OLD.used_at <> '' AND NEW.used_at <> OLD.used_at
BEGIN
    SELECT RAISE(ABORT, 'a household review token is single-use (ADR-W030 A8.1)');
END;

CREATE TRIGGER notices_answered_once
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.answered_at <> '' AND NEW.answered_at <> OLD.answered_at
BEGIN
    SELECT RAISE(ABORT, 'a notice is answered once (ADR-W030 A10)');
END;

CREATE TRIGGER notices_expired_stays_expired
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.undelivered_at <> '' AND NEW.answered_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an expired notice cannot be answered (ADR-W030 A10.1)');
END;

CREATE TRIGGER butler_memory_audience_cannot_loosen
BEFORE UPDATE OF audience ON butler_memory
WHEN OLD.audience <> NEW.audience
BEGIN
    SELECT RAISE(ABORT, 'a memory audience cannot be changed by an update; narrowing needs a verb that proves it (ADR-W030 A2/A3, design §11)');
END;

CREATE TRIGGER butler_memory_content_immutable
BEFORE UPDATE OF content, source_kind, source_id, author_person_id, kind ON butler_memory
BEGIN
    SELECT RAISE(ABORT, 'a memory record is corrected by superseding it, not by editing it (ADR-W030 A3)');
END;

CREATE TRIGGER butler_index_ad AFTER DELETE ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
END;

CREATE TRIGGER butler_index_ai AFTER INSERT ON butler_index BEGIN
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_index_au AFTER UPDATE OF content ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_tasks_author_immutable
BEFORE UPDATE OF author_person_id ON butler_tasks
WHEN NEW.author_person_id <> OLD.author_person_id
BEGIN
    SELECT RAISE(ABORT, 'butler task authorship is immutable (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_insert
BEFORE INSERT ON butler_tasks
WHEN NEW.state = 'completed'
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot be created completed (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_update
BEFORE UPDATE OF state ON butler_tasks
WHEN NEW.state = 'completed'
 AND NOT EXISTS (SELECT 1 FROM butler_task_evidence WHERE task = NEW.seq)
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot complete without evidence (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON butler_tasks
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'butler task lease epoch cannot move backwards (ADR-W030 A11)');
END;

CREATE TRIGGER butler_task_evidence_immutable
BEFORE UPDATE ON butler_task_evidence
BEGIN
    SELECT RAISE(ABORT, 'butler task evidence is append-only (ADR-W030 A4)');
END;

CREATE TRIGGER butler_work_trace_immutable
BEFORE UPDATE ON butler_work_trace
BEGIN
    SELECT RAISE(ABORT, 'butler work trace is append-only (ADR-W030 A11)');
END;

CREATE TRIGGER integration_connections_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON integration_connections
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'a connection lease epoch cannot move backwards (ADR-W030 A5)');
END;

CREATE TRIGGER butlers_class_moves_with_surface
BEFORE UPDATE OF agent_class, surface ON butlers
WHEN (NEW.agent_class <> OLD.agent_class) <> (NEW.surface <> OLD.surface)
BEGIN
    SELECT RAISE(ABORT, 'an agent class and its surface move together or not at all: the class names the ceiling and the surface enforces it (ADR-W033 design §5.2)');
END;

CREATE TRIGGER butlers_model_usable_insert
BEFORE INSERT ON butlers
WHEN (SELECT enabled FROM models WHERE seq = NEW.model) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

CREATE TRIGGER butlers_model_usable_update
BEFORE UPDATE OF model ON butlers
WHEN (SELECT enabled FROM models WHERE seq = NEW.model) = 0
BEGIN
    SELECT RAISE(ABORT, 'that model is disabled, so a butler assigned to it would have nowhere to run (ADR-W031 A6)');
END;

CREATE TRIGGER butler_addresses_live_butler
BEFORE INSERT ON butler_addresses
WHEN (SELECT retired_at FROM butlers WHERE seq = NEW.butler) <> ''
BEGIN
    SELECT RAISE(ABORT, 'that butler has been retired, so it cannot be given an address (ADR-W031 A7)');
END;

CREATE TRIGGER butler_connection_scope_audience
BEFORE INSERT ON butler_connection_scope
WHEN (SELECT a.kind
        FROM integration_connections c
        JOIN audiences a ON a.audience_id = c.audience
       WHERE c.seq = NEW.connection) <> 'household'
 AND (SELECT surface FROM butlers WHERE seq = NEW.butler) = 'family'
BEGIN
    SELECT RAISE(ABORT, 'that connection is not shared with the whole household, so a household-surface butler cannot be given it (ADR-W031 A4)');
END;

CREATE TRIGGER connection_admission_default
AFTER INSERT ON butler_addresses
BEGIN
    INSERT INTO connection_admission (connection, admits, subject_kind, set_at, set_by)
    VALUES (
        NEW.seq,
        CASE (SELECT b.agent_class FROM butlers b WHERE b.seq = NEW.butler)
            WHEN 'owner' THEN 'owners' ELSE 'household' END,
        'person',
        NEW.created_at,
        NEW.created_by
    );
END;

CREATE TRIGGER connection_admission_owner_agent_insert
BEFORE INSERT ON connection_admission
WHEN NEW.admits <> 'owners'
 AND (SELECT b.agent_class FROM butler_addresses a JOIN butlers b ON b.seq = a.butler
       WHERE a.seq = NEW.connection) = 'owner'
BEGIN
    SELECT RAISE(ABORT, 'an owner agent''s connection admits owners only, so it cannot be set to admit the household (ADR-W033 B9, design 8.3)');
END;

CREATE TRIGGER connection_admission_owner_agent_update
BEFORE UPDATE OF admits ON connection_admission
WHEN NEW.admits <> 'owners'
 AND (SELECT b.agent_class FROM butler_addresses a JOIN butlers b ON b.seq = a.butler
       WHERE a.seq = NEW.connection) = 'owner'
BEGIN
    SELECT RAISE(ABORT, 'an owner agent''s connection admits owners only, so it cannot be set to admit the household (ADR-W033 B9, design 8.3)');
END;

CREATE TRIGGER chat_outbound_no_push_into_a_room
BEFORE INSERT ON chat_outbound
WHEN NEW.kind = 'push'
 AND EXISTS (
     SELECT 1 FROM chat_rooms r
      WHERE r.connection = NEW.connection AND r.conversation = NEW.target)
BEGIN
    SELECT RAISE(ABORT,
        'that destination is a room, and the house does not send what it knows into a room (ADR-W033 B9.1, design 8.3)');
END;

CREATE TRIGGER memory_scope_valid_insert BEFORE INSERT ON butler_memory
WHEN (NEW.scope = 'personal' AND NEW.owner_person_id IS NULL)
  OR (NEW.scope = 'agent_local' AND NEW.butler = '')
BEGIN SELECT RAISE(ABORT, 'memory scope needs its owner or Agent'); END;

CREATE TRIGGER memory_scope_immutable BEFORE UPDATE OF scope, owner_person_id ON butler_memory
WHEN NEW.scope <> OLD.scope OR NEW.owner_person_id IS NOT OLD.owner_person_id
BEGIN SELECT RAISE(ABORT, 'memory scope and ownership require a new record'); END;

CREATE TRIGGER memory_capture_disclosure AFTER INSERT ON butler_memory BEGIN
    INSERT INTO memory_disclosure_origins
    SELECT o.memory, o.connection_ref, o.connection_name,
           COALESCE(d.reach,''), COALESCE(d.encryption,''),
           COALESCE(d.retention,''), COALESCE(d.surface,'')
    FROM memory_source_connections o LEFT JOIN connection_disclosure d ON d.connection=o.connection_ref
    WHERE o.memory=NEW.seq;
END;

CREATE TRIGGER household_secret_person_revoked AFTER UPDATE OF generation ON butler_members
WHEN NEW.generation != OLD.generation
BEGIN
 INSERT INTO deletion_journal(subject_kind,subject_id,action,at,actor,detail)
 SELECT 'secret','household-secret/'||id,'revoked',strftime('%Y-%m-%dT%H:%M:%fZ','now'),NEW.member,'Person generation changed'
 FROM household_secrets WHERE owner_person_id=NEW.person_id AND scope='personal' AND state!='deleted';
 UPDATE household_secrets SET state='quarantine' WHERE owner_person_id=NEW.person_id AND scope='personal' AND state!='deleted';
 DELETE FROM household_secret_agents WHERE secret_id IN (SELECT id FROM household_secrets WHERE owner_person_id=NEW.person_id AND scope='personal');
 DELETE FROM household_secret_grants WHERE person_id=NEW.person_id;
END;

CREATE TRIGGER privacy_link_first_turn AFTER UPDATE OF session ON butler_tasks WHEN NEW.session IS NOT NULL BEGIN
 INSERT OR IGNORE INTO session_memory_sources(session,memory)
 SELECT NEW.session,memory FROM task_memory_sources WHERE task=NEW.seq;
END;

CREATE TRIGGER privacy_link_current_turn AFTER INSERT ON task_memory_sources BEGIN
 INSERT OR IGNORE INTO session_memory_sources(session,memory)
 SELECT session,NEW.memory FROM butler_tasks WHERE seq=NEW.task AND session IS NOT NULL;
END;

CREATE TRIGGER discard_outbound_with_deleted_history BEFORE DELETE ON butler_sessions BEGIN
 DELETE FROM chat_outbound WHERE seq IN (SELECT outbound FROM chat_outbound_sessions WHERE session=OLD.seq);
END;

CREATE TRIGGER discard_outbound_with_deleted_task BEFORE DELETE ON butler_tasks BEGIN
 DELETE FROM chat_outbound WHERE seq IN (SELECT outbound FROM chat_outbound_tasks WHERE task=OLD.seq);
END;

CREATE TRIGGER discard_outbound_with_deleted_memory BEFORE DELETE ON butler_memory BEGIN
 DELETE FROM chat_outbound WHERE seq IN (SELECT outbound FROM chat_outbound_memories WHERE memory=OLD.seq);
END;

CREATE TRIGGER preserve_privacy_before_task_retention BEFORE DELETE ON butler_tasks BEGIN
 INSERT OR IGNORE INTO butler_memory_derivations(derivative,source)
 SELECT m.seq,u.memory FROM butler_memory m JOIN task_memory_sources u ON u.task=OLD.seq
 WHERE m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||OLD.seq AND m.seq<>u.memory;
 INSERT OR IGNORE INTO session_memory_sources(session,memory)
 SELECT s.session,m.memory FROM
 (SELECT session FROM session_task_sources WHERE task=OLD.seq
  UNION SELECT OLD.session WHERE OLD.session IS NOT NULL) s
 CROSS JOIN
 (SELECT memory FROM task_memory_sources WHERE task=OLD.seq
  UNION SELECT seq FROM butler_memory WHERE source_kind IN ('conversation','group-conversation') AND source_id='bt-'||OLD.seq) m;
 INSERT OR IGNORE INTO memory_origin_sessions(memory,session)
 SELECT m.seq,s.session FROM butler_memory m CROSS JOIN
 (SELECT session FROM session_task_sources WHERE task=OLD.seq
  UNION SELECT OLD.session WHERE OLD.session IS NOT NULL) s
 WHERE m.source_kind IN ('conversation','group-conversation') AND m.source_id='bt-'||OLD.seq;
END;

CREATE TRIGGER owner_queue_submission AFTER INSERT ON plans WHEN NEW.state='proposed' BEGIN
 INSERT INTO owner_review_queue(plan,proposer_person_id,queued_at)
 VALUES(NEW.seq,COALESCE((SELECT person_id FROM butler_members WHERE member=COALESCE(NULLIF(NEW.acting_for,''),NEW.principal) COLLATE BINARY), (SELECT CASE WHEN COUNT(*)=1 THEN MIN(person_id) END FROM butler_members WHERE member=COALESCE(NULLIF(NEW.acting_for,''),NEW.principal) COLLATE NOCASE)),NEW.created_at);
END;

CREATE TRIGGER routine_default_output_audience AFTER INSERT ON household_routines WHEN NEW.output_audience IS NULL BEGIN
 UPDATE household_routines SET output_audience='aud_household' WHERE seq=NEW.seq;
END;

CREATE TRIGGER routine_edit_requires_review AFTER UPDATE OF revision ON household_routines
WHEN NEW.revision<>OLD.revision BEGIN
 UPDATE household_routines SET state='draft' WHERE seq=NEW.seq AND ownership='wildstone';
 UPDATE plans SET state='superseded' WHERE state='proposed' AND seq IN
 (SELECT plan FROM routine_plan_versions WHERE routine=NEW.seq AND revision<>NEW.revision);
END;

CREATE TRIGGER routine_activation_requires_review BEFORE UPDATE OF state ON household_routines
WHEN NEW.ownership='wildstone' AND NEW.state IN ('enabled','trial') AND NEW.approved_revision<>NEW.revision BEGIN
 SELECT RAISE(ABORT,'the exact routine version needs Owner approval');
END;

CREATE TRIGGER owner_review_revoke_pending AFTER UPDATE OF generation ON butler_members
WHEN NEW.generation<>OLD.generation BEGIN
 UPDATE plans SET state='superseded' WHERE state='proposed' AND seq IN
 (SELECT plan FROM owner_review_queue WHERE proposer_person_id=NEW.person_id);
 UPDATE household_review_tokens SET used_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),used_by='revoked'
 WHERE person_id=NEW.person_id AND used_at='';
END;

CREATE TRIGGER routine_creation_requires_review BEFORE INSERT ON household_routines
WHEN NEW.ownership='wildstone' AND NEW.state IN ('enabled','trial') BEGIN
 SELECT RAISE(ABORT,'create a draft and submit it for Owner review');
END;

CREATE TRIGGER routine_plan_approval_requires_owner BEFORE UPDATE OF state ON plans
WHEN NEW.state='approved' AND EXISTS(SELECT 1 FROM routine_plan_versions WHERE plan=NEW.seq AND approved_by IS NULL) BEGIN
 SELECT RAISE(ABORT,'routine Plans require explicit Owner approval of the exact version');
END;

CREATE TRIGGER owner_review_cancel_notice AFTER UPDATE OF state ON plans
WHEN NEW.state<>'proposed' BEGIN
 UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z'
 WHERE delivered_at='' AND dedupe_key LIKE 'owner-review:plan-'||NEW.seq||':%';
 UPDATE notices SET undelivered_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE kind='owner.review' AND delivered_at='' AND dedupe_key LIKE 'owner-review:plan-'||NEW.seq||':%';
END;

CREATE TRIGGER owner_review_refuse_stale_notice AFTER INSERT ON chat_outbound
WHEN NEW.dedupe_key LIKE 'owner-review:%' AND NOT EXISTS (
 SELECT 1 FROM plans p,owner_review_preference pref WHERE pref.binding_id=NEW.binding_id AND NEW.correlation_id='owner-review-pref:'||pref.generation AND p.state='proposed' AND NEW.dedupe_key LIKE 'owner-review:plan-'||p.seq||':%'
) BEGIN
 UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z' WHERE seq=NEW.seq;
END;

CREATE TRIGGER owner_review_preference_changed AFTER UPDATE OF generation ON owner_review_preference
WHEN NEW.generation<>OLD.generation BEGIN
 UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z'
 WHERE delivered_at='' AND dedupe_key LIKE 'owner-review:%';
 UPDATE notices SET undelivered_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE kind='owner.review' AND delivered_at='';
END;

CREATE TRIGGER owner_review_recipient_revoked AFTER UPDATE OF generation,role ON butler_members
WHEN NEW.generation<>OLD.generation OR (NEW.role<>OLD.role AND NEW.role<>'owner') BEGIN
 UPDATE owner_review_preference SET binding_id=NULL,console_url='',generation=generation+1
 WHERE binding_id IN (SELECT id FROM chat_bindings WHERE person_id=NEW.person_id);
END;

CREATE TRIGGER owner_review_binding_revoked AFTER UPDATE OF revoked_at ON chat_bindings
WHEN NEW.revoked_at<>'' AND NEW.revoked_at<>OLD.revoked_at BEGIN
 UPDATE owner_review_preference SET binding_id=NULL,console_url='',generation=generation+1 WHERE binding_id=NEW.id;
END;

CREATE TRIGGER effect_receipt_tool_requires_verified_step BEFORE INSERT ON butler_effect_receipts
WHEN NEW.kind='tool' AND NEW.outcome='succeeded' AND NOT EXISTS (
 SELECT 1 FROM butler_task_steps s JOIN butler_task_attempts a ON a.step=s.seq
 WHERE s.seq=NEW.step AND s.task=NEW.task AND s.kind='tool' AND s.state='verified'
 AND a.state='succeeded' AND json_valid(a.result_json) AND ((NEW.proposal_only=0 AND json_extract(a.result_json,'$.effectConfirmed')=1) OR (NEW.proposal_only=1 AND json_extract(a.result_json,'$.proposalConfirmed')=1)) AND COALESCE(json_extract(a.result_json,'$.rejected'),0)=0)
BEGIN SELECT RAISE(ABORT,'tool receipt needs a verified successful step'); END;

CREATE TRIGGER effect_receipt_immutable BEFORE UPDATE ON butler_effect_receipts
BEGIN SELECT RAISE(ABORT,'effect receipts are immutable'); END;

CREATE TRIGGER evidence_receipt_matches_task BEFORE INSERT ON butler_task_evidence
WHEN NEW.receipt IS NOT NULL AND (NEW.kind NOT IN ('tool-result','external-verification') OR NOT EXISTS (
 SELECT 1 FROM butler_effect_receipts r WHERE r.seq=NEW.receipt AND r.task=NEW.task
 AND r.permission_label=NEW.permission_label AND r.audience=NEW.audience))
BEGIN SELECT RAISE(ABORT,'evidence receipt must match task and disclosure metadata'); END;

CREATE TRIGGER effectful_completion_requires_receipt BEFORE UPDATE OF state,objective_kind,proposal_only ON butler_tasks
WHEN NEW.state='completed' AND NEW.objective_kind<>'informational' AND (
 NOT EXISTS (SELECT 1 FROM butler_required_effects WHERE task=NEW.seq) OR
 EXISTS (SELECT 1 FROM butler_required_effects required WHERE required.task=NEW.seq AND NOT EXISTS (
 SELECT 1 FROM butler_task_evidence e JOIN butler_effect_receipts r ON r.seq=e.receipt
 WHERE e.task=NEW.seq AND r.task=NEW.seq AND r.outcome='succeeded' AND r.proposal_only=required.proposal_only
 AND required.source_key=CASE WHEN r.step IS NOT NULL THEN 'step:'||r.step ELSE r.kind||':'||r.source_id END
 AND e.kind IN ('tool-result','external-verification'))))
BEGIN SELECT RAISE(ABORT,'every required effect needs a matching successful receipt'); END;

CREATE TRIGGER required_effects_no_completed_insert BEFORE INSERT ON butler_required_effects
WHEN (SELECT state FROM butler_tasks WHERE seq=NEW.task)='completed'
BEGIN SELECT RAISE(ABORT,'completed objective requirements are immutable'); END;

CREATE TRIGGER required_effects_no_completed_update BEFORE UPDATE ON butler_required_effects
WHEN (SELECT state FROM butler_tasks WHERE seq=OLD.task)='completed' OR (SELECT state FROM butler_tasks WHERE seq=NEW.task)='completed'
BEGIN SELECT RAISE(ABORT,'completed objective requirements are immutable'); END;

CREATE TRIGGER required_effects_no_completed_delete BEFORE DELETE ON butler_required_effects
WHEN (SELECT state FROM butler_tasks WHERE seq=OLD.task)='completed'
BEGIN SELECT RAISE(ABORT,'completed objective requirements are immutable'); END;

CREATE TRIGGER effect_receipt_step_matches_task BEFORE INSERT ON butler_effect_receipts
WHEN NEW.step IS NOT NULL AND NOT EXISTS (SELECT 1 FROM butler_task_steps WHERE seq=NEW.step AND task=NEW.task)
BEGIN SELECT RAISE(ABORT,'receipt operation must belong to its task'); END;

CREATE TRIGGER chat_effect_requires_own_operation BEFORE INSERT ON chat_outbound_effects
WHEN NOT EXISTS(SELECT 1 FROM butler_task_steps WHERE seq=NEW.step AND task=NEW.task AND kind='tool')
BEGIN SELECT RAISE(ABORT,'chat effect must belong to its originating operation'); END;

CREATE TRIGGER chat_delivery_receipt AFTER UPDATE OF delivered_at ON chat_outbound
WHEN OLD.delivered_at='' AND NEW.delivered_at<>'' BEGIN
 INSERT OR IGNORE INTO butler_effect_receipts(task,recorded_at,kind,source_id,step,outcome,permission_label,audience,proposal_only)
 SELECT task,NEW.delivered_at,'executor','chat-outbound:'||NEW.seq,step,'succeeded',permission_label,audience,0
 FROM chat_outbound_effects WHERE outbound=NEW.seq AND expected_body=NEW.body;
END;

CREATE TRIGGER routine_secret_grant_matches_review BEFORE INSERT ON household_secret_routine_grants
WHEN NOT EXISTS (
 SELECT 1 FROM routine_plan_versions v JOIN plans p ON p.seq=v.plan
 JOIN json_each(v.secret_ids) requested ON requested.value=NEW.secret_id
 WHERE v.plan=NEW.plan AND v.approved_by IS NOT NULL AND p.state='proposed'
) BEGIN SELECT RAISE(ABORT,'routine secret grant was not in the reviewed Plan'); END;

CREATE TRIGGER routine_memory_grant_matches_review BEFORE INSERT ON routine_memory_grants
WHEN NOT EXISTS (
 SELECT 1 FROM routine_plan_versions v JOIN plans p ON p.seq=v.plan
 JOIN json_each(v.memory_ids) requested ON requested.value=NEW.memory
 WHERE v.plan=NEW.plan AND v.approved_by IS NOT NULL AND p.state='proposed'
) BEGIN SELECT RAISE(ABORT,'routine memory grant was not in the reviewed Plan'); END;

CREATE TRIGGER routine_effect_receipts_immutable BEFORE UPDATE ON routine_effect_receipts
BEGIN SELECT RAISE(ABORT,'routine effect receipts are append-only'); END;

CREATE TRIGGER routine_run_transitions_immutable BEFORE UPDATE ON routine_run_transitions
BEGIN SELECT RAISE(ABORT,'routine run transitions are append-only'); END;

CREATE TRIGGER household_execution_stop_events_immutable BEFORE UPDATE ON household_execution_stop_events
BEGIN SELECT RAISE(ABORT,'household execution stop history is append-only'); END;

CREATE TRIGGER owner_demotion_revoke_review_tokens
AFTER UPDATE OF role ON butler_members
WHEN OLD.role='owner' AND NEW.role='member'
BEGIN
 UPDATE household_review_tokens
 SET used_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),used_by='demoted'
 WHERE person_id=NEW.person_id AND used_at='';
END;

CREATE TRIGGER routine_revocation_fences_activation BEFORE UPDATE OF state ON household_routines
WHEN NEW.ownership='wildstone' AND NEW.state IN ('enabled','trial')
 AND (NEW.revoked_at<>'' OR NEW.deleted_at<>'' OR NEW.paused_reason<>'')
BEGIN SELECT RAISE(ABORT,'routine requires Owner resolution or a new approved version'); END;

CREATE TRIGGER conversation_work_reviews_immutable BEFORE UPDATE ON conversation_work_reviews
BEGIN SELECT RAISE(ABORT,'conversation work reviews are append-only'); END;

CREATE TRIGGER routine_payload_requires_version BEFORE UPDATE OF document,output_audience,butler ON household_routines
WHEN (NEW.document<>OLD.document OR NEW.output_audience<>OLD.output_audience OR NEW.butler<>OLD.butler)
 AND NEW.revision<=OLD.revision AND NEW.purged_at=''
BEGIN SELECT RAISE(ABORT,'changed routine behavior requires a new version'); END;

CREATE TRIGGER routine_review_snapshot_immutable BEFORE UPDATE OF routine,revision,document,output_audience,delivery,assigned_butler ON routine_plan_versions
WHEN (SELECT purged_at FROM household_routines WHERE seq=OLD.routine)=''
BEGIN SELECT RAISE(ABORT,'a reviewed version is immutable; submit a new Plan'); END;

CREATE TRIGGER reviewed_plan_immutable BEFORE UPDATE OF plan_json ON plans
WHEN NEW.plan_json<>OLD.plan_json AND NOT EXISTS (
 SELECT 1 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine
 WHERE v.plan=OLD.seq AND r.purged_at<>'')
BEGIN SELECT RAISE(ABORT,'editing a Plan requires a new submitted version'); END;

CREATE TRIGGER routine_purged_cannot_restore BEFORE UPDATE OF deleted_at ON household_routines
WHEN OLD.purged_at<>'' AND NEW.deleted_at=''
BEGIN SELECT RAISE(ABORT,'purged routine cannot be restored'); END;

CREATE TRIGGER routine_purged_payload_sealed BEFORE UPDATE OF document,revision ON household_routines
WHEN OLD.purged_at<>'' AND (NEW.document<>'[purged]' OR NEW.revision<>OLD.revision)
BEGIN SELECT RAISE(ABORT,'purged routine cannot be edited'); END;

CREATE TRIGGER routine_review_purged_payload_sealed BEFORE UPDATE OF document ON routine_plan_versions
WHEN (SELECT purged_at FROM household_routines WHERE seq=OLD.routine)<>'' AND NEW.document<>'[purged]'
BEGIN SELECT RAISE(ABORT,'purged reviewed version cannot be edited'); END;

CREATE TRIGGER routine_plan_purged_payload_sealed BEFORE UPDATE OF plan_json ON plans
WHEN EXISTS (SELECT 1 FROM routine_plan_versions v JOIN household_routines r ON r.seq=v.routine
 WHERE v.plan=OLD.seq AND r.purged_at<>'') AND NEW.plan_json<>'{}'
BEGIN SELECT RAISE(ABORT,'purged Plan cannot be edited'); END;

CREATE TRIGGER routine_resources_require_version BEFORE UPDATE OF resource_signature ON household_routines
WHEN NEW.resource_signature<>OLD.resource_signature AND NEW.revision<=OLD.revision AND NEW.purged_at=''
BEGIN SELECT RAISE(ABORT,'changed routine grants require a new version'); END;

CREATE TRIGGER routine_resource_snapshot_immutable BEFORE UPDATE OF secret_ids,memory_ids ON routine_plan_versions
WHEN (SELECT purged_at FROM household_routines WHERE seq=OLD.routine)=''
BEGIN SELECT RAISE(ABORT,'reviewed resource requests are immutable; submit a new Plan'); END;

CREATE TRIGGER runtime_principal_binding_immutable
BEFORE UPDATE OF butler ON runtime_principals
WHEN NEW.butler <> OLD.butler
BEGIN
    SELECT RAISE(ABORT, 'a runtime principal is bound to one agent for its life; mint a new one (ADR-W039 §4.1)');
END;

CREATE TRIGGER connection_disclosure_default
AFTER INSERT ON butler_addresses
BEGIN
    INSERT INTO connection_disclosure
        (connection, reach, encryption, retention, surface, accepts_fallback, set_at, set_by)
    VALUES (
        NEW.seq, 'external', 'transport',
        CASE NEW.transport
            WHEN 'telegram' THEN 'provider' WHEN 'discord' THEN 'provider'
            WHEN 'slack' THEN 'provider' WHEN 'whatsapp' THEN 'provider'
            ELSE 'unknown' END,
        'external_messaging', 0, NEW.created_at, NEW.created_by
    );
END;

CREATE TRIGGER person_handles_refuse_reuse_on_insert
BEFORE INSERT ON butler_members
WHEN EXISTS (
    SELECT 1 FROM person_handles h
      JOIN butler_members b ON b.person_id = h.person_id
     WHERE h.handle = NEW.member
       AND h.person_id <> NEW.person_id
       AND b.member <> NEW.member)
BEGIN
    SELECT RAISE(ABORT, 'runtimedb: that handle belonged to another person and is kept so their references still reach them');
END;

CREATE TRIGGER person_handles_refuse_reuse_on_rename
BEFORE UPDATE OF member ON butler_members
WHEN EXISTS (
    SELECT 1 FROM person_handles h
     WHERE h.handle = NEW.member
       AND h.person_id <> NEW.person_id)
BEGIN
    SELECT RAISE(ABORT, 'runtimedb: that handle belonged to another person and is kept so their references still reach them');
END;

CREATE TRIGGER person_handles_record_on_insert
AFTER INSERT ON butler_members
BEGIN
    INSERT OR IGNORE INTO person_handles (handle, person_id, recorded_at)
        VALUES (NEW.member, NEW.person_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;

CREATE TRIGGER person_handles_record_on_rename
AFTER UPDATE OF member ON butler_members
BEGIN
    INSERT OR IGNORE INTO person_handles (handle, person_id, recorded_at)
        VALUES (NEW.member, NEW.person_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;

CREATE TRIGGER person_file_dirs_record_on_insert
AFTER INSERT ON butler_members
WHEN NOT EXISTS (SELECT 1 FROM person_file_dirs WHERE person_id = NEW.person_id)
 AND NOT EXISTS (SELECT 1 FROM person_file_dirs WHERE dir = lower(NEW.member))
BEGIN
    INSERT INTO person_file_dirs (person_id, dir, created_at)
        VALUES (NEW.person_id, lower(NEW.member), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
END;

CREATE TRIGGER agent_workspaces_immutable BEFORE UPDATE ON agent_workspaces
BEGIN SELECT RAISE(ABORT,'workspace identity is immutable'); END;

CREATE TRIGGER workspace_artifacts_identity BEFORE UPDATE OF id,workspace_id,created_at ON workspace_artifacts
BEGIN SELECT RAISE(ABORT,'artifact ownership is immutable'); END;

CREATE TRIGGER workspace_revisions_immutable BEFORE UPDATE ON workspace_revisions
BEGIN SELECT RAISE(ABORT,'workspace revisions are immutable'); END;

CREATE TRIGGER workspace_revisions_task_scope BEFORE INSERT ON workspace_revisions
WHEN NOT EXISTS (SELECT 1 FROM workspace_artifacts a JOIN agent_workspaces w ON w.id=a.workspace_id JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE a.id=NEW.artifact_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'revision task does not own workspace'); END;

CREATE TRIGGER workspace_reservations_identity BEFORE UPDATE OF id,workspace_id,task_id,size_bytes,purpose,created_at ON workspace_reservations
BEGIN SELECT RAISE(ABORT,'reservation identity is immutable'); END;

CREATE TRIGGER workspace_reservations_transition BEFORE UPDATE OF state ON workspace_reservations
WHEN OLD.state<>'held' OR NEW.state NOT IN ('consumed','released')
BEGIN SELECT RAISE(ABORT,'invalid reservation transition'); END;

CREATE TRIGGER workspace_reservations_task_scope BEFORE INSERT ON workspace_reservations
WHEN NOT EXISTS (SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE w.id=NEW.workspace_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'reservation task does not own workspace'); END;

CREATE TRIGGER workspace_reservations_initial BEFORE INSERT ON workspace_reservations
WHEN NEW.state<>'held'
BEGIN SELECT RAISE(ABORT,'reservation must start held'); END;

CREATE TRIGGER workspace_publications_identity BEFORE UPDATE OF id,workspace_id,artifact_id,revision_id,base_revision,task_id,reservation_id,effect_key,operation,created_at ON workspace_publications
BEGIN SELECT RAISE(ABORT,'publication intent is immutable'); END;

CREATE TRIGGER workspace_publications_initial BEFORE INSERT ON workspace_publications
WHEN NEW.state<>'staging' OR NOT EXISTS(SELECT 1 FROM workspace_reservations q JOIN workspace_revisions r ON r.id=NEW.revision_id JOIN workspace_artifacts a ON a.id=NEW.artifact_id WHERE q.id=NEW.reservation_id AND q.state='held' AND q.purpose='publication' AND q.size_bytes>=r.size_bytes AND r.task_id=NEW.task_id AND a.state<>'trashed' AND a.current_revision IS NEW.base_revision)
BEGIN SELECT RAISE(ABORT,'invalid publication intent'); END;

CREATE TRIGGER workspace_publications_transition BEFORE UPDATE OF state ON workspace_publications
WHEN NOT ((OLD.state='staging' AND NEW.state IN ('prepared','aborted')) OR (OLD.state='prepared' AND NEW.state IN ('bytes_published','aborted')) OR (OLD.state='bytes_published' AND NEW.state IN ('committed','aborted')))
BEGIN SELECT RAISE(ABORT,'invalid publication transition'); END;

CREATE TRIGGER workspace_publications_commit BEFORE UPDATE OF state ON workspace_publications
WHEN NEW.state='committed' AND NOT EXISTS (SELECT 1 FROM workspace_artifacts a JOIN workspace_reservations q ON q.id=NEW.reservation_id WHERE a.id=NEW.artifact_id AND a.state<>'trashed' AND a.current_revision IS NEW.base_revision AND q.state='held')
BEGIN SELECT RAISE(ABORT,'publication base or reservation changed'); END;

CREATE TRIGGER workspace_artifacts_current BEFORE UPDATE OF current_revision,state ON workspace_artifacts
WHEN NEW.current_revision IS NOT NULL AND NOT EXISTS(SELECT 1 FROM workspace_publications p WHERE p.artifact_id=NEW.id AND p.revision_id=NEW.current_revision AND p.state='committed')
BEGIN SELECT RAISE(ABORT,'only committed revisions may be current'); END;

CREATE TRIGGER workspace_artifacts_initial BEFORE INSERT ON workspace_artifacts
WHEN NEW.state<>'pending' OR NEW.current_revision IS NOT NULL
BEGIN SELECT RAISE(ABORT,'artifact must start pending'); END;

CREATE TRIGGER workspace_reservations_pinned BEFORE UPDATE OF state ON workspace_reservations
WHEN EXISTS(SELECT 1 FROM workspace_publications p WHERE p.reservation_id=OLD.id AND p.state NOT IN ('committed','aborted'))
BEGIN SELECT RAISE(ABORT,'publication pins reservation'); END;

CREATE TRIGGER workspace_provenance_immutable BEFORE UPDATE ON workspace_provenance
BEGIN SELECT RAISE(ABORT,'workspace provenance is immutable'); END;

CREATE TRIGGER workspace_provenance_pending BEFORE INSERT ON workspace_provenance
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=NEW.revision_id AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'prepared provenance is sealed'); END;

CREATE TRIGGER workspace_publications_sources_committed BEFORE UPDATE OF state ON workspace_publications
WHEN NEW.state='prepared' AND EXISTS(
 SELECT 1 FROM workspace_provenance edge
 WHERE edge.revision_id=NEW.revision_id AND edge.source_revision IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM workspace_publications source
                WHERE source.revision_id=edge.source_revision AND source.state='committed'))
BEGIN SELECT RAISE(ABORT,'workspace provenance source is not committed'); END;

CREATE TRIGGER workspace_provenance_privacy BEFORE INSERT ON workspace_provenance
WHEN (NEW.privacy='local-only' OR EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.source_revision AND privacy='local-only')) AND NOT EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.revision_id AND privacy='local-only')
BEGIN SELECT RAISE(ABORT,'derived revision cannot widen source privacy'); END;

CREATE TRIGGER workspace_receipts_committed BEFORE INSERT ON workspace_receipts
WHEN NOT EXISTS(SELECT 1 FROM workspace_publications WHERE id=NEW.publication_id AND state='committed')
BEGIN SELECT RAISE(ABORT,'receipt requires committed publication'); END;

CREATE TRIGGER workspace_receipts_immutable BEFORE UPDATE ON workspace_receipts
BEGIN SELECT RAISE(ABORT,'workspace receipts are immutable'); END;

CREATE TRIGGER workspace_receipts_no_delete BEFORE DELETE ON workspace_receipts
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE id=OLD.publication_id)
BEGIN SELECT RAISE(ABORT,'workspace receipt history requires explicit retention coordination'); END;

CREATE TRIGGER workspace_provenance_delete_sealed BEFORE DELETE ON workspace_provenance
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=OLD.revision_id AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'prepared provenance is sealed'); END;

CREATE TRIGGER workspace_provenance_trusted BEFORE INSERT ON workspace_provenance
WHEN (NEW.trusted_only=1 OR EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.source_revision AND trusted_only=1)) AND NOT EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.revision_id AND trusted_only=1)
BEGIN SELECT RAISE(ABORT,'derived revision cannot widen source trust'); END;

CREATE TRIGGER workspace_publications_task_scope BEFORE UPDATE OF state ON workspace_publications
WHEN NEW.state<>'aborted' AND NOT EXISTS(SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE w.id=NEW.workspace_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'publication task no longer owns workspace'); END;

CREATE TRIGGER workspace_publications_no_delete BEFORE DELETE ON workspace_publications
BEGIN SELECT RAISE(ABORT,'publication history requires explicit retention coordination'); END;

CREATE TRIGGER workspace_publications_base_privacy BEFORE INSERT ON workspace_publications
WHEN EXISTS(SELECT 1 FROM workspace_revisions base JOIN workspace_revisions next ON next.id=NEW.revision_id WHERE base.id=NEW.base_revision AND ((base.privacy='local-only' AND next.privacy<>'local-only') OR (base.trusted_only=1 AND next.trusted_only<>1)))
BEGIN SELECT RAISE(ABORT,'new revision cannot widen base privacy'); END;

CREATE TRIGGER integration_secret_copies_erased_final
BEFORE UPDATE OF erase_state ON integration_secret_copies
WHEN OLD.erase_state = 'erased' AND NEW.erase_state <> 'erased'
BEGIN
    SELECT RAISE(ABORT, 'a proven credential erasure cannot be reopened (ADR-W030 A5.1)');
END;

CREATE TRIGGER personal_background_authorizations_immutable
BEFORE UPDATE OF connection, person, credential, credential_version, source_scope,
    processing_location, result_audience, agent, target_calendar, executable,
    subscription, subscription_digest, digest, created_at
ON personal_background_authorizations
BEGIN
    SELECT RAISE(ABORT, 'a background authorization cannot be edited; a material change is a new authorization (ADR-W033 D1)');
END;

CREATE TRIGGER personal_background_authorizations_revocation_final
BEFORE UPDATE OF revoked_at ON personal_background_authorizations
WHEN OLD.revoked_at <> '' AND NEW.revoked_at <> OLD.revoked_at
BEGIN
    SELECT RAISE(ABORT, 'a revoked background authorization stays revoked (ADR-W033 D1)');
END;

CREATE TRIGGER workspace_revision_session AFTER INSERT ON workspace_revisions BEGIN
 INSERT INTO workspace_revision_sessions(revision,session)
 SELECT NEW.id,session FROM butler_tasks WHERE seq=NEW.task_id AND session IS NOT NULL;
END;

CREATE TRIGGER workspace_source_session AFTER INSERT ON workspace_task_sources BEGIN
 INSERT OR IGNORE INTO workspace_session_sources(session,revision)
 SELECT session,NEW.revision FROM butler_tasks WHERE seq=NEW.task AND session IS NOT NULL;
END;

CREATE TRIGGER workspace_source_late_session AFTER UPDATE OF session ON butler_tasks
WHEN NEW.session IS NOT NULL BEGIN
 INSERT OR IGNORE INTO workspace_session_sources(session,revision)
 SELECT NEW.session,revision FROM workspace_task_sources WHERE task=NEW.seq;
 INSERT OR IGNORE INTO workspace_revision_sessions(revision,session)
 SELECT id,NEW.session FROM workspace_revisions WHERE task_id=NEW.seq;
END;

CREATE TRIGGER workspace_source_committed BEFORE INSERT ON workspace_task_sources
WHEN NOT EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=NEW.revision AND state='committed')
BEGIN SELECT RAISE(ABORT,'workspace source must be committed'); END;

CREATE TRIGGER workspace_revision_capture AFTER INSERT ON workspace_revisions BEGIN
 INSERT OR IGNORE INTO workspace_revision_memories(revision,memory)
 WITH RECURSIVE edges(child,parent) AS (
 SELECT child,parent FROM privacy_edges UNION SELECT child,parent FROM workspace_privacy_edges
 ), sources(object) AS (SELECT 't:'||NEW.task_id UNION SELECT e.parent FROM edges e JOIN sources s ON s.object=e.child)
 SELECT NEW.id,m.seq FROM sources s JOIN butler_memory m ON s.object='m:'||m.seq;
 INSERT OR IGNORE INTO workspace_revision_inputs(revision,source_revision)
 WITH RECURSIVE edges(child,parent) AS (
 SELECT child,parent FROM privacy_edges UNION SELECT child,parent FROM workspace_privacy_edges
 ), sources(object) AS (SELECT 't:'||NEW.task_id UNION SELECT e.parent FROM edges e JOIN sources s ON s.object=e.child)
 SELECT NEW.id,r.id FROM sources s JOIN workspace_revisions r ON s.object='wr:'||r.id WHERE r.id<>NEW.id;
END;

CREATE TRIGGER workspace_revision_memories_immutable BEFORE UPDATE ON workspace_revision_memories
BEGIN SELECT RAISE(ABORT,'workspace input identity is immutable'); END;

CREATE TRIGGER workspace_revision_inputs_immutable BEFORE UPDATE ON workspace_revision_inputs
BEGIN SELECT RAISE(ABORT,'workspace input identity is immutable'); END;

CREATE TRIGGER workspace_revision_sessions_immutable BEFORE UPDATE ON workspace_revision_sessions
BEGIN SELECT RAISE(ABORT,'workspace input identity is immutable'); END;

CREATE TRIGGER workspace_revision_memories_sealed BEFORE DELETE ON workspace_revision_memories
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=OLD.revision AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'sealed workspace inputs require retention coordination'); END;

CREATE TRIGGER workspace_revision_inputs_sealed BEFORE DELETE ON workspace_revision_inputs
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=OLD.revision AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'sealed workspace inputs require retention coordination'); END;

CREATE TRIGGER workspace_revision_sessions_sealed BEFORE DELETE ON workspace_revision_sessions
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=OLD.revision AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'sealed workspace inputs require retention coordination'); END;

CREATE TRIGGER workspace_session_source_committed BEFORE INSERT ON workspace_session_sources
WHEN NOT EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=NEW.revision AND state='committed')
BEGIN SELECT RAISE(ABORT,'workspace source must be committed'); END;

CREATE TRIGGER workspace_write_sessions_identity BEFORE UPDATE OF id,workspace_id,task_id,artifact_id,path,base_revision,begin_key,request_hash,config_revision,max_bytes,privacy,trusted_only,provenance,created_at ON workspace_write_sessions
BEGIN SELECT RAISE(ABORT,'write session identity is immutable'); END;

CREATE TRIGGER workspace_write_sessions_transition BEFORE UPDATE OF state ON workspace_write_sessions
WHEN NOT ((OLD.state='open' AND NEW.state='sealed') OR (OLD.state='sealed' AND NEW.state IN ('committed','aborted')))
BEGIN SELECT RAISE(ABORT,'invalid write session transition'); END;

CREATE TRIGGER workspace_write_sessions_task BEFORE INSERT ON workspace_write_sessions
WHEN NOT EXISTS(SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE w.id=NEW.workspace_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'write session task does not own workspace'); END;

CREATE TRIGGER workspace_write_chunks_identity BEFORE UPDATE OF session_id,chunk_index,object_id,hash,size_bytes,reservation_id ON workspace_write_chunks
BEGIN SELECT RAISE(ABORT,'chunk identity is immutable'); END;

CREATE TRIGGER workspace_write_chunks_transition BEFORE UPDATE OF state ON workspace_write_chunks
WHEN OLD.state<>'pending' OR NEW.state<>'durable'
BEGIN SELECT RAISE(ABORT,'invalid chunk transition'); END;

CREATE TRIGGER workspace_write_chunks_append BEFORE INSERT ON workspace_write_chunks
WHEN NEW.state<>'pending' OR NOT EXISTS(SELECT 1 FROM workspace_write_sessions s JOIN workspace_reservations r ON r.id=NEW.reservation_id WHERE s.id=NEW.session_id AND s.state='open' AND r.workspace_id=s.workspace_id AND r.task_id=s.task_id AND r.state='held' AND r.purpose='scratch' AND r.size_bytes=NEW.size_bytes)
 OR NEW.chunk_index<>(SELECT COUNT(*) FROM workspace_write_chunks WHERE session_id=NEW.session_id)
 OR EXISTS(SELECT 1 FROM workspace_write_chunks WHERE session_id=NEW.session_id AND state<>'durable')
 OR NEW.size_bytes+COALESCE((SELECT SUM(size_bytes) FROM workspace_write_chunks WHERE session_id=NEW.session_id),0)>(SELECT max_bytes FROM workspace_write_sessions WHERE id=NEW.session_id)
BEGIN SELECT RAISE(ABORT,'invalid chunk append or file ceiling'); END;

CREATE TRIGGER workspace_write_commit_keys_immutable BEFORE UPDATE ON workspace_write_commit_keys
BEGIN SELECT RAISE(ABORT,'write commit key is immutable'); END;

CREATE TRIGGER workspace_write_sessions_initial BEFORE INSERT ON workspace_write_sessions
WHEN NEW.state<>'open'
BEGIN SELECT RAISE(ABORT,'write session must start open'); END;

CREATE TRIGGER workspace_write_chunks_reservation_pin BEFORE UPDATE OF state ON workspace_reservations
WHEN EXISTS(SELECT 1 FROM workspace_write_chunks c JOIN workspace_write_sessions s ON s.id=c.session_id WHERE c.reservation_id=OLD.id AND s.state IN ('open','sealed'))
BEGIN SELECT RAISE(ABORT,'unresolved write session pins scratch reservation'); END;

CREATE TRIGGER workspace_write_commit_keys_scope BEFORE INSERT ON workspace_write_commit_keys
WHEN NOT EXISTS(SELECT 1 FROM workspace_write_sessions WHERE id=NEW.session_id AND task_id=NEW.task_id)
BEGIN SELECT RAISE(ABORT,'commit key task does not own write session'); END;

CREATE TRIGGER workspace_write_chunk_keys_immutable BEFORE UPDATE ON workspace_write_chunk_keys
BEGIN SELECT RAISE(ABORT,'write chunk key is immutable'); END;

CREATE TRIGGER workspace_write_chunk_keys_scope BEFORE INSERT ON workspace_write_chunk_keys
WHEN NOT EXISTS(SELECT 1 FROM workspace_write_sessions WHERE id=NEW.session_id AND task_id=NEW.task_id)
BEGIN SELECT RAISE(ABORT,'chunk key task does not own write session'); END;

CREATE TRIGGER mail_reviews_closed_final
BEFORE UPDATE OF state ON mail_reviews
WHEN OLD.state <> 'open'
BEGIN
    SELECT RAISE(ABORT, 'a closed mail review cannot be reopened');
END;

CREATE TRIGGER workspace_reservations_quota BEFORE INSERT ON workspace_reservations
WHEN NEW.size_bytes +
 COALESCE((SELECT SUM(r.size_bytes) FROM workspace_reservations r JOIN agent_workspaces w ON w.id=r.workspace_id WHERE w.agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id) AND r.state='held'),0) +
 COALESCE((SELECT SUM(r.size_bytes) FROM workspace_revisions r JOIN workspace_publications p ON p.revision_id=r.id JOIN workspace_artifacts a ON a.id=r.artifact_id JOIN agent_workspaces w ON w.id=a.workspace_id WHERE p.state='committed' AND w.agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id)),0) >
 COALESCE((SELECT limit_bytes FROM workspace_quota_policy WHERE agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id)),1073741824)
BEGIN SELECT RAISE(ABORT,'quota_exceeded: workspace quota exceeded for Agent'); END;

CREATE TRIGGER workspace_directories_identity BEFORE UPDATE OF id,workspace_id,path,revision,created_at ON workspace_directories
BEGIN SELECT RAISE(ABORT,'directory identity is immutable'); END;

CREATE TRIGGER workspace_directories_transition BEFORE UPDATE OF state ON workspace_directories
WHEN OLD.state<>'committed' OR NEW.state<>'removed'
BEGIN SELECT RAISE(ABORT,'invalid directory transition'); END;

CREATE TRIGGER workspace_directories_no_delete BEFORE DELETE ON workspace_directories
BEGIN SELECT RAISE(ABORT,'directory history requires retention coordination'); END;

CREATE TRIGGER workspace_directory_collision BEFORE INSERT ON workspace_directories
WHEN NEW.state<>'committed' OR EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=NEW.workspace_id AND a.state<>'trashed' AND
 (a.path=NEW.path OR substr(NEW.path,1,length(a.path)+1)=a.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_write_sessions s WHERE s.workspace_id=NEW.workspace_id AND s.state IN ('open','sealed') AND
 (s.path=NEW.path OR substr(NEW.path,1,length(s.path)+1)=s.path||'/'))
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

CREATE TRIGGER workspace_namespace_identity BEFORE UPDATE OF id,workspace_id,task_id,operation,effect_key,request_hash,path,destination,artifact_id,expected_revision,directory_id,directory_revision,created_at ON workspace_namespace_intents
BEGIN SELECT RAISE(ABORT,'namespace intent is immutable'); END;

CREATE TRIGGER workspace_namespace_initial BEFORE INSERT ON workspace_namespace_intents
WHEN NEW.state<>'prepared' OR NOT EXISTS(SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE w.id=NEW.workspace_id AND w.visibility='private' AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'invalid namespace intent scope or state'); END;

CREATE TRIGGER workspace_namespace_transition BEFORE UPDATE OF state ON workspace_namespace_intents
WHEN OLD.state<>'prepared' OR NEW.state NOT IN ('committed','aborted')
BEGIN SELECT RAISE(ABORT,'invalid namespace transition'); END;

CREATE TRIGGER workspace_namespace_no_delete BEFORE DELETE ON workspace_namespace_intents
BEGIN SELECT RAISE(ABORT,'namespace receipt history requires retention coordination'); END;

CREATE TRIGGER workspace_directory_empty BEFORE UPDATE OF state ON workspace_directories
WHEN NEW.state='removed' AND (
 EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=OLD.workspace_id AND a.state<>'trashed' AND substr(a.path,1,length(OLD.path)+1)=OLD.path||'/') OR
 EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=OLD.workspace_id AND d.state='committed' AND substr(d.path,1,length(OLD.path)+1)=OLD.path||'/') OR
 EXISTS(SELECT 1 FROM workspace_write_sessions s WHERE s.workspace_id=OLD.workspace_id AND s.state IN ('open','sealed') AND substr(s.path,1,length(OLD.path)+1)=OLD.path||'/'))
BEGIN SELECT RAISE(ABORT,'workspace directory is not empty'); END;

CREATE TRIGGER workspace_artifact_namespace_insert BEFORE INSERT ON workspace_artifacts
WHEN EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.state='committed' AND
 (d.path=NEW.path OR substr(d.path,1,length(NEW.path)+1)=NEW.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=NEW.workspace_id AND a.state<>'trashed' AND
 (substr(a.path,1,length(NEW.path)+1)=NEW.path||'/' OR substr(NEW.path,1,length(a.path)+1)=a.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_namespace_intents n WHERE n.workspace_id=NEW.workspace_id AND n.state='prepared' AND
 (n.path=NEW.path OR n.destination=NEW.path OR substr(NEW.path,1,length(n.path)+1)=n.path||'/' OR substr(n.path,1,length(NEW.path)+1)=NEW.path||'/' OR
 (n.destination<>'' AND (substr(NEW.path,1,length(n.destination)+1)=n.destination||'/' OR substr(n.destination,1,length(NEW.path)+1)=NEW.path||'/'))))
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

CREATE TRIGGER workspace_artifact_namespace_update BEFORE UPDATE OF path,state ON workspace_artifacts
WHEN NEW.state<>'trashed' AND (EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.state='committed' AND
 (d.path=NEW.path OR substr(d.path,1,length(NEW.path)+1)=NEW.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=NEW.workspace_id AND a.id<>NEW.id AND a.state<>'trashed' AND
 (substr(a.path,1,length(NEW.path)+1)=NEW.path||'/' OR substr(NEW.path,1,length(a.path)+1)=a.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_namespace_intents n WHERE n.workspace_id=NEW.workspace_id AND n.state='prepared' AND
 (n.path=NEW.path OR n.destination=NEW.path OR substr(NEW.path,1,length(n.path)+1)=n.path||'/' OR substr(n.path,1,length(NEW.path)+1)=NEW.path||'/' OR
 (n.destination<>'' AND (substr(NEW.path,1,length(n.destination)+1)=n.destination||'/' OR substr(n.destination,1,length(NEW.path)+1)=NEW.path||'/')))))
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

CREATE TRIGGER workspace_session_namespace BEFORE INSERT ON workspace_write_sessions
WHEN EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.state='committed' AND
 (d.path=NEW.path OR substr(d.path,1,length(NEW.path)+1)=NEW.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=NEW.workspace_id AND a.state<>'trashed' AND
 (substr(a.path,1,length(NEW.path)+1)=NEW.path||'/' OR substr(NEW.path,1,length(a.path)+1)=a.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_namespace_intents n WHERE n.workspace_id=NEW.workspace_id AND n.state='prepared' AND
 (n.artifact_id=NEW.artifact_id OR n.path=NEW.path OR n.destination=NEW.path OR substr(NEW.path,1,length(n.path)+1)=n.path||'/' OR substr(n.path,1,length(NEW.path)+1)=NEW.path||'/' OR
 (n.destination<>'' AND (substr(NEW.path,1,length(n.destination)+1)=n.destination||'/' OR substr(n.destination,1,length(NEW.path)+1)=NEW.path||'/'))))
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

CREATE TRIGGER workspace_publication_namespace BEFORE INSERT ON workspace_publications
WHEN EXISTS(SELECT 1 FROM workspace_namespace_intents n WHERE n.workspace_id=NEW.workspace_id AND n.artifact_id=NEW.artifact_id AND n.state='prepared')
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

CREATE TRIGGER workspace_artifact_namespace_pinned BEFORE UPDATE OF path,state,current_revision ON workspace_artifacts
WHEN EXISTS(SELECT 1 FROM workspace_namespace_intents n WHERE n.artifact_id=OLD.id AND n.state='prepared')
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

CREATE TRIGGER workspace_artifact_parents AFTER INSERT ON workspace_artifacts
BEGIN
 INSERT INTO workspace_directories(id,workspace_id,path,revision,created_at)
 SELECT lower(hex(randomblob(16))),NEW.workspace_id,parent,lower(hex(randomblob(16))),NEW.created_at
 FROM (
  WITH RECURSIVE parents(parent,tail) AS (
   SELECT '',NEW.path
   UNION ALL SELECT CASE WHEN parent='' THEN substr(tail,1,instr(tail,'/')-1) ELSE parent||'/'||substr(tail,1,instr(tail,'/')-1) END,substr(tail,instr(tail,'/')+1)
   FROM parents WHERE instr(tail,'/')>0
  ) SELECT parent FROM parents WHERE parent<>''
 ) WHERE NOT EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.path=parent AND d.state='committed');
END;

CREATE TRIGGER workspace_artifact_move_parents AFTER UPDATE OF path ON workspace_artifacts
BEGIN
 INSERT INTO workspace_directories(id,workspace_id,path,revision,created_at)
 SELECT lower(hex(randomblob(16))),NEW.workspace_id,parent,lower(hex(randomblob(16))),NEW.created_at
 FROM (
  WITH RECURSIVE parents(parent,tail) AS (
   SELECT '',NEW.path
   UNION ALL SELECT CASE WHEN parent='' THEN substr(tail,1,instr(tail,'/')-1) ELSE parent||'/'||substr(tail,1,instr(tail,'/')-1) END,substr(tail,instr(tail,'/')+1)
   FROM parents WHERE instr(tail,'/')>0
  ) SELECT parent FROM parents WHERE parent<>''
 ) WHERE NOT EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.path=parent AND d.state='committed');
END;

CREATE TRIGGER workspace_session_parents AFTER INSERT ON workspace_write_sessions
BEGIN
 INSERT INTO workspace_directories(id,workspace_id,path,revision,created_at)
 SELECT lower(hex(randomblob(16))),NEW.workspace_id,parent,lower(hex(randomblob(16))),NEW.created_at
 FROM (
  WITH RECURSIVE parents(parent,tail) AS (
   SELECT '',NEW.path
   UNION ALL SELECT CASE WHEN parent='' THEN substr(tail,1,instr(tail,'/')-1) ELSE parent||'/'||substr(tail,1,instr(tail,'/')-1) END,substr(tail,instr(tail,'/')+1)
   FROM parents WHERE instr(tail,'/')>0
  ) SELECT parent FROM parents WHERE parent<>''
 ) WHERE NOT EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.path=parent AND d.state='committed');
END;

CREATE TRIGGER workspace_directory_parents AFTER INSERT ON workspace_directories
BEGIN
 INSERT INTO workspace_directories(id,workspace_id,path,revision,created_at)
 SELECT lower(hex(randomblob(16))),NEW.workspace_id,parent,lower(hex(randomblob(16))),NEW.created_at
 FROM (
  WITH RECURSIVE parents(parent,tail) AS (
   SELECT '',NEW.path
   UNION ALL SELECT CASE WHEN parent='' THEN substr(tail,1,instr(tail,'/')-1) ELSE parent||'/'||substr(tail,1,instr(tail,'/')-1) END,substr(tail,instr(tail,'/')+1)
   FROM parents WHERE instr(tail,'/')>0
  ) SELECT parent FROM parents WHERE parent<>''
 ) WHERE NOT EXISTS(SELECT 1 FROM workspace_directories d WHERE d.workspace_id=NEW.workspace_id AND d.path=parent AND d.state='committed');
END;

