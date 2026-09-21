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
CREATE UNIQUE INDEX household_secret_person_grant ON household_secret_grants(secret_id,person_id,verb) WHERE person_id IS NOT NULL;
CREATE UNIQUE INDEX household_secret_group_grant ON household_secret_grants(secret_id,group_id,verb) WHERE group_id IS NOT NULL;
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
-- Revoking a Person invalidates Personal use even if the account is later
-- readmitted; reactivation does not silently resurrect a prepared capability.
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
CREATE TABLE household_secret_disclosures (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 person_id TEXT NOT NULL REFERENCES butler_members(person_id),
 connection_ref INTEGER NOT NULL,
 recorded_at TEXT NOT NULL
);
