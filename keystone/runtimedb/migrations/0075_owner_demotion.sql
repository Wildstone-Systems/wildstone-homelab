-- Demotion withdraws pending Owner-bound approval artifacts without
-- offboarding the Person or undoing already-materialized Household grants.
CREATE TRIGGER owner_demotion_revoke_review_tokens
AFTER UPDATE OF role ON butler_members
WHEN OLD.role='owner' AND NEW.role='member'
BEGIN
 UPDATE household_review_tokens
 SET used_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),used_by='demoted'
 WHERE person_id=NEW.person_id AND used_at='';
END;
