-- One row per remembered instruction or fact. The caps on how much a principal may keep are
-- enforced by the memory service, not here, because they are per kind and may change.
CREATE TABLE memory_entries (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('instruction', 'fact')),
    text TEXT NOT NULL
        CHECK (length(text) BETWEEN 1 AND 300),
    source TEXT NOT NULL
        CHECK (source IN ('model', 'confirmed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX memory_entries_principal_idx
    ON memory_entries (principal_id, kind, created_at);

-- A row exists only for a principal who changed the switch; memory is on without one.
CREATE TABLE memory_settings (
    principal_id TEXT PRIMARY KEY NOT NULL,
    enabled INTEGER NOT NULL
        CHECK (enabled IN (0, 1)),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- Set once a turn of the session has read third-party content, which stays in the session's
-- history for as long as the session lives; a memory write in it then needs Confirm.
ALTER TABLE conversation_sessions ADD COLUMN exposed_at TEXT;
