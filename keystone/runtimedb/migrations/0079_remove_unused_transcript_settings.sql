-- Slice 24: migration 0038 was applied history, but neither setting ever
-- acquired a production reader or writer. Retire its unused opt-in and
-- retention columns forward so fresh and upgraded databases agree.
-- This does not delete conversation history or explicit memory records.
ALTER TABLE butler_runtime_settings DROP COLUMN index_conversations;
ALTER TABLE butler_runtime_settings DROP COLUMN transcript_retention_days;
