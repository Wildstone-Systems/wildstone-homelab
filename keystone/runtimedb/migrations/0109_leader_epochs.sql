-- Safety contract #180 §2.1 (W050 slice 3b): Keystone issues every leader
-- epoch. One row per epoch, never updated or deleted, so the history of
-- who led when survives and an epoch is never issued twice.
CREATE TABLE leader_epochs (
    epoch       INTEGER PRIMARY KEY CHECK (epoch > 0),
    leader      TEXT NOT NULL CHECK (leader <> ''),
    issued_at   TEXT NOT NULL
);
