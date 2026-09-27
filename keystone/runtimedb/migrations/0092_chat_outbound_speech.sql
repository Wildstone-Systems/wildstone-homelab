-- 0092: a chat reply that is also to be read aloud (ADR-W042 part two).
--
-- speech is the text to speak — the Agent's answer, without the "I
-- heard" line — set only on the reply to a voice message, and only when
-- the deployment reads replies aloud and the Agent may use a voice. ''
-- means text only, which is every message written before this migration.
--
-- The adapter takes it once, after the text is delivered: Keystone
-- clears it as it is read, so a reply is spoken at most once.
ALTER TABLE chat_outbound ADD COLUMN speech TEXT NOT NULL DEFAULT '';
