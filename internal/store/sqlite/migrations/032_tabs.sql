-- SQLite cannot widen the close_reason check in place, so conversation_sessions is rebuilt as
-- migration 015 did. Everything that references it, directly or through the reply messages, is
-- rebuilt with it, because dropping the old table while they still reference it would
-- cascade-delete their rows; renaming the new parents rewrites their references.
--
-- tab_gone closes the conversation of a tab Telegram no longer accepts sends to, and tab_closed
-- the conversation of a tab the user closed. subject names what a conversation is about, such as a
-- scheduled job, for the conversations that have one.
CREATE TABLE conversation_sessions_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 36),
    principal_id TEXT NOT NULL,
    runtime_session_id TEXT NOT NULL UNIQUE
        CHECK (length(runtime_session_id) = 36),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'closed')),
    close_reason TEXT
        CHECK (close_reason IS NULL OR close_reason IN ('user_reset', 'switched', 'tab_gone', 'tab_closed')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at TEXT,
    exposed_at TEXT,
    external_at TEXT,
    private_at TEXT,
    thread_id INTEGER NOT NULL DEFAULT 0
        CHECK (thread_id >= 0),
    subject TEXT
        CHECK (subject IS NULL OR length(subject) BETWEEN 1 AND 100),
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (updated_at >= created_at),
    CHECK (
        (status = 'active' AND close_reason IS NULL AND closed_at IS NULL) OR
        (status = 'closed' AND close_reason IS NOT NULL AND closed_at IS NOT NULL)
    )
) STRICT;

INSERT INTO conversation_sessions_next (
    id, principal_id, runtime_session_id, status, close_reason, created_at, updated_at, closed_at,
    exposed_at, external_at, private_at, thread_id
)
SELECT id, principal_id, runtime_session_id, status, close_reason, created_at, updated_at, closed_at,
       exposed_at, external_at, private_at, thread_id
FROM conversation_sessions;

CREATE TABLE conversation_requests_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 36),
    bot_user_id INTEGER NOT NULL
        CHECK (bot_user_id > 0),
    telegram_update_id INTEGER NOT NULL
        CHECK (telegram_update_id >= 0),
    principal_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL
        CHECK (chat_id > 0),
    session_id TEXT NOT NULL,
    message TEXT NOT NULL
        CHECK (length(message) <= 4096),
    state TEXT NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'running', 'ready', 'delivering', 'delivered', 'cancelled')),
    response_text TEXT
        CHECK (response_text IS NULL OR length(response_text) <= 262144),
    error_kind TEXT
        CHECK (error_kind IS NULL OR error_kind IN ('timeout', 'runtime', 'tool_provider')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at TEXT,
    finished_at TEXT,
    telegram_message_id INTEGER
        CHECK (telegram_message_id IS NULL OR telegram_message_id > 0),
    resumed_from TEXT
        CHECK (resumed_from IS NULL OR length(resumed_from) = 24),
    reply_to_message_id INTEGER
        CHECK (reply_to_message_id IS NULL OR reply_to_message_id > 0),
    thread_id INTEGER NOT NULL DEFAULT 0
        CHECK (thread_id >= 0),
    UNIQUE (bot_user_id, telegram_update_id),
    FOREIGN KEY (session_id, principal_id) REFERENCES conversation_sessions_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (
        (state = 'queued' AND started_at IS NULL AND finished_at IS NULL AND response_text IS NULL) OR
        (state = 'running' AND started_at IS NOT NULL AND finished_at IS NULL AND response_text IS NULL) OR
        (state = 'ready' AND started_at IS NOT NULL AND finished_at IS NULL AND response_text IS NOT NULL) OR
        (state = 'delivering' AND started_at IS NOT NULL AND finished_at IS NULL AND response_text IS NOT NULL) OR
        (state IN ('delivered', 'cancelled') AND finished_at IS NOT NULL AND response_text IS NULL AND message = '')
    )
) STRICT;

INSERT INTO conversation_requests_next (
    id, bot_user_id, telegram_update_id, principal_id, chat_id, session_id, message, state,
    response_text, error_kind, created_at, started_at, finished_at, telegram_message_id, resumed_from,
    reply_to_message_id, thread_id
)
SELECT id, bot_user_id, telegram_update_id, principal_id, chat_id, session_id, message, state,
       response_text, error_kind, created_at, started_at, finished_at, telegram_message_id, resumed_from,
       reply_to_message_id, thread_id
