-- 0004: node inventory (#30). Until now a node's existence was implicit:
-- the gateway issued it a certificate at join and held a session while it
-- was connected, but nothing durable recorded that the node exists. So
-- nothing could answer "what nodes do I have, and are they connected?" —
-- every node route required a name the caller already knew.
--
-- One row per node that has ever completed a join. The gateway writes
-- through a hook (it keeps no database dependency of its own); live
-- connection state is NOT stored here — it comes from the session
-- registry at read time, because a row that says "connected" would be a
-- lie the moment the process restarts.
CREATE TABLE nodes (
    node_id         TEXT PRIMARY KEY,
    first_joined_at TEXT NOT NULL,             -- RFC3339Nano UTC
    last_seen_at    TEXT NOT NULL,             -- last session open or close
    agent_version   TEXT NOT NULL DEFAULT '',  -- from the agent's Hello
    protocol_minor  INTEGER NOT NULL DEFAULT 0
);
