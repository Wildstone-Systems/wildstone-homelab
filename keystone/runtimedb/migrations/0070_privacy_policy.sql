-- Defaults preserve existing processing and disclosure behavior.
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
-- Commit the task handle with the turn, before the later task.session update.
-- A failed secondary link must not orphan effects or their privacy provenance.
CREATE TABLE session_task_sources (
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 PRIMARY KEY(session,task)
);
-- Written memories retain their source history beyond task retention.
CREATE TABLE memory_origin_sessions (
 memory INTEGER NOT NULL REFERENCES butler_memory(seq) ON DELETE CASCADE,
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 PRIMARY KEY(memory,session)
);
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

CREATE TRIGGER privacy_link_first_turn AFTER UPDATE OF session ON butler_tasks WHEN NEW.session IS NOT NULL BEGIN
 INSERT OR IGNORE INTO session_memory_sources(session,memory)
 SELECT NEW.session,memory FROM task_memory_sources WHERE task=NEW.seq;
END;
CREATE TRIGGER privacy_link_current_turn AFTER INSERT ON task_memory_sources BEGIN
 INSERT OR IGNORE INTO session_memory_sources(session,memory)
 SELECT session,NEW.memory FROM butler_tasks WHERE seq=NEW.task AND session IS NOT NULL;
END;

-- Old sessions predate recall tracking. Preserve defaults, but conservatively
-- inherit later restrictions from every record that could have been recalled:
-- same-Person Personal records, this Agent's Agent-local records, and Household
-- records that already existed by the session's last turn. An unattributed old
-- session cannot safely exclude a Person's records. New sessions use exact reads.
INSERT OR IGNORE INTO session_memory_sources(session,memory)
SELECT s.seq,m.seq FROM butler_sessions s JOIN butler_memory m
 ON (s.person_id IS NULL OR m.scope='household' OR m.owner_person_id=s.person_id)
 AND (m.scope<>'agent_local' OR s.butler='' OR m.butler=s.butler)
WHERE julianday(m.created_at)<=julianday(COALESCE(
 (SELECT MAX(t.created_at) FROM butler_session_turns t WHERE t.session=s.seq),s.created_at))
 OR julianday(m.created_at) IS NULL
 OR julianday(COALESCE((SELECT MAX(t.created_at) FROM butler_session_turns t WHERE t.session=s.seq),s.created_at)) IS NULL;

-- Interrupted pre-migration tasks may resume cached reads before they have a
-- session. Give their already-existing ledger the same conservative provenance.
INSERT OR IGNORE INTO task_memory_sources(task,memory)
SELECT t.seq,m.seq FROM butler_tasks t JOIN butler_memory m
 ON (m.scope='household' OR m.owner_person_id IN (t.owner_person_id,t.author_person_id))
 AND (m.scope<>'agent_local' OR t.butler='' OR m.butler=t.butler)
WHERE julianday(m.created_at)<=julianday(COALESCE(
 (SELECT MAX(a.ended_at) FROM butler_task_attempts a JOIN butler_task_steps step ON step.seq=a.step WHERE step.task=t.seq AND a.ended_at<>''),t.updated_at))
 OR julianday(m.created_at) IS NULL;

-- Retain origin through offline adapter queues. Legacy replies conservatively
-- inherit all earlier histories of the bound Person.
CREATE TABLE chat_outbound_sessions (
 outbound INTEGER NOT NULL REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 PRIMARY KEY(outbound,session)
);
INSERT OR IGNORE INTO chat_outbound_sessions(outbound,session)
 SELECT o.seq,s.seq FROM chat_outbound o JOIN chat_bindings b ON b.id=o.binding_id
 JOIN butler_sessions s ON s.person_id=b.person_id

 WHERE o.kind='reply' AND o.delivered_at='' AND (julianday(s.created_at)<=julianday(o.created_at) OR julianday(s.created_at) IS NULL);
CREATE TRIGGER discard_outbound_with_deleted_history BEFORE DELETE ON butler_sessions BEGIN
 DELETE FROM chat_outbound WHERE seq IN (SELECT outbound FROM chat_outbound_sessions WHERE session=OLD.seq);
END;

CREATE TABLE chat_outbound_tasks (
 outbound INTEGER NOT NULL REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 task INTEGER NOT NULL REFERENCES butler_tasks(seq) ON DELETE CASCADE,
 PRIMARY KEY(outbound,task)
);
CREATE TRIGGER discard_outbound_with_deleted_task BEFORE DELETE ON butler_tasks BEGIN
 DELETE FROM chat_outbound WHERE seq IN (SELECT outbound FROM chat_outbound_tasks WHERE task=OLD.seq);
END;

-- Old proactive messages have no reliable generating-task provenance, even
-- when their recipient is known. Withhold that one pre-upgrade queue rather
-- than guessing whose protected context generated it. Keep the rows for audit.
UPDATE notices SET undelivered_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE delivered_at='' AND outbound IN (SELECT seq FROM chat_outbound WHERE kind='push' AND delivered_at='');
UPDATE chat_outbound SET expires_at='2000-01-01T00:00:00Z' WHERE kind='push' AND delivered_at='';

-- First-turn legacy tools can queue before a task/session exists. Preserve
-- their direct memory inputs independently of those later identities.
CREATE TABLE chat_outbound_memories (
 outbound INTEGER NOT NULL REFERENCES chat_outbound(seq) ON DELETE CASCADE,
 memory INTEGER NOT NULL REFERENCES butler_memory(seq) ON DELETE CASCADE,
 PRIMARY KEY(outbound,memory)
);
CREATE TRIGGER discard_outbound_with_deleted_memory BEFORE DELETE ON butler_memory BEGIN
 DELETE FROM chat_outbound WHERE seq IN (SELECT outbound FROM chat_outbound_memories WHERE memory=OLD.seq);
END;

-- Retaining a transcript or memory while pruning its task must not erase
-- either direction of provenance. Copy reads and writes before FK cascades.
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

-- Plans generated from a conversation remain subject to that conversation's
-- live disclosure policy. In-flight or orphaned provenance is Console-only.
CREATE TABLE plan_private_origins (
 plan INTEGER PRIMARY KEY REFERENCES plans(seq) ON DELETE CASCADE
);
CREATE TABLE plan_session_sources (
 plan INTEGER NOT NULL REFERENCES plans(seq) ON DELETE CASCADE,
 session INTEGER NOT NULL REFERENCES butler_sessions(seq) ON DELETE CASCADE,
 PRIMARY KEY(plan,session)
);
INSERT OR IGNORE INTO plan_private_origins(plan) SELECT seq FROM plans WHERE principal='butler';
INSERT OR IGNORE INTO plan_session_sources(plan,session)
 SELECT p.seq,t.session FROM butler_session_turns t JOIN json_each(t.plan_ids) j
 JOIN plans p ON j.value='plan-'||p.seq;
INSERT OR IGNORE INTO plan_private_origins(plan) SELECT plan FROM plan_session_sources;
