-- 0089: a chat Connection's retention says what its transport does.
--
-- 0064 classified every Connection -- the ones it backfilled and, through
-- its connection_disclosure_default trigger, every one added since -- with
-- retention 'unknown', on the principle that nothing should assert what a
-- vendor keeps when nobody has checked. Every policy question asked of
-- 'unknown' takes the strict branch, so no chat Connection could carry a
-- remembered fact, or (before verified continuation) its own
-- conversation from one message to the next.
--
-- For the services below the principle is too strict to be useful and
-- the fact is not in doubt: Telegram (cloud chats, which every bot
-- conversation is), Discord, Slack and WhatsApp's business API all keep
-- message history on the provider's servers. 'provider' says exactly
-- that, and it is the stricter of the two resolved values. Anything else
-- stays 'unknown' until an operator says otherwise.
--
-- Reach, encryption and surface are 0064's: external, transport (these
-- are HTTPS APIs, which is true and is no claim about end-to-end), and
-- external messaging.

DROP TRIGGER connection_disclosure_default;

CREATE TRIGGER connection_disclosure_default
AFTER INSERT ON butler_addresses
BEGIN
    INSERT INTO connection_disclosure
        (connection, reach, encryption, retention, surface, accepts_fallback, set_at, set_by)
    VALUES (
        NEW.seq, 'external', 'transport',
        CASE NEW.transport
            WHEN 'telegram' THEN 'provider' WHEN 'discord' THEN 'provider'
            WHEN 'slack' THEN 'provider' WHEN 'whatsapp' THEN 'provider'
            ELSE 'unknown' END,
        'external_messaging', 0, NEW.created_at, NEW.created_by
    );
END;

-- The rows 0064 and its trigger already wrote for those transports,
-- corrected -- only while each still holds the default it was given: the
-- 0064 backfill's, or the trigger's (stamped with the address's own
-- creation time).
UPDATE connection_disclosure
   SET retention = 'provider', set_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), set_by = 'migration:0089'
 WHERE retention = 'unknown' AND reach = 'external' AND encryption = 'transport'
   AND surface = 'external_messaging'
   AND connection IN (SELECT a.seq FROM butler_addresses a
                       WHERE a.transport IN ('telegram', 'discord', 'slack', 'whatsapp')
                         AND (connection_disclosure.set_by = 'migration:0064'
                              OR connection_disclosure.set_at = a.created_at));
