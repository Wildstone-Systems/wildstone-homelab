-- The runtime schema as it stood after migrations/0039_person_core.sql,
-- before 0043 replaced the free-text person columns with person_id.
-- personref's tests survey this shape: it is the deployment the
-- pre-flight report exists for. Frozen when the chain was squashed into
-- migrations/0000_baseline.sql (ADR-W007 A4, "Baseline"), because the
-- chain that built it no longer ships. Schema only; no rows.

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
, surface TEXT NOT NULL DEFAULT 'admin', butler TEXT NOT NULL DEFAULT '');

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

CREATE TABLE chat_bindings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    transport    TEXT NOT NULL,          -- 'telegram', 'discord', ...
    account      TEXT NOT NULL,          -- the transport's own user id
    surface      TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    identity     TEXT NOT NULL,          -- principal (admin) or household member (family)
    display_name TEXT NOT NULL DEFAULT '', -- what the transport calls them, for the operator's list
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,          -- who issued the code this redeemed
    last_used_at TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT NOT NULL DEFAULT ''
, conversation TEXT NOT NULL DEFAULT '');

CREATE TABLE chat_claim_codes (
    hash        TEXT PRIMARY KEY,
    surface     TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    identity    TEXT NOT NULL,
    transport   TEXT NOT NULL DEFAULT '',  -- '' means any transport
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT NOT NULL DEFAULT '',
    binding_id  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE chat_outbound (
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    surface        TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    transport      TEXT NOT NULL,
    target         TEXT NOT NULL,          -- conversation id: a direct chat or a group
    body           TEXT NOT NULL,
    kind           TEXT NOT NULL DEFAULT 'reply',  -- 'reply' | 'push'
    binding_id     INTEGER NOT NULL DEFAULT 0,
    correlation_id TEXT NOT NULL DEFAULT '',
    dedupe_key     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    delivered_at   TEXT NOT NULL DEFAULT ''
, hold_until TEXT NOT NULL DEFAULT '');

CREATE TABLE chat_questions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    correlation_id TEXT NOT NULL,
    surface        TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    transport      TEXT NOT NULL,
    target         TEXT NOT NULL,
    rule           TEXT NOT NULL DEFAULT '',
    subject        TEXT NOT NULL DEFAULT '',
    asked_at       TEXT NOT NULL,
    answered_at    TEXT NOT NULL DEFAULT ''
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
);

CREATE TABLE chat_threads (
    transport    TEXT NOT NULL,
    conversation TEXT NOT NULL,
    surface      TEXT NOT NULL,
    identity     TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,   -- RFC3339Nano UTC
    PRIMARY KEY (transport, conversation, surface, identity)
);

CREATE TABLE butler_tasks (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at          TEXT NOT NULL,     -- RFC3339Nano UTC
    updated_at          TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('conversation', 'routine')),
    state               TEXT NOT NULL CHECK (state IN (
                            'pending', 'running', 'waiting',
                            'completed', 'failed', 'cancelled',
                            'paused-offboarded')),

    -- author is the member who asked for the work and never changes;
    -- owner is who operates it now. Offboarding moves a shared task's
    -- owner to the homelab administrator and "preserves original
    -- authorship" (A4) — two columns is what that sentence costs.
    author              TEXT NOT NULL,
    owner               TEXT NOT NULL,

    -- surface and profile are the authority the task was admitted under.
    -- They are snapshotted because authority is rechecked on resume
    -- against what the task CLAIMED, not against whatever the caller
    -- would get today (A4, A9).
    surface             TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    profile             TEXT NOT NULL,
    audience            TEXT NOT NULL CHECK (audience IN ('private', 'shared')),

    -- Conversation linkage. Both nullable: a routine run has no
    -- conversation. origin_turn is the butler_session_turns ordinal that
    -- started the task, so a Console timeline can point back at the
    -- exact message without a join through text.
    session             INTEGER REFERENCES butler_sessions(seq),
    origin_turn         INTEGER,

    -- correlation_id ties a routine run to the W024 household timeline's
    -- occurrence/action ids. The timeline stays authoritative for
    -- deterministic outcomes; anything copied here is a projection (A4).
    correlation_id      TEXT NOT NULL DEFAULT '',

    objective           TEXT NOT NULL,
    completion_criteria TEXT NOT NULL,

    -- Budgets and grants are persisted so they survive a restart and do
    -- NOT reset on retry (A4). Waiting does not consume active-work
    -- budget; the absolute deadline still expires.
    grants_json         TEXT NOT NULL DEFAULT '{}',
    budget_json         TEXT NOT NULL DEFAULT '{}',
    deadline_at         TEXT NOT NULL DEFAULT '',
    next_wakeup_at      TEXT NOT NULL DEFAULT '',

    -- lease_epoch fences attempts and, in P3, the single active
    -- coordinator (A11). It only ever increases.
    lease_epoch         INTEGER NOT NULL DEFAULT 0,

    -- uncertainty records that an action's outcome is unknown, so a
    -- resume pauses instead of blindly retrying it (A4).
    uncertainty         TEXT NOT NULL DEFAULT '',
    result_summary      TEXT NOT NULL DEFAULT '',

    -- retention follows the conversation's configured policy; the
    -- default is manual deletion (A10).
    retention           TEXT NOT NULL CHECK (retention IN ('manual', 'conversation', 'ephemeral')),
    envelope_version    INTEGER NOT NULL
