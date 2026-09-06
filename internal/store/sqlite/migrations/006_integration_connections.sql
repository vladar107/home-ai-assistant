CREATE TABLE integration_connections (
    principal_id TEXT NOT NULL,
    integration TEXT NOT NULL
        CHECK (integration = trim(integration) AND length(integration) BETWEEN 1 AND 64),
    token_kind TEXT NOT NULL
        CHECK (token_kind IN ('internal', 'personal')),
    workspace_id TEXT NOT NULL
        CHECK (length(workspace_id) BETWEEN 1 AND 128),
    workspace_name TEXT NOT NULL
        CHECK (length(workspace_name) <= 200),
    bot_name TEXT NOT NULL
        CHECK (length(bot_name) <= 200),
    writes_enabled INTEGER NOT NULL DEFAULT 0
        CHECK (writes_enabled IN (0, 1)),
    connected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, integration),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
