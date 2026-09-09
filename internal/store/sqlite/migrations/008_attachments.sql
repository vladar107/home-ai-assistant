CREATE TABLE conversation_attachments (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    position INTEGER NOT NULL
        CHECK (position >= 0),
    kind TEXT NOT NULL
        CHECK (kind IN ('image', 'file')),
    file_name TEXT NOT NULL
        CHECK (length(file_name) BETWEEN 1 AND 200),
    mime_type TEXT NOT NULL
        CHECK (length(mime_type) BETWEEN 1 AND 100),
    size_bytes INTEGER NOT NULL
        CHECK (size_bytes > 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (request_id, position),
    FOREIGN KEY (session_id, principal_id) REFERENCES conversation_sessions (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (request_id) REFERENCES conversation_requests (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX conversation_attachments_session_idx
    ON conversation_attachments (principal_id, session_id, created_at, id);
