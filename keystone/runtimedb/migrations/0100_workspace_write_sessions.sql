-- ADR-W047 / #478. Merge after 0098 (#490) and 0099 (#475).
-- Bytes remain in confined storage. These rows journal immutable identities
-- BEFORE staging I/O; reservations stay held until later lifecycle cleanup.
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
CREATE TRIGGER workspace_write_sessions_identity BEFORE UPDATE OF id,workspace_id,task_id,artifact_id,path,base_revision,begin_key,request_hash,config_revision,max_bytes,privacy,trusted_only,provenance,created_at ON workspace_write_sessions
BEGIN SELECT RAISE(ABORT,'write session identity is immutable'); END;
CREATE TRIGGER workspace_write_sessions_transition BEFORE UPDATE OF state ON workspace_write_sessions
WHEN NOT ((OLD.state='open' AND NEW.state='sealed') OR (OLD.state='sealed' AND NEW.state IN ('committed','aborted')))
BEGIN SELECT RAISE(ABORT,'invalid write session transition'); END;
CREATE TRIGGER workspace_write_sessions_task BEFORE INSERT ON workspace_write_sessions
WHEN NOT EXISTS(SELECT 1 FROM agent_workspaces w JOIN butlers b ON b.seq=w.agent_id JOIN butler_tasks t ON t.seq=NEW.task_id WHERE w.id=NEW.workspace_id AND t.owner_person_id=w.person_id AND t.butler=b.name)
BEGIN SELECT RAISE(ABORT,'write session task does not own workspace'); END;

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

CREATE TABLE workspace_write_commit_keys (
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 effect_key TEXT NOT NULL CHECK(length(CAST(effect_key AS BLOB)) BETWEEN 1 AND 128),
 session_id TEXT NOT NULL REFERENCES workspace_write_sessions(id),
 PRIMARY KEY(task_id,effect_key)
);
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

CREATE TABLE workspace_write_chunk_keys (
 task_id INTEGER NOT NULL REFERENCES butler_tasks(seq),
 effect_key TEXT NOT NULL CHECK(length(CAST(effect_key AS BLOB)) BETWEEN 1 AND 128),
 session_id TEXT NOT NULL,
 chunk_index INTEGER NOT NULL,
 PRIMARY KEY(task_id,effect_key),
 FOREIGN KEY(session_id,chunk_index) REFERENCES workspace_write_chunks(session_id,chunk_index)
);
CREATE TRIGGER workspace_write_chunk_keys_immutable BEFORE UPDATE ON workspace_write_chunk_keys
BEGIN SELECT RAISE(ABORT,'write chunk key is immutable'); END;
CREATE TRIGGER workspace_write_chunk_keys_scope BEFORE INSERT ON workspace_write_chunk_keys
WHEN NOT EXISTS(SELECT 1 FROM workspace_write_sessions WHERE id=NEW.session_id AND task_id=NEW.task_id)
BEGIN SELECT RAISE(ABORT,'chunk key task does not own write session'); END;