, butler TEXT NOT NULL DEFAULT '');

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

CREATE TABLE butler_task_waits (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    task        INTEGER NOT NULL REFERENCES butler_tasks(seq),
    created_at  TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN (
                    'reply', 'approval', 'schedule', 'model-unavailable',
                    'budget-exhausted', 'clarification', 'source-unavailable',
                    'uncertain-outcome')),
    detail      TEXT NOT NULL DEFAULT '',
    resume_at   TEXT NOT NULL DEFAULT '',
    resolved_at TEXT NOT NULL DEFAULT '',
    resolved_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE butler_task_evidence (
    seq              INTEGER PRIMARY KEY AUTOINCREMENT,
    task             INTEGER NOT NULL REFERENCES butler_tasks(seq),
    recorded_at      TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('observation', 'tool-result', 'approval', 'delivery', 'external-verification')),
    description      TEXT NOT NULL,
    source_id        TEXT NOT NULL DEFAULT '',
    observed_at      TEXT NOT NULL DEFAULT '',
    permission_label TEXT NOT NULL,
    audience         TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (description <> '')
);

CREATE TABLE butler_work_trace (
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
    audience         TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    redacted         INTEGER NOT NULL DEFAULT 1,
    payload_json     TEXT NOT NULL DEFAULT '{}',
    envelope_version INTEGER NOT NULL,
    CHECK (permission_label <> ''),
    CHECK (type <> '')
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
    updated_by        TEXT NOT NULL DEFAULT '', index_conversations INTEGER NOT NULL DEFAULT 1, transcript_retention_days INTEGER NOT NULL DEFAULT 180,

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

CREATE TABLE butler_identity_links (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    link_type   TEXT NOT NULL CHECK (link_type IN ('api-principal', 'chat-binding', 'trusted-ingress')),
    -- external_id is the principal name, the chat binding id rendered as
    -- text, or the ingress subject. Stored as text so one column serves
    -- all three without a nullable pair.
    external_id TEXT NOT NULL,
    member      TEXT NOT NULL REFERENCES butler_members(member),
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

CREATE TABLE butler_conversations (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    member     TEXT NOT NULL REFERENCES butler_members(member),
    created_at TEXT NOT NULL,
    subject    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE butler_conversation_sessions (
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    session      INTEGER NOT NULL REFERENCES butler_sessions(seq),
    attached_at  TEXT NOT NULL,
    PRIMARY KEY (conversation, session)
);

CREATE TABLE butler_handoffs (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation INTEGER NOT NULL REFERENCES butler_conversations(seq),
    member       TEXT NOT NULL,
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

CREATE TABLE butler_tool_controls (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    profile    TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    tool       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    CHECK (tool <> ''),
    CHECK (updated_by <> '')
);

CREATE TABLE butler_tool_grants (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL CHECK (scope IN ('task', 'routine')),
    -- scope_id is the task id ("bt-<n>") or the routine name. Text so
    -- one column serves both.
    scope_id   TEXT NOT NULL,
    profile    TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
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

CREATE TABLE butler_capability_requests (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('supported-tool', 'development')),
    -- requested_by is the MEMBER the asking session was acting for. The
    -- Butler asks on someone's behalf; it does not have wishes of its
    -- own, and an unattributed request is one nobody can weigh.
    requested_by TEXT NOT NULL,
    profile     TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
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

CREATE TABLE butler_memory (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- kind is closed and deliberately contains no "permission",
    -- "grant" or "rule" member. What the Butler may DO is decided by
    -- ADR-W030 A7's controls and grants, and there is no kind here that
    -- could be mistaken for one.
    kind       TEXT NOT NULL CHECK (kind IN ('fact', 'preference', 'inference', 'procedure')),
    -- subject is who or what the record is about. Retrieval filters on
    -- it before content ever reaches a model.
    subject    TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    -- audience and permission_label are mandatory for the reason every
    -- other table in this ADR makes them mandatory: a record that cannot
    -- be permission-filtered can never be safely shown.
    audience   TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    permission_label TEXT NOT NULL,
    -- owner is the person the record belongs to: who may correct it,
    -- forget it, and export it. For household-shared records it is the
    -- household, and any admitted adult may correct it (A1).
    owner      TEXT NOT NULL,

    -- Provenance. source_kind names the sort of thing it came from —
    -- a conversation, a household event, an app read — and source_id
    -- identifies the exact one, which is what deletion invalidation and
    -- re-extraction suppression both key on.
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    observed_at TEXT NOT NULL DEFAULT '',

    -- confidence separates a stated fact from a tentative inference. An
    -- inference presented with the confidence of a fact is how a
    -- household ends up arguing with its own house.
    confidence TEXT NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated', 'inferred', 'uncertain')),

    -- safety_relevant marks a record whose REMOVAL the household must
    -- hear about immediately rather than in the next digest — an
    -- allergy, a medication, a standing instruction (A1).
    safety_relevant INTEGER NOT NULL DEFAULT 0,

    created_at TEXT NOT NULL,
    author     TEXT NOT NULL,
    -- superseded_by points at the record that corrected this one, so a
    -- correction is a new row and history is not rewritten.
    superseded_by INTEGER REFERENCES butler_memory(seq),
    forgotten_at  TEXT NOT NULL DEFAULT '',
    forgotten_by  TEXT NOT NULL DEFAULT '',
    envelope_version INTEGER NOT NULL, butler TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (permission_label <> ''),
    CHECK (owner <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
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

CREATE TABLE butler_index (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    source_kind TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    -- memory links an indexed chunk to the record it came from, so
    -- forgetting the record removes the chunk (A3).
    memory      INTEGER REFERENCES butler_memory(seq),
    owner       TEXT NOT NULL,
    audience    TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
    permission_label TEXT NOT NULL,
    content     TEXT NOT NULL,
    -- observed_at is when the underlying thing was true. Freshness is
    -- measured from this, not from indexed_at: reindexing an old
    -- conversation does not make it recent.
    observed_at TEXT NOT NULL DEFAULT '',
    indexed_at  TEXT NOT NULL,
    -- volatile marks a source whose CURRENT state cannot be answered
    -- from an index at all — a light that is on or off, a door that is
    -- locked or not. Retrieval returns these flagged for live
    -- verification rather than as answers (A6: changing app state is
    -- verified live, not trusted from RAG).
    volatile    INTEGER NOT NULL DEFAULT 0,
    -- embedding is an optional float32 vector, little-endian, supplied
    -- by whatever local model the deployment has. NULL means this
    -- deployment ranks by keyword alone, which is a working
    -- configuration and not a degraded one.
    embedding   BLOB,
    tokens      INTEGER NOT NULL DEFAULT 0, butler TEXT NOT NULL DEFAULT '',

    CHECK (content <> ''),
    CHECK (owner <> ''),
    CHECK (permission_label <> ''),
    CHECK (source_kind <> ''),
    CHECK (source_id <> '')
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

CREATE TABLE integration_connections (
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
    audience    TEXT NOT NULL CHECK (audience IN ('private', 'shared')),
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
    degraded_reason TEXT NOT NULL DEFAULT '',

    CHECK (name <> ''),
    CHECK (adapter <> ''),
    CHECK (owner <> ''),
    CHECK (created_by <> '')
);

CREATE TABLE integration_principals (
    principal  TEXT NOT NULL,
    connection INTEGER NOT NULL REFERENCES integration_connections(seq),
    bound_at   TEXT NOT NULL,
    bound_by   TEXT NOT NULL,
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
    revoked_at TEXT NOT NULL DEFAULT '',

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
    disabled_by TEXT NOT NULL DEFAULT '', butler TEXT NOT NULL DEFAULT '',

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

CREATE TABLE household_review_tokens (
    hash       TEXT PRIMARY KEY,
    -- plan_id is the ONE proposal this token can approve.
    plan_id    TEXT NOT NULL,
    routine    INTEGER REFERENCES household_routines(seq),
    -- revision pins what was proposed. If the routine has been edited
    -- since, the token approves nothing.
    revision   INTEGER NOT NULL DEFAULT 0,
    -- member is who it was issued to. A token that works for whoever
    -- holds it is a password, and this is not one.
    member     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NOT NULL DEFAULT '',
    used_by    TEXT NOT NULL DEFAULT '',

    CHECK (plan_id <> ''),
    CHECK (member <> ''),
    CHECK (expires_at <> '')
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

CREATE TABLE notices (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    -- recipient is the member. An unassigned notice routes to the
    -- configured administrator (A10) — and does so WITHOUT granting them
    -- access to whatever private thing prompted it, which is why the
    -- body is written for the recipient it ends up with.
    recipient  TEXT NOT NULL,
    routed_to_admin INTEGER NOT NULL DEFAULT 0,
    -- correlation_id ties the notice to the task or plan it is about.
    correlation_id TEXT NOT NULL DEFAULT '',
    -- outbound is the chat_outbound row that carried it, when one did.
    outbound   INTEGER,
    dedupe_key TEXT NOT NULL,
    urgent     INTEGER NOT NULL DEFAULT 0,
    relevance  INTEGER NOT NULL DEFAULT 50,
    created_at TEXT NOT NULL,
    hold_until TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,

    -- The three states, deliberately three columns.
    delivered_at    TEXT NOT NULL DEFAULT '',
    acknowledged_at TEXT NOT NULL DEFAULT '',
    -- undelivered_at is set when the notice expired without reaching
    -- anyone. It is visible in Console: a notice nobody can see was
    -- missed is a notice that was silently dropped.
    undelivered_at  TEXT NOT NULL DEFAULT '',

    -- asks marks a notice that poses a question. A notice that does not
    -- ask cannot be answered, so a reply to one authorizes nothing.
    asks       INTEGER NOT NULL DEFAULT 0,
    -- answered_at and answer record a human's reply, which is a
    -- different fact from acknowledgement.
    answered_at TEXT NOT NULL DEFAULT '',
    answer      TEXT NOT NULL DEFAULT '',
    answered_by TEXT NOT NULL DEFAULT '',

    CHECK (kind <> ''),
    CHECK (recipient <> ''),
    CHECK (dedupe_key <> ''),
    -- A notice that answers a question it never asked would be an
    -- authorization from nowhere.
    CHECK (answered_at = '' OR asks = 1)
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

CREATE TABLE offboarding_records (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    member     TEXT NOT NULL,
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    store      TEXT NOT NULL,
    affected   INTEGER NOT NULL DEFAULT 0,
    detail     TEXT NOT NULL DEFAULT '',
    CHECK (member <> ''),
    CHECK (actor <> ''),
    CHECK (store <> '')
);

CREATE TABLE butlers (
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
    -- surface is the structural ceiling. A family Butler cannot be
    -- given an operator tool because the family gateway does not
    -- contain one — configuration selects from what the ceiling allows
    -- and can never add to it (A3).
    surface    TEXT NOT NULL CHECK (surface IN ('admin', 'family')),
    -- profile is the ceiling template within that surface, matching
    -- butler_members. 'child' is storable and, as there, not yet
    -- admissible.
    profile    TEXT NOT NULL CHECK (profile IN ('household', 'operator', 'child')),
    -- route is the assigned model. NOT NULL and a real reference: the
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
    CHECK (created_by <> '')
);

CREATE TABLE butler_access (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    butler     INTEGER NOT NULL REFERENCES butlers(seq),
    member     TEXT NOT NULL,
    granted_at TEXT NOT NULL,
    granted_by TEXT NOT NULL,
    revoked_at TEXT NOT NULL DEFAULT '',
    revoked_by TEXT NOT NULL DEFAULT '',

    CHECK (member <> ''),
    CHECK (granted_by <> '')
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

CREATE TABLE "butler_members" (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,

    -- person_id is the identity. It is opaque and carries no meaning:
    -- not the handle, not the seq, not a directory login, nothing that
    -- could change or that anyone could be tempted to parse. A Person
    -- keeps it across a rename, a re-admission and a directory move,
    -- which is the entire point of having it.
    person_id    TEXT NOT NULL UNIQUE,

    -- member is now a display and login HANDLE, not the identity.
    --
    -- It stays strictly unique in this slice. §2.2 wants it unique
    -- among ACTIVE people, which would let a departed person's handle
    -- be reused — but "active" here is a predicate over three columns
    -- (admitted, disabled_at, offboarded_at), and a partial unique
    -- index over it would let a staged, not-yet-admitted row claim an
    -- active person's handle. That is a new lifecycle rule, not a
    -- constraint change, and inventing one to enable a reuse nobody has
    -- asked for yet is how a schema acquires behaviour no design
    -- document describes. Handle reuse waits for the slice that
    -- actually specifies it.
    member       TEXT NOT NULL UNIQUE,

    -- role is the RBAC axis. Two values, closed (§2.3).
    role         TEXT NOT NULL CHECK (role IN ('owner', 'member')),

    -- profile is the preset axis, and NULL is the ordinary case: most
    -- Members are just Members. The CHECK is closed at one value so a
    -- future preset is a deliberate migration rather than whatever
    -- string a caller happened to pass.
    profile      TEXT CHECK (profile IS NULL OR profile IN ('child')),

    admitted     INTEGER NOT NULL DEFAULT 0,
    -- generation increments on every admission, disable, unlink and
    -- offboarding. It is what makes revocation immediate.
    generation   INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    admitted_at  TEXT NOT NULL DEFAULT '',
    admitted_by  TEXT NOT NULL DEFAULT '',
    disabled_at  TEXT NOT NULL DEFAULT '',
    disabled_by  TEXT NOT NULL DEFAULT '',
    -- Offboarding is one audited administrator action that immediately
    -- denies access. Desired-state cleanup follows through Plans; failed
    -- cleanup never restores access (A2.1).
    offboarded_at TEXT NOT NULL DEFAULT '',
    offboarded_by TEXT NOT NULL DEFAULT '',
    -- guardian is required for a child record and meaningless otherwise.
    guardian     TEXT NOT NULL DEFAULT '',

    CHECK (member <> ''),
    CHECK (person_id <> ''),
    -- A child is never admitted. Carried over from 0021 unchanged: the
    -- schema refuses the row, so no code path, no migration and no
    -- console form can produce an admitted child before that surface
    -- has its own evidence (ADR-W030 Part C).
    CHECK (NOT (profile = 'child' AND admitted = 1)),
    CHECK (profile <> 'child' OR guardian <> ''),
    -- A child is a constrained MEMBER (§2.3). Without this the preset
    -- and the role could drift apart in storage, and "is a child an
    -- Owner?" would become a question the data could answer either way.
    CHECK (profile <> 'child' OR role = 'member')
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


CREATE INDEX backup_runs_by_plan ON backup_runs (plan_name, run_kind, seq);

CREATE INDEX butler_sessions_by_caller ON butler_sessions (acting_for, seq);

CREATE INDEX household_events_by_type ON household_events (type, seq);

CREATE INDEX household_events_by_subject ON household_events (subject, seq);

CREATE INDEX butler_sessions_by_surface ON butler_sessions (surface, seq);

CREATE UNIQUE INDEX chat_bindings_live
    ON chat_bindings (transport, account, surface) WHERE revoked_at = '';

CREATE INDEX chat_bindings_by_identity ON chat_bindings (identity, id);

CREATE UNIQUE INDEX chat_outbound_dedupe ON chat_outbound (dedupe_key);

CREATE INDEX chat_outbound_pending ON chat_outbound (surface, transport, seq);

CREATE INDEX chat_questions_open ON chat_questions (transport, target, answered_at, id);

CREATE UNIQUE INDEX butler_session_turns_order ON butler_session_turns (session, ordinal);

CREATE INDEX butler_tasks_by_owner ON butler_tasks (owner, seq);

CREATE INDEX butler_tasks_by_session ON butler_tasks (session, seq);

CREATE INDEX butler_tasks_by_state ON butler_tasks (state, seq);

CREATE INDEX butler_tasks_by_wakeup ON butler_tasks (next_wakeup_at) WHERE next_wakeup_at <> '';

CREATE UNIQUE INDEX butler_task_steps_order ON butler_task_steps (task, ordinal);

CREATE UNIQUE INDEX butler_task_attempts_order ON butler_task_attempts (step, attempt);

CREATE INDEX butler_task_attempts_by_task ON butler_task_attempts (task, seq);

CREATE INDEX butler_task_transitions_by_task ON butler_task_transitions (task, seq);

CREATE INDEX butler_task_waits_by_task ON butler_task_waits (task, seq);

CREATE UNIQUE INDEX butler_task_waits_open ON butler_task_waits (task) WHERE resolved_at = '';

CREATE INDEX butler_task_evidence_by_task ON butler_task_evidence (task, seq);

CREATE INDEX butler_work_trace_by_task ON butler_work_trace (task, seq);

CREATE INDEX butler_task_feedback_by_task ON butler_task_feedback (task, seq);

CREATE INDEX butler_task_feedback_by_label ON butler_task_feedback (label, seq);

CREATE UNIQUE INDEX butler_evaluations_name ON butler_evaluations (name);

CREATE INDEX butler_debug_capture_by_task ON butler_debug_capture (task, seq);

CREATE UNIQUE INDEX butler_identity_links_live
    ON butler_identity_links (link_type, external_id) WHERE revoked_at = '';

CREATE INDEX butler_identity_links_by_member ON butler_identity_links (member, seq);

CREATE INDEX butler_conversations_by_member ON butler_conversations (member, seq);

CREATE INDEX butler_handoffs_by_conversation ON butler_handoffs (conversation, seq);

CREATE UNIQUE INDEX butler_tool_controls_key ON butler_tool_controls (profile, tool);

CREATE INDEX butler_tool_grants_live ON butler_tool_grants (scope, scope_id, tool);

CREATE INDEX butler_capability_requests_state ON butler_capability_requests (state, seq);

CREATE INDEX butler_memory_eligible ON butler_memory (subject, forgotten_at, seq);

CREATE INDEX butler_memory_by_source ON butler_memory (source_kind, source_id);

CREATE INDEX butler_memory_by_owner ON butler_memory (owner, seq);

CREATE INDEX butler_memory_derivations_by_source ON butler_memory_derivations (source);

CREATE INDEX butler_memory_digest_pending ON butler_memory_digest (delivered_at, seq);

CREATE INDEX butler_index_by_source ON butler_index (source_kind, source_id);

CREATE INDEX butler_index_by_memory ON butler_index (memory);

CREATE INDEX butler_index_permission ON butler_index (audience, owner, seq);

CREATE INDEX butler_index_jobs_queue ON butler_index_jobs (state, priority, seq);

CREATE UNIQUE INDEX integration_capabilities_key ON integration_capabilities (connection, kind, name);

CREATE INDEX integration_work_queue ON integration_work (connection, state, seq);

CREATE INDEX integration_work_correlation ON integration_work (correlation_id);

CREATE UNIQUE INDEX integration_placements_node ON integration_placements (connection, node);

CREATE UNIQUE INDEX integration_placements_holder
    ON integration_placements (connection) WHERE holder = 1;

CREATE UNIQUE INDEX integration_secrets_ref ON integration_secrets (connection, member, secret_ref);

CREATE INDEX household_review_tokens_plan ON household_review_tokens (plan_id);

CREATE UNIQUE INDEX notices_dedupe ON notices (dedupe_key);

CREATE INDEX notices_by_recipient ON notices (recipient, seq);

CREATE INDEX notices_pending ON notices (delivered_at, expires_at);

CREATE INDEX deletion_journal_subject ON deletion_journal (subject_kind, subject_id);

CREATE INDEX offboarding_records_member ON offboarding_records (member, seq);

CREATE INDEX butlers_live ON butlers (enabled, retired_at, surface);

CREATE UNIQUE INDEX butler_access_live ON butler_access (butler, member)
    WHERE revoked_at = '';

CREATE INDEX butler_access_member ON butler_access (member, revoked_at);

CREATE INDEX butler_memory_scope ON butler_memory (butler, audience, owner, seq);

CREATE INDEX butler_index_scope ON butler_index (butler, audience, owner, seq);

CREATE UNIQUE INDEX butler_tool_scope_key ON butler_tool_scope (butler, tool);

CREATE UNIQUE INDEX butler_connection_scope_live
    ON butler_connection_scope (butler, connection) WHERE revoked_at = '';

CREATE INDEX household_routines_butler ON household_routines (butler);

CREATE INDEX butler_tasks_butler ON butler_tasks (butler, state);

CREATE UNIQUE INDEX butler_addresses_live
    ON butler_addresses (transport, external_id) WHERE revoked_at = '';

CREATE INDEX butler_addresses_butler ON butler_addresses (butler, revoked_at);

CREATE INDEX butler_pool_disclosures_memory ON butler_pool_disclosures (memory, seq);

CREATE INDEX butler_sessions_butler ON butler_sessions (butler, seq);

CREATE INDEX models_assignable ON models (enabled, kind, name);

CREATE INDEX butler_members_admitted ON butler_members (admitted, member);

CREATE INDEX butler_members_active_owners
    ON butler_members (role, admitted, disabled_at, offboarded_at);

CREATE INDEX butler_role_changes_by_person ON butler_role_changes (person_id, seq);

CREATE TRIGGER butler_tasks_complete_needs_evidence_update
BEFORE UPDATE OF state ON butler_tasks
WHEN NEW.state = 'completed'
 AND NOT EXISTS (SELECT 1 FROM butler_task_evidence WHERE task = NEW.seq)
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot complete without evidence (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_complete_needs_evidence_insert
BEFORE INSERT ON butler_tasks
WHEN NEW.state = 'completed'
BEGIN
    SELECT RAISE(ABORT, 'butler task cannot be created completed (ADR-W030 A4)');
END;

CREATE TRIGGER butler_tasks_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON butler_tasks
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'butler task lease epoch cannot move backwards (ADR-W030 A11)');
END;

CREATE TRIGGER butler_tasks_author_immutable
BEFORE UPDATE OF author ON butler_tasks
WHEN NEW.author <> OLD.author
BEGIN
    SELECT RAISE(ABORT, 'butler task authorship is immutable (ADR-W030 A4)');
END;

CREATE TRIGGER butler_task_transitions_immutable
BEFORE UPDATE ON butler_task_transitions
BEGIN
    SELECT RAISE(ABORT, 'butler task transitions are append-only (ADR-W030 A11)');
END;

CREATE TRIGGER butler_work_trace_immutable
BEFORE UPDATE ON butler_work_trace
BEGIN
    SELECT RAISE(ABORT, 'butler work trace is append-only (ADR-W030 A11)');
END;

CREATE TRIGGER butler_task_evidence_immutable
BEFORE UPDATE ON butler_task_evidence
BEGIN
    SELECT RAISE(ABORT, 'butler task evidence is append-only (ADR-W030 A4)');
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

CREATE TRIGGER butler_handoffs_immutable
BEFORE UPDATE ON butler_handoffs
BEGIN
    SELECT RAISE(ABORT, 'butler handoffs are append-only (ADR-W030 A2.1)');
END;

CREATE TRIGGER butler_memory_content_immutable
BEFORE UPDATE OF content, source_kind, source_id, author, kind ON butler_memory
BEGIN
    SELECT RAISE(ABORT, 'a memory record is corrected by superseding it, not by editing it (ADR-W030 A3)');
END;

CREATE TRIGGER butler_memory_audience_cannot_loosen
BEFORE UPDATE OF audience ON butler_memory
WHEN OLD.audience = 'private' AND NEW.audience = 'shared'
BEGIN
    SELECT RAISE(ABORT, 'private memory cannot be widened to shared by an update (ADR-W030 A2/A3)');
END;

CREATE TRIGGER butler_index_ai AFTER INSERT ON butler_index BEGIN
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER butler_index_ad AFTER DELETE ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
END;

CREATE TRIGGER butler_index_au AFTER UPDATE OF content ON butler_index BEGIN
    INSERT INTO butler_index_fts (butler_index_fts, rowid, content) VALUES ('delete', old.seq, old.content);
    INSERT INTO butler_index_fts (rowid, content) VALUES (new.seq, new.content);
END;

CREATE TRIGGER integration_work_no_reopen
BEFORE UPDATE OF state ON integration_work
WHEN OLD.state IN ('done', 'cancelled', 'expired') AND NEW.state != OLD.state
BEGIN
    SELECT RAISE(ABORT, 'integration work that has finished cannot be reopened (ADR-W030 A5.1)');
END;

CREATE TRIGGER integration_connections_epoch_monotonic
BEFORE UPDATE OF lease_epoch ON integration_connections
WHEN NEW.lease_epoch < OLD.lease_epoch
BEGIN
    SELECT RAISE(ABORT, 'a connection lease epoch cannot move backwards (ADR-W030 A5)');
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

CREATE TRIGGER household_review_tokens_single_use
BEFORE UPDATE OF used_at ON household_review_tokens
WHEN OLD.used_at <> '' AND NEW.used_at <> OLD.used_at
BEGIN
    SELECT RAISE(ABORT, 'a household review token is single-use (ADR-W030 A8.1)');
END;

CREATE TRIGGER notices_expired_stays_expired
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.undelivered_at <> '' AND NEW.answered_at <> ''
BEGIN
    SELECT RAISE(ABORT, 'an expired notice cannot be answered (ADR-W030 A10.1)');
END;

CREATE TRIGGER notices_answered_once
BEFORE UPDATE OF answered_at ON notices
WHEN OLD.answered_at <> '' AND NEW.answered_at <> OLD.answered_at
BEGIN
    SELECT RAISE(ABORT, 'a notice is answered once (ADR-W030 A10)');
END;

CREATE TRIGGER restore_state_release_needs_replay
BEFORE UPDATE OF released_at ON restore_state
WHEN NEW.released_at <> '' AND (NEW.replayed_at = '' OR NEW.journal_current = 0)
BEGIN
    SELECT RAISE(ABORT, 'quarantine is released only after the deletion journal is asserted current AND replayed (ADR-W030 A10)');
END;

CREATE TRIGGER butlers_surface_immutable
BEFORE UPDATE OF surface ON butlers
WHEN NEW.surface <> OLD.surface
BEGIN
    SELECT RAISE(ABORT, 'a butler cannot change surface: the ceiling is what it was created against (ADR-W031 A3)');
END;

CREATE TRIGGER butlers_profile_immutable
BEFORE UPDATE OF profile ON butlers
WHEN NEW.profile <> OLD.profile
BEGIN
    SELECT RAISE(ABORT, 'a butler cannot change profile: the ceiling is what it was created against (ADR-W031 A3)');
END;

CREATE TRIGGER butler_connection_scope_audience
BEFORE INSERT ON butler_connection_scope
WHEN (SELECT audience FROM integration_connections WHERE seq = NEW.connection) = 'private'
 AND (SELECT surface FROM butlers WHERE seq = NEW.butler) = 'family'
BEGIN
    SELECT RAISE(ABORT, 'that connection is private to one person, so a household-surface butler cannot be given it (ADR-W031 A4)');
END;

CREATE TRIGGER butler_addresses_live_butler
BEFORE INSERT ON butler_addresses
WHEN (SELECT retired_at FROM butlers WHERE seq = NEW.butler) <> ''
BEGIN
    SELECT RAISE(ABORT, 'that butler has been retired, so it cannot be given an address (ADR-W031 A7)');
END;

CREATE TRIGGER models_kind_immutable
BEFORE UPDATE OF kind ON models
WHEN NEW.kind <> OLD.kind
BEGIN
    SELECT RAISE(ABORT, 'a model cannot change kind: local and cloud are different destinations, and a butler was assigned to it on the strength of which one (ADR-W031 A2)');
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

CREATE TRIGGER butler_access_requires_admission
BEFORE INSERT ON butler_access
WHEN (SELECT COUNT(*) FROM butler_members
      WHERE member = NEW.member AND admitted = 1 AND offboarded_at = '') = 0
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

