CREATE TABLE conversation_sessions (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 36),
    principal_id TEXT NOT NULL,
    runtime_session_id TEXT NOT NULL UNIQUE
        CHECK (length(runtime_session_id) = 36),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'closed')),
    close_reason TEXT
        CHECK (close_reason IS NULL OR close_reason IN ('user_reset')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at TEXT,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (updated_at >= created_at),
    CHECK (
        (status = 'active' AND close_reason IS NULL AND closed_at IS NULL) OR
        (status = 'closed' AND close_reason IS NOT NULL AND closed_at IS NOT NULL)
    )
) STRICT;

CREATE UNIQUE INDEX conversation_sessions_one_active_principal_idx
    ON conversation_sessions (principal_id)
    WHERE status = 'active';

CREATE TABLE conversation_requests (
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
    UNIQUE (bot_user_id, telegram_update_id),
    FOREIGN KEY (session_id, principal_id) REFERENCES conversation_sessions (id, principal_id)
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

CREATE INDEX conversation_requests_principal_state_idx
    ON conversation_requests (principal_id, state, created_at);
