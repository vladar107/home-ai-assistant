-- A report a scheduled job delivered, kept so later conversation can refer to it. Its ID is the
-- run's; it names the job as it was then and has no key to it, so it outlives a deleted or
-- finished job. noticed is set once a chat turn has been told of it.
CREATE TABLE job_reports (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    job_id TEXT NOT NULL
        CHECK (length(job_id) = 16),
    job_name TEXT NOT NULL
        CHECK (length(job_name) BETWEEN 1 AND 60),
    job_type TEXT NOT NULL
        CHECK (length(job_type) BETWEEN 1 AND 64),
    text TEXT NOT NULL
        CHECK (length(text) >= 1),
    private INTEGER NOT NULL DEFAULT 0
        CHECK (private IN (0, 1)),
    noticed INTEGER NOT NULL DEFAULT 0
        CHECK (noticed IN (0, 1)),
    delivered_at TEXT NOT NULL,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX job_reports_principal_idx
    ON job_reports (principal_id, delivered_at);

-- The Telegram messages a report was sent as, so a reply to one finds it.
CREATE TABLE job_report_messages (
    principal_id TEXT NOT NULL,
    telegram_message_id INTEGER NOT NULL
        CHECK (telegram_message_id > 0),
    report_id TEXT NOT NULL,
    PRIMARY KEY (principal_id, telegram_message_id),
    FOREIGN KEY (report_id, principal_id) REFERENCES job_reports (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX job_report_messages_report_idx
    ON job_report_messages (report_id);

-- The message a request replied to, so its turn can be given the job report it answers.
ALTER TABLE conversation_requests ADD COLUMN reply_to_message_id INTEGER
    CHECK (reply_to_message_id IS NULL OR reply_to_message_id > 0);
