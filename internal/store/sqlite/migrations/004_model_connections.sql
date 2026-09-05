CREATE TABLE model_connections (
    principal_id TEXT PRIMARY KEY NOT NULL,
    provider TEXT NOT NULL
        CHECK (length(provider) BETWEEN 1 AND 64),
    account_id TEXT NOT NULL
        CHECK (length(account_id) BETWEEN 1 AND 128),
    connected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (provider, account_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
