-- ADR-W047 / #482 part 1: logical namespace only. No filesystem effect or grant.
CREATE TABLE workspace_directories (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 workspace_id TEXT NOT NULL REFERENCES agent_workspaces(id),
 path TEXT NOT NULL CHECK(path<>''),
 revision TEXT NOT NULL UNIQUE CHECK(length(revision)=32 AND revision NOT GLOB '*[^0-9a-f]*'),
 state TEXT NOT NULL DEFAULT 'committed' CHECK(state IN ('committed','removed')),
 created_at TEXT NOT NULL,
 UNIQUE(id,workspace_id)
);
CREATE UNIQUE INDEX workspace_directories_live_path ON workspace_directories(workspace_id,path) WHERE state='committed';
CREATE TRIGGER workspace_directories_identity BEFORE UPDATE OF id,workspace_id,path,revision,created_at ON workspace_directories
BEGIN SELECT RAISE(ABORT,'directory identity is immutable'); END;
CREATE TRIGGER workspace_directories_transition BEFORE UPDATE OF state ON workspace_directories
WHEN OLD.state<>'committed' OR NEW.state<>'removed'
BEGIN SELECT RAISE(ABORT,'invalid directory transition'); END;
CREATE TRIGGER workspace_directories_no_delete BEFORE DELETE ON workspace_directories
BEGIN SELECT RAISE(ABORT,'directory history requires retention coordination'); END;
-- Install before backfill: ambiguous legacy file/ancestor collisions refuse
-- migration transactionally rather than inventing a conflicting directory.
CREATE TRIGGER workspace_directory_collision BEFORE INSERT ON workspace_directories
WHEN NEW.state<>'committed' OR EXISTS(SELECT 1 FROM workspace_artifacts a WHERE a.workspace_id=NEW.workspace_id AND a.state<>'trashed' AND
 (a.path=NEW.path OR substr(NEW.path,1,length(a.path)+1)=a.path||'/')) OR
 EXISTS(SELECT 1 FROM workspace_write_sessions s WHERE s.workspace_id=NEW.workspace_id AND s.state IN ('open','sealed') AND
 (s.path=NEW.path OR substr(NEW.path,1,length(s.path)+1)=s.path||'/'))
BEGIN SELECT RAISE(ABORT,'workspace namespace conflict'); END;

-- Populate implicit ancestors of existing live files and unfinished writes.
-- Tokens are server-minted independent random identities, never path hashes.
INSERT INTO workspace_directories(id,workspace_id,path,revision,created_at)
SELECT lower(hex(randomblob(16))),workspace_id,parent,lower(hex(randomblob(16))),strftime('%Y-%m-%dT%H:%M:%fZ','now')
FROM (
 WITH RECURSIVE parents(workspace_id,parent,tail) AS (
  SELECT workspace_id,'',path FROM workspace_artifacts WHERE state<>'trashed'
  UNION SELECT workspace_id,'',path FROM workspace_write_sessions WHERE state IN ('open','sealed')
  UNION ALL SELECT workspace_id,CASE WHEN parent='' THEN substr(tail,1,instr(tail,'/')-1) ELSE parent||'/'||substr(tail,1,instr(tail,'/')-1) END,substr(tail,instr(tail,'/')+1)
  FROM parents WHERE instr(tail,'/')>0
 ) SELECT DISTINCT workspace_id,parent FROM parents WHERE parent<>''
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

-- SQLite writer transactions serialize these checks with every path admission.
-- substr comparisons are literal (LIKE would interpret % and _ in filenames).
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

-- Existing staged writers may create nested paths. Give their implicit parents
-- durable identities in the same transaction, without changing write internals.
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
