-- ADR-W034 A5 / ADR-W033 B3-B5: calendar actions are granted per action,
-- on top of do_household_action (both must allow).
--
-- A governed (default-deny) Agent that holds do_household_action reached
-- every calendar action through it. Without this, the per-action check
-- would silently narrow it to nothing. So each such Agent is explicitly
-- given the seven calendar actions declared today and the Owner narrows
-- from there. Explicit names are static (W033 B3): an action declared
-- later is not granted by this migration.
--
-- Ungoverned Agents are untouched, and so are Agents that already hold
-- an action (the unique live index makes this idempotent).
INSERT INTO butler_tool_assignments (butler, tool, assigned_at, assigned_by)
SELECT a.butler, t.tool, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), 'migration:0104'
FROM butler_tool_assignments a
JOIN butler_tool_governance g ON g.butler = a.butler
CROSS JOIN (
    SELECT 'calendar.how_to_add' AS tool
    UNION ALL SELECT 'calendar.get_agenda'
    UNION ALL SELECT 'calendar.search_events'
    UNION ALL SELECT 'calendar.check_availability'
    UNION ALL SELECT 'calendar.create_event'
    UNION ALL SELECT 'calendar.update_event'
    UNION ALL SELECT 'calendar.delete_event'
) t
WHERE a.tool = 'do_household_action' AND a.revoked_at = ''
  AND NOT EXISTS (
    SELECT 1 FROM butler_tool_assignments x
    WHERE x.butler = a.butler AND x.tool = t.tool AND x.revoked_at = '');
