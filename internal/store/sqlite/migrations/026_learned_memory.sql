-- An instruction proposed by the turn a 👎 starts, and confirmed, is kept as learned. SQLite cannot
-- widen a check in place, so the entries are rebuilt as in migration 025. Nothing references them.
CREATE TABLE memory_entries_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('instruction', 'fact')),
    text TEXT NOT NULL
        CHECK (length(text) BETWEEN 1 AND 300),
    source TEXT NOT NULL
        CHECK (source IN ('model', 'confirmed', 'learned')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO memory_entries_next (id, principal_id, kind, text, source, created_at, updated_at)
SELECT id, principal_id, kind, text, source, created_at, updated_at
FROM memory_entries
ORDER BY rowid;

DROP TABLE memory_entries;
ALTER TABLE memory_entries_next RENAME TO memory_entries;

CREATE INDEX memory_entries_principal_idx
    ON memory_entries (principal_id, kind, created_at);

-- Learning from 👎 is on for every principal until they turn it off; it needs memory on as well.
ALTER TABLE memory_settings ADD COLUMN learn_from_feedback INTEGER NOT NULL DEFAULT 1
    CHECK (learn_from_feedback IN (0, 1));
