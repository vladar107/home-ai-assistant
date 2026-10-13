-- One row per idea the user noted for developing the assistant later. The cap on open items is
-- enforced by the backlog service, not here.
CREATE TABLE backlog_items (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 120),
    details TEXT NOT NULL
        CHECK (length(details) <= 2000),
    status TEXT NOT NULL
        CHECK (status IN ('open', 'done')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX backlog_items_principal_idx
    ON backlog_items (principal_id, status, created_at);
