-- An intake record keeps the facts the model read from one item of incoming content, such as a
-- letter's dates, amounts, and references, so a later question is answered without the original.
-- It never holds the content itself. source_ref names the item: an attachment ID, a mail account
-- and message ID, or the record's own ID for a forwarded message, so the inbox check skips a
-- message it already handled. facts is {"dates":[],"amounts":[],"references":[]}.
-- pending_actions holds the follow-ups an inbox check proposed that no final outcome has taken
-- yet, each with the approval it was last offered as.
CREATE TABLE intake_records (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    source TEXT NOT NULL
        CHECK (source IN ('photo', 'file', 'forward', 'mail')),
    source_ref TEXT NOT NULL
        CHECK (length(source_ref) BETWEEN 1 AND 300),
    kind TEXT NOT NULL
        CHECK (length(kind) BETWEEN 1 AND 40),
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 120),
    sender TEXT NOT NULL
        CHECK (length(sender) <= 120),
    facts TEXT NOT NULL
        CHECK (json_valid(facts) AND json_type(facts) = 'object'),
    action_required TEXT NOT NULL
        CHECK (length(action_required) <= 300),
    pending_actions TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(pending_actions) AND json_type(pending_actions) = 'array'),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (principal_id, source, source_ref),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX intake_records_principal_idx
    ON intake_records (principal_id, created_at);

-- When each principal's inbox check last read mail up to, so the next one reads only what came
-- after, and how many checks in a row made no progress on that window, so a window that keeps
-- failing is skipped rather than retried for good.
CREATE TABLE intake_scans (
    principal_id TEXT PRIMARY KEY NOT NULL,
    read_until TEXT NOT NULL,
    stalls INTEGER NOT NULL DEFAULT 0
        CHECK (stalls >= 0),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
