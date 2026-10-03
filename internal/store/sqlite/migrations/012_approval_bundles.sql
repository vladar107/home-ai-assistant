-- SQLite cannot add a checked column that existing rows must fill from another column, so the
-- table is rebuilt; each existing record becomes a bundle of its own. A record confirmed before
-- the upgrade may have been executing, so it counts as started.
CREATE TABLE approvals_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    bundle_id TEXT NOT NULL
        CHECK (length(bundle_id) = 16),
    position INTEGER NOT NULL
        CHECK (position >= 0),
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
    started_at TEXT,
    finished_at TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    UNIQUE (bundle_id, position),
    CHECK (expires_at > created_at),
    CHECK (started_at IS NULL OR status <> 'pending'),
    CHECK (
        (status IN ('pending', 'confirmed') AND finished_at IS NULL) OR
        (status IN ('rejected', 'expired', 'executed', 'failed') AND finished_at IS NOT NULL)
    )
) STRICT;

INSERT INTO approvals_next (
    id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, reason,
    outcome, chat_id, message_id, created_at, expires_at, started_at, finished_at
)
SELECT
    id, id, 0, principal_id, tool, arguments, arguments_hash, summary, status, reason,
    outcome, chat_id, message_id, created_at, expires_at,
    CASE WHEN status = 'confirmed' THEN created_at END, finished_at
FROM approvals;

DROP TABLE approvals;

ALTER TABLE approvals_next RENAME TO approvals;

CREATE INDEX approvals_pending_principal_idx
    ON approvals (principal_id)
    WHERE status = 'pending';
