-- The icon of each tab, and the conversation the bot last named a tab after. A tab is named once
-- per conversation, and keeping that here means a restart does not name it again. The session is
-- not a foreign key, so a rebuild of the sessions table never has to carry this table along.
ALTER TABLE telegram_tabs ADD COLUMN icon_custom_emoji_id TEXT;
ALTER TABLE telegram_tabs ADD COLUMN named_session_id TEXT;
