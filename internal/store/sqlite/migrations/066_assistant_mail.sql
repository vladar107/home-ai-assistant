-- The assistant's own mailbox. seen_mail holds every message id the poller has handled, routed or
-- not, so a message is handled once. mail_routes ties a message to each principal it belongs to:
-- incoming messages by recipient or thread, sent ones to the principal they were sent for, which
-- is how a reply finds its way back. notified_at is null until the principal was told about an
-- incoming message. mail_cursor is the single saved Gmail history position. mail_sends counts the
-- sends a principal started, for the daily limit; send_id is unique so a retried approval is
-- counted once.
CREATE TABLE seen_mail (
    gmail_id TEXT PRIMARY KEY
        CHECK (length(gmail_id) BETWEEN 1 AND 200),
    internal_date TEXT NOT NULL,
    recorded_at TEXT NOT NULL
) STRICT;

CREATE INDEX seen_mail_internal_date ON seen_mail (internal_date);

CREATE TABLE mail_routes (
    gmail_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    thread_id TEXT NOT NULL
        CHECK (length(thread_id) BETWEEN 1 AND 200),
    direction TEXT NOT NULL
        CHECK (direction IN ('in', 'out')),
    internal_date TEXT NOT NULL,
    spam INTEGER NOT NULL DEFAULT 0
        CHECK (spam IN (0, 1)),
    recorded_at TEXT NOT NULL,
    notified_at TEXT,
    PRIMARY KEY (gmail_id, principal_id),
    FOREIGN KEY (gmail_id) REFERENCES seen_mail (gmail_id)
        ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX mail_routes_principal_notified ON mail_routes (principal_id, direction, notified_at, internal_date);
CREATE INDEX mail_routes_principal_date ON mail_routes (principal_id, internal_date);
CREATE INDEX mail_routes_thread ON mail_routes (thread_id, direction);

CREATE TABLE mail_cursor (
    id INTEGER PRIMARY KEY
        CHECK (id = 1),
    history_id TEXT NOT NULL
        CHECK (length(history_id) BETWEEN 1 AND 100),
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE mail_sends (
    send_id TEXT PRIMARY KEY
        CHECK (length(send_id) BETWEEN 1 AND 200),
    principal_id TEXT NOT NULL,
    started_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX mail_sends_principal_started ON mail_sends (principal_id, started_at);
