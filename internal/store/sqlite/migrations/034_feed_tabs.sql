-- What a tab serves, such as feed:reports for the tab that gets scheduled job reports. A principal
-- has at most one tab for each subject.
ALTER TABLE telegram_tabs ADD COLUMN subject TEXT
    CHECK (subject IS NULL OR length(subject) BETWEEN 1 AND 100);

CREATE UNIQUE INDEX telegram_tabs_subject_idx
    ON telegram_tabs (principal_id, subject)
    WHERE subject IS NOT NULL;

-- When /new was last sent in the tab, which a feed tab counts as its last exchange even when the
-- tab held no conversation to close.
ALTER TABLE telegram_tabs ADD COLUMN reset_at TEXT;

-- A conversation about one job or watch is active in one tab at most.
CREATE UNIQUE INDEX conversation_sessions_one_active_subject_idx
    ON conversation_sessions (principal_id, subject)
    WHERE status = 'active' AND subject IS NOT NULL;

-- The name of the job or watch a request's conversation turned to, set only when the request
-- moved its tab to a conversation about another one.
ALTER TABLE conversation_requests ADD COLUMN subject_label TEXT
    CHECK (subject_label IS NULL OR length(subject_label) BETWEEN 1 AND 200);

-- The tab a report's message landed in, and when it was sent. Messages sent before this migration
-- have no time and are never taken for the latest message of a tab.
ALTER TABLE job_report_messages ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0
    CHECK (thread_id >= 0);
ALTER TABLE job_report_messages ADD COLUMN sent_at TEXT;

CREATE INDEX job_report_messages_thread_idx
    ON job_report_messages (principal_id, thread_id, sent_at);

-- A watch alert as it reached the user, so a reply to it finds its watch and what it said. It names
-- the watch as it was then and has no key to it, so it outlives a deleted watch.
CREATE TABLE watch_alert_messages (
    principal_id TEXT NOT NULL,
    telegram_message_id INTEGER NOT NULL
        CHECK (telegram_message_id > 0),
    thread_id INTEGER NOT NULL DEFAULT 0
        CHECK (thread_id >= 0),
    watch_id TEXT NOT NULL
        CHECK (length(watch_id) = 16),
    watch_label TEXT NOT NULL
        CHECK (length(watch_label) BETWEEN 1 AND 200),
    text TEXT NOT NULL
        CHECK (length(text) BETWEEN 1 AND 2000),
    private INTEGER NOT NULL DEFAULT 0
        CHECK (private IN (0, 1)),
    external INTEGER NOT NULL DEFAULT 0
        CHECK (external IN (0, 1)),
    sent_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, telegram_message_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX watch_alert_messages_thread_idx
    ON watch_alert_messages (principal_id, thread_id, sent_at);

CREATE INDEX watch_alert_messages_watch_idx
    ON watch_alert_messages (principal_id, watch_id, sent_at);
