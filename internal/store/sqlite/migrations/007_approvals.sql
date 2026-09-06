CREATE TABLE approvals (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    tool TEXT NOT NULL
        CHECK (tool = trim(tool) AND length(tool) BETWEEN 1 AND 64),
    arguments TEXT NOT NULL
        CHECK (json_valid(arguments)),
    arguments_hash TEXT NOT NULL
        CHECK (length(arguments_hash) = 64),
    summary TEXT NOT NULL
        CHECK (length(summary) BETWEEN 1 AND 4000),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'rejected', 'expired', 'executed', 'failed')),
    reason TEXT
        CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 64),
    outcome TEXT
        CHECK (outcome IS NULL OR length(outcome) BETWEEN 1 AND 1000),
    chat_id INTEGER
        CHECK (chat_id IS NULL OR chat_id > 0),
    message_id INTEGER
        CHECK (message_id IS NULL OR message_id > 0),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    finished_at TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (expires_at > created_at),
    CHECK (
        (status IN ('pending', 'confirmed') AND finished_at IS NULL) OR
        (status IN ('rejected', 'expired', 'executed', 'failed') AND finished_at IS NOT NULL)
    )
) STRICT;

CREATE UNIQUE INDEX approvals_one_pending_principal_idx
    ON approvals (principal_id)
    WHERE status = 'pending';
