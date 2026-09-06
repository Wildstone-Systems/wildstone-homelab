-- Butler provider config as runtime state (ADR-W017 A9).
--
-- Provider, model and endpoint were startup flags, so changing a model
-- meant editing a systemd environment file and restarting the control
-- plane — which drops agent sessions, re-runs the observe cycle, and
-- proposes a drift repair that reports healthy resources as absent
-- (F75/F76). Changing which model answers a question should not make a
-- household look broken.
--
-- ONE ROW, enforced. This is the deployment's Butler configuration, not
-- a collection of them: a second row would be a second answer to a
-- question with one answer, and the bug it caused would be whichever row
-- happened to be read first.
--
-- No API key column, deliberately and permanently. ADR-W017 A4 keeps
-- credentials in the process environment, and A9 does not relax it:
-- what became editable is which provider to use and which model to ask,
-- never the secret used to reach it. A column here would be a place for
-- one to be written by accident.
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
