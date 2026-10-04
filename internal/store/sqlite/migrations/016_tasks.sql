-- A background task: one long model run on the principal's own subscription, started from a chat
-- turn and reported as a reply to the message that asked for it. The row survives restarts, and
-- the Pi session it names holds the run's progress, so an interrupted task resumes.
CREATE TABLE tasks (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL
        CHECK (chat_id > 0),
    request_message_id INTEGER NOT NULL
        CHECK (request_message_id > 0),
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 80),
    brief TEXT NOT NULL
        CHECK (length(brief) BETWEEN 1 AND 4000),
    session_id TEXT NOT NULL UNIQUE
        CHECK (length(session_id) = 36),
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    partial INTEGER NOT NULL DEFAULT 0
        CHECK (partial IN (0, 1)),
    error_kind TEXT
        CHECK (error_kind IS NULL OR length(error_kind) BETWEEN 1 AND 64),
    result TEXT
        CHECK (result IS NULL OR length(result) >= 1),
    status_message_id INTEGER
        CHECK (status_message_id IS NULL OR status_message_id > 0),
    attempts INTEGER NOT NULL DEFAULT 0
        CHECK (attempts >= 0),
    tool_calls INTEGER NOT NULL DEFAULT 0
        CHECK (tool_calls >= 0),
    web_searches INTEGER NOT NULL DEFAULT 0
        CHECK (web_searches BETWEEN 0 AND tool_calls),
    model_calls INTEGER NOT NULL DEFAULT 0
        CHECK (model_calls >= 0),
    prompt_tokens INTEGER NOT NULL DEFAULT 0
        CHECK (prompt_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0
        CHECK (output_tokens >= 0),
    deadline TEXT,
    noticed INTEGER NOT NULL DEFAULT 0
        CHECK (noticed IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    delivered_at TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((status IN ('done', 'failed', 'cancelled')) = (finished_at IS NOT NULL)),
    CHECK ((status IN ('done', 'failed')) = (result IS NOT NULL)),
    CHECK (status <> 'running' OR (started_at IS NOT NULL AND deadline IS NOT NULL)),
    CHECK (delivered_at IS NULL OR finished_at IS NOT NULL)
) STRICT;

CREATE INDEX tasks_principal_idx
    ON tasks (principal_id, created_at);

CREATE INDEX tasks_open_idx
    ON tasks (principal_id, created_at)
    WHERE status IN ('queued', 'running') OR (finished_at IS NOT NULL AND delivered_at IS NULL);
