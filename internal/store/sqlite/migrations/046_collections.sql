-- A collection is a typed list a user keeps through the assistant, such as things lent out. Its
-- fields are checked JSON, each with a key that record values are stored under, so a rename never
-- rewrites records. next_key is the number the next added field's key takes; it only grows, so a
-- key whose values were dropped is never given out again. Names are unique per user regardless of
-- case; the per-user caps are enforced by the collection service.
CREATE TABLE collections (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (length(name) BETWEEN 1 AND 60),
    description TEXT NOT NULL
        CHECK (length(description) <= 300),
    fields TEXT NOT NULL
        CHECK (json_valid(fields) AND json_type(fields) = 'array' AND json_array_length(fields) BETWEEN 1 AND 20),
    next_key INTEGER NOT NULL
        CHECK (next_key >= 2),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX collections_principal_name_idx
    ON collections (principal_id, name COLLATE NOCASE);

-- A record's values are a JSON object keyed by field key.
CREATE TABLE collection_records (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    field_values TEXT NOT NULL
        CHECK (json_valid(field_values) AND json_type(field_values) = 'object'),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (collection_id, principal_id) REFERENCES collections (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX collection_records_collection_idx
    ON collection_records (principal_id, collection_id, created_at);
