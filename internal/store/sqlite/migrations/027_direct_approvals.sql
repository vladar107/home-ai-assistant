-- Direct actions are approval records made in a clean chat turn without asking. The status checks
-- of a STRICT table cannot be altered in place, so the table is rebuilt; existing records keep
-- every column and become proposals.
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
        CHECK (status IN ('pending', 'confirmed', 'rejected', 'expired', 'executed', 'failed', 'done', 'undone')),
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
    mode TEXT NOT NULL DEFAULT 'proposed'
        CHECK (mode IN ('proposed', 'direct')),
    request_id TEXT
        CHECK (request_id IS NULL OR length(request_id) BETWEEN 1 AND 64),
    -- What the executor needs to reverse a direct action; it can hold a whole job definition, so
    -- it is not capped like outcome.
    undo TEXT
        CHECK (undo IS NULL OR json_valid(undo)),
    undo_until TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    UNIQUE (bundle_id, position),
    CHECK (expires_at > created_at),
    CHECK (started_at IS NULL OR status <> 'pending'),
    CHECK (
        (status IN ('pending', 'confirmed') AND finished_at IS NULL) OR
        (status IN ('rejected', 'expired', 'executed', 'failed', 'done', 'undone') AND finished_at IS NOT NULL)
    ),
    CHECK (mode = 'direct' OR (status NOT IN ('done', 'undone') AND undo IS NULL AND undo_until IS NULL)),
    CHECK (mode = 'proposed' OR (request_id IS NOT NULL AND status NOT IN ('pending', 'rejected', 'expired', 'executed')))
) STRICT;

INSERT INTO approvals_next (
    id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, reason,
    outcome, chat_id, message_id, created_at, expires_at, started_at, finished_at
)
SELECT
    id, bundle_id, position, principal_id, tool, arguments, arguments_hash, summary, status, reason,
    outcome, chat_id, message_id, created_at, expires_at, started_at, finished_at
FROM approvals;

DROP TABLE approvals;

ALTER TABLE approvals_next RENAME TO approvals;

CREATE INDEX approvals_pending_principal_idx
    ON approvals (principal_id)
    WHERE status = 'pending';

CREATE INDEX approvals_direct_request_idx
    ON approvals (principal_id, request_id)
    WHERE mode = 'direct';
