-- A route is a user's saved journey: a name, the weekdays it applies to, an optional arrival target,
-- and its legs as checked JSON. Names are unique per user regardless of case; the per-user cap is
-- enforced by the routes service.
CREATE TABLE routes (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (length(name) BETWEEN 1 AND 60),
    days TEXT NOT NULL
        CHECK (json_valid(days) AND json_type(days) = 'array' AND json_array_length(days) BETWEEN 1 AND 7),
    arrive_by TEXT
        CHECK (arrive_by IS NULL OR length(arrive_by) = 5),
    legs TEXT NOT NULL
        CHECK (json_valid(legs) AND json_type(legs) = 'array' AND json_array_length(legs) BETWEEN 1 AND 8),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX routes_principal_name_idx
    ON routes (principal_id, name COLLATE NOCASE);
