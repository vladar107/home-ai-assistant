-- Set once a chat turn of the session has read Notion page content or NAS file contents, or a tool
-- that can replay them; until /new, no reply of the session is sent to TypeSafe. Sessions from
-- before this column existed may have read such content, so they start as private.
ALTER TABLE conversation_sessions ADD COLUMN private_at TEXT;

UPDATE conversation_sessions SET private_at = created_at;
