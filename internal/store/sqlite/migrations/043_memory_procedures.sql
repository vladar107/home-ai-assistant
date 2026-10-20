-- A procedure is how a request that needed several steps was done, kept once the user confirmed it.
-- The prompt lists only its title; the model reads the steps with procedure_read, which stamps
-- read_at. How many a principal may keep is the memory service's cap. Its id shares the space of
-- memory entry ids, so an id in the prompt names one thing.
CREATE TABLE memory_procedures (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 80),
    steps TEXT NOT NULL
        CHECK (length(steps) BETWEEN 1 AND 1000),
    source TEXT NOT NULL
        CHECK (source IN ('confirmed', 'learned')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    read_at TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX memory_procedures_principal_idx
    ON memory_procedures (principal_id, created_at);

-- Learning procedures is on for every principal until they turn it off; it needs memory on as well.
ALTER TABLE memory_settings ADD COLUMN learn_procedures INTEGER NOT NULL DEFAULT 1
    CHECK (learn_procedures IN (0, 1));
