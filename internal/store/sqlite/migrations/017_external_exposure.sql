-- Set once a turn of the session has read text written outside the household, such as a web page
-- or an email; until /new, home actions, reminders, and background tasks then need Confirm.
-- Sessions exposed before this column existed may have read such text, so they start as external.
ALTER TABLE conversation_sessions ADD COLUMN external_at TEXT;

UPDATE conversation_sessions SET external_at = exposed_at WHERE exposed_at IS NOT NULL;
