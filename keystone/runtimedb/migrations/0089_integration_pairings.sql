-- 0089: credentials Keystone received by PAIRING with an app it does not
-- run (ADR-W042 A6, #384) — today, the agent key PantryOS hands back when
-- an Owner pairs it.
--
-- Not a household secret. Those belong to a person or the household and
-- are released per call under that person's grants; this is the control
-- plane's own credential for one integration, used for every household
-- call to that app and never revealed through any API. It is sealed with
-- the household secret key all the same (AES-GCM, bound to the capability
-- and endpoint), so a database copy without the key discloses nothing,
-- and the store refuses to open with a missing or wrong key.
--
-- One row per capability: pairing again replaces the row, which is the
-- rotation.
CREATE TABLE integration_pairings (
    capability     TEXT PRIMARY KEY CHECK (capability <> '' AND length(capability) <= 52),
    endpoint       TEXT NOT NULL CHECK (endpoint <> '' AND length(endpoint) <= 512),
    key_ciphertext BLOB NOT NULL,
    paired_at      TEXT NOT NULL,
    paired_by      TEXT NOT NULL DEFAULT ''
);
