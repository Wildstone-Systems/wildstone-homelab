-- Preserve disclosure provenance independently of scope and audience. A source
-- without resolvable provenance remains usable on trusted UIs, never externally.
CREATE TABLE memory_disclosure_origins (
    memory INTEGER NOT NULL REFERENCES butler_memory(seq),
    connection_ref INTEGER NOT NULL,
    connection_name TEXT NOT NULL,
    reach TEXT NOT NULL, encryption TEXT NOT NULL,
    retention TEXT NOT NULL, surface TEXT NOT NULL
);
CREATE INDEX memory_disclosure_origins_memory ON memory_disclosure_origins(memory);

-- The conversation source ID is the task ID, including on the first turn.
-- Continued tasks also inherit every inbound origin of their input history.
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

INSERT INTO memory_disclosure_origins
SELECT o.memory, o.connection_ref, o.connection_name,
       COALESCE(d.reach,''), COALESCE(d.encryption,''),
       COALESCE(d.retention,''), COALESCE(d.surface,'')
FROM memory_source_connections o LEFT JOIN connection_disclosure d ON d.connection=o.connection_ref;

CREATE TRIGGER memory_capture_disclosure AFTER INSERT ON butler_memory BEGIN
    INSERT INTO memory_disclosure_origins
    SELECT o.memory, o.connection_ref, o.connection_name,
           COALESCE(d.reach,''), COALESCE(d.encryption,''),
           COALESCE(d.retention,''), COALESCE(d.surface,'')
    FROM memory_source_connections o LEFT JOIN connection_disclosure d ON d.connection=o.connection_ref
    WHERE o.memory=NEW.seq;
END;
