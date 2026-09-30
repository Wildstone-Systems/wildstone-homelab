-- ADR-W047 / #474. Metadata only: no roots, bytes, grants or cleanup.
-- Additive migration after 0096; successors must allocate numbers after 0097.
CREATE TABLE agent_workspaces (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 agent_id INTEGER NOT NULL REFERENCES butlers(seq),
 created_at TEXT NOT NULL,
 visibility TEXT NOT NULL DEFAULT 'private' CHECK(visibility IN ('private','shared')),
 UNIQUE(person_id,agent_id,visibility)
);
CREATE TRIGGER agent_workspaces_immutable BEFORE UPDATE ON agent_workspaces
BEGIN SELECT RAISE(ABORT,'workspace identity is immutable'); END;

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
CREATE UNIQUE INDEX workspace_artifacts_live_path ON workspace_artifacts(workspace_id,path) WHERE state<>'trashed';
CREATE TRIGGER workspace_artifacts_identity BEFORE UPDATE OF id,workspace_id,created_at ON workspace_artifacts
BEGIN SELECT RAISE(ABORT,'artifact ownership is immutable'); END;

-- Revision facts never change. Visibility is determined by the journal, not
-- the presence of a revision row. Retained history, including trash, is charged.
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
CREATE TRIGGER workspace_revisions_immutable BEFORE UPDATE ON workspace_revisions
BEGIN SELECT RAISE(ABORT,'workspace revisions are immutable'); END;
CREATE TRIGGER workspace_revisions_task_scope BEFORE INSERT ON workspace_revisions
WHEN NOT EXISTS (SELECT 1 FROM workspace_artifacts a JOIN agent_workspaces w ON w.id=a.workspace_id JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE a.id=NEW.artifact_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'revision task does not own workspace'); END;

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
CREATE INDEX workspace_reservations_accounting ON workspace_reservations(workspace_id,state);
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
-- Agent quota spans all owning Persons; readers never duplicate the charge.
CREATE TRIGGER workspace_reservations_quota BEFORE INSERT ON workspace_reservations
WHEN NEW.size_bytes +
 COALESCE((SELECT SUM(r.size_bytes) FROM workspace_reservations r JOIN agent_workspaces w ON w.id=r.workspace_id WHERE w.agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id) AND r.state='held'),0) +
 COALESCE((SELECT SUM(r.size_bytes) FROM workspace_revisions r JOIN workspace_publications p ON p.revision_id=r.id JOIN workspace_artifacts a ON a.id=r.artifact_id JOIN agent_workspaces w ON w.id=a.workspace_id WHERE p.state='committed' AND w.agent_id=(SELECT agent_id FROM agent_workspaces WHERE id=NEW.workspace_id)),0) > 1073741824
BEGIN SELECT RAISE(ABORT,'workspace quota exceeded'); END;

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
CREATE UNIQUE INDEX workspace_publications_pending ON workspace_publications(artifact_id) WHERE state NOT IN ('committed','aborted');
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
-- An unresolved intent pins its reservation until commit/explicit recovery abort.
CREATE TRIGGER workspace_reservations_pinned BEFORE UPDATE OF state ON workspace_reservations
WHEN EXISTS(SELECT 1 FROM workspace_publications p WHERE p.reservation_id=OLD.id AND p.state NOT IN ('committed','aborted'))
BEGIN SELECT RAISE(ABORT,'publication pins reservation'); END;

-- Server-owned provenance. Source references can point outside workspaces;
-- revision edges are typed FKs. Labels are persisted, never inferred from text.
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
CREATE INDEX workspace_provenance_revision ON workspace_provenance(revision_id);
CREATE TRIGGER workspace_provenance_immutable BEFORE UPDATE ON workspace_provenance
BEGIN SELECT RAISE(ABORT,'workspace provenance is immutable'); END;
CREATE TRIGGER workspace_provenance_pending BEFORE INSERT ON workspace_provenance
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=NEW.revision_id AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'prepared provenance is sealed'); END;
CREATE TRIGGER workspace_provenance_privacy BEFORE INSERT ON workspace_provenance
WHEN (NEW.privacy='local-only' OR EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.source_revision AND privacy='local-only')) AND NOT EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.revision_id AND privacy='local-only')
BEGIN SELECT RAISE(ABORT,'derived revision cannot widen source privacy'); END;

CREATE TABLE workspace_receipts (
 id TEXT PRIMARY KEY CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
 publication_id TEXT NOT NULL UNIQUE REFERENCES workspace_publications(id),
 recorded_at TEXT NOT NULL
);
CREATE TRIGGER workspace_receipts_committed BEFORE INSERT ON workspace_receipts
WHEN NOT EXISTS(SELECT 1 FROM workspace_publications WHERE id=NEW.publication_id AND state='committed')
BEGIN SELECT RAISE(ABORT,'receipt requires committed publication'); END;
CREATE TRIGGER workspace_receipts_immutable BEFORE UPDATE ON workspace_receipts
BEGIN SELECT RAISE(ABORT,'workspace receipts are immutable'); END;

CREATE TRIGGER workspace_provenance_delete_sealed BEFORE DELETE ON workspace_provenance
WHEN EXISTS(SELECT 1 FROM workspace_publications WHERE revision_id=OLD.revision_id AND state<>'staging')
BEGIN SELECT RAISE(ABORT,'prepared provenance is sealed'); END;
CREATE TRIGGER workspace_provenance_trusted BEFORE INSERT ON workspace_provenance
WHEN (NEW.trusted_only=1 OR EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.source_revision AND trusted_only=1)) AND NOT EXISTS(SELECT 1 FROM workspace_revisions WHERE id=NEW.revision_id AND trusted_only=1)
BEGIN SELECT RAISE(ABORT,'derived revision cannot widen source trust'); END;

-- Admission is rechecked at each publication transition. Historical attribution
-- is stable on the revision even when an existing task is later reassigned.
CREATE TRIGGER workspace_publications_task_scope BEFORE UPDATE OF state ON workspace_publications
WHEN NEW.state<>'aborted' AND NOT EXISTS(SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE w.id=NEW.workspace_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'publication task no longer owns workspace'); END;

CREATE TRIGGER workspace_publications_no_delete BEFORE DELETE ON workspace_publications
BEGIN SELECT RAISE(ABORT,'publication history requires explicit retention coordination'); END;
CREATE TRIGGER workspace_publications_base_privacy BEFORE INSERT ON workspace_publications
WHEN EXISTS(SELECT 1 FROM workspace_revisions base JOIN workspace_revisions next ON next.id=NEW.revision_id WHERE base.id=NEW.base_revision AND ((base.privacy='local-only' AND next.privacy<>'local-only') OR (base.trusted_only=1 AND next.trusted_only<>1)))
BEGIN SELECT RAISE(ABORT,'new revision cannot widen base privacy'); END;