FROM conversation_requests;

CREATE TABLE conversation_attachments_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    position INTEGER NOT NULL
        CHECK (position >= 0),
    kind TEXT NOT NULL
        CHECK (kind IN ('image', 'file')),
    file_name TEXT NOT NULL
        CHECK (length(file_name) BETWEEN 1 AND 200),
    mime_type TEXT NOT NULL
        CHECK (length(mime_type) BETWEEN 1 AND 100),
    size_bytes INTEGER NOT NULL
        CHECK (size_bytes > 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (request_id, position),
    FOREIGN KEY (session_id, principal_id) REFERENCES conversation_sessions_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (request_id) REFERENCES conversation_requests_next (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO conversation_attachments_next (
    id, principal_id, session_id, request_id, position, kind, file_name, mime_type, size_bytes, created_at
)
SELECT id, principal_id, session_id, request_id, position, kind, file_name, mime_type, size_bytes, created_at
FROM conversation_attachments;

CREATE TABLE conversation_reply_messages_next (
    principal_id TEXT NOT NULL,
    telegram_message_id INTEGER NOT NULL
        CHECK (telegram_message_id > 0),
    session_id TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    request_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (principal_id, telegram_message_id),
    FOREIGN KEY (session_id, principal_id) REFERENCES conversation_sessions_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO conversation_reply_messages_next (principal_id, telegram_message_id, session_id, created_at, request_id)
SELECT principal_id, telegram_message_id, session_id, created_at, request_id
FROM conversation_reply_messages;

CREATE TABLE reply_feedback_next (
    principal_id TEXT NOT NULL,
    telegram_message_id INTEGER NOT NULL,
    class TEXT NOT NULL
        CHECK (class IN ('positive', 'negative', 'other')),
    emoji TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, telegram_message_id),
    FOREIGN KEY (principal_id, telegram_message_id)
        REFERENCES conversation_reply_messages_next (principal_id, telegram_message_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO reply_feedback_next (principal_id, telegram_message_id, class, emoji, updated_at)
SELECT principal_id, telegram_message_id, class, emoji, updated_at
FROM reply_feedback;

DROP TABLE reply_feedback;
DROP TABLE conversation_reply_messages;
DROP TABLE conversation_attachments;
DROP TABLE conversation_requests;
DROP TABLE conversation_sessions;
ALTER TABLE conversation_sessions_next RENAME TO conversation_sessions;
ALTER TABLE conversation_requests_next RENAME TO conversation_requests;
ALTER TABLE conversation_attachments_next RENAME TO conversation_attachments;
ALTER TABLE conversation_reply_messages_next RENAME TO conversation_reply_messages;
ALTER TABLE reply_feedback_next RENAME TO reply_feedback;

CREATE UNIQUE INDEX conversation_sessions_one_active_lane_idx
    ON conversation_sessions (principal_id, thread_id)
    WHERE status = 'active';

CREATE INDEX conversation_requests_lane_state_idx
    ON conversation_requests (principal_id, thread_id, state, created_at);

CREATE INDEX conversation_requests_session_idx
    ON conversation_requests (session_id, created_at);

CREATE INDEX conversation_attachments_session_idx
    ON conversation_attachments (principal_id, session_id, created_at, id);

-- The tabs the bot knows of in each principal's chat, with their names. Sessions stay the record of
-- which conversation a tab holds. named_by is who chose the name: the bot, the user, or fixed for
-- a tab whose name never follows its topic. confirmed_at is the first time a send into the tab, or
-- a message of the user's from it, succeeded.
CREATE TABLE telegram_tabs (
    principal_id TEXT NOT NULL,
    thread_id INTEGER NOT NULL
        CHECK (thread_id > 0),
    name TEXT NOT NULL
        CHECK (length(name) BETWEEN 1 AND 128),
    named_by TEXT NOT NULL
        CHECK (named_by IN ('bot', 'user', 'fixed')),
    confirmed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, thread_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (updated_at >= created_at)
) STRICT;
