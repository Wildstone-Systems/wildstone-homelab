-- ADR-W047 / #475. Reserved after #490's 0098; do not land out of order.
-- Reads retain exact revision identity before disclosure. Foreign keys pin
-- source bytes until the consuming task/session is explicitly removed.
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
-- A revision retains the session it was derived from even if its task later
-- points elsewhere. Source session deletion must not silently erase privacy.
CREATE TABLE workspace_revision_sessions (
 revision TEXT NOT NULL REFERENCES workspace_revisions(id),
 session INTEGER NOT NULL REFERENCES butler_sessions(seq),
 PRIMARY KEY(revision,session)
);
INSERT INTO workspace_revision_sessions(revision,session)
 SELECT r.id,t.session FROM workspace_revisions r JOIN butler_tasks t ON t.seq=r.task_id
 WHERE t.session IS NOT NULL;
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

-- Keep the existing memory graph intact. contentPrivacy traverses its union
-- with this graph, using UNION to terminate conservative lineage cycles.
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

-- Pin the exact known inputs at revision creation. Live graph traversal still
-- catches later tightening, but task/session relinking cannot discard history.
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

INSERT OR IGNORE INTO workspace_revision_memories(revision,memory)
 WITH RECURSIVE lineage(revision,object) AS (
 SELECT id,'t:'||task_id FROM workspace_revisions UNION
 SELECT l.revision,e.parent FROM lineage l JOIN (
 SELECT child,parent FROM privacy_edges UNION SELECT child,parent FROM workspace_privacy_edges
 ) e ON e.child=l.object
 ) SELECT l.revision,m.seq FROM lineage l JOIN butler_memory m ON l.object='m:'||m.seq;
INSERT OR IGNORE INTO workspace_revision_inputs(revision,source_revision)
 WITH RECURSIVE lineage(revision,object) AS (
 SELECT id,'t:'||task_id FROM workspace_revisions UNION
 SELECT l.revision,e.parent FROM lineage l JOIN (
 SELECT child,parent FROM privacy_edges UNION SELECT child,parent FROM workspace_privacy_edges
 ) e ON e.child=l.object
 ) SELECT l.revision,r.id FROM lineage l JOIN workspace_revisions r ON l.object='wr:'||r.id WHERE r.id<>l.revision;

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
