-- The message in an agent watch's tab that sums the watch up and is pinned there. The tab itself is
-- the telegram_tabs row whose subject is watch:<id>, so a watch without that row has no tab.
ALTER TABLE watches ADD COLUMN summary_message_id INTEGER
    CHECK (summary_message_id IS NULL OR summary_message_id > 0);
