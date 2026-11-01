-- A system of the user's own that may message them through POST /api/events. Only the SHA-256 of
-- its token is kept; the token itself is shown once.
CREATE TABLE event_sources (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (name = trim(name) AND length(name) BETWEEN 1 AND 40),
    mode TEXT NOT NULL
        CHECK (mode IN ('forward', 'assess')),
    token_hash TEXT NOT NULL UNIQUE
        CHECK (length(token_hash) = 64),
    enabled INTEGER NOT NULL DEFAULT 1
        CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    last_used_at TEXT,
    rotated_at TEXT,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX event_sources_principal_idx
    ON event_sources (principal_id, created_at);

-- An event a source posted, kept 30 days so the rate limit, the dedupe, and conversation can read
-- it. outcome is pending until it is delivered, suppressed by its assessment, or given up on.
-- assessment keeps what an assess source's turn decided, and assessed_message what it worded, so
-- a retried delivery never runs the turn again.
CREATE TABLE inbound_events (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    received_at TEXT NOT NULL,
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 200),
    text TEXT NOT NULL
        CHECK (length(text) <= 4000),
    severity TEXT NOT NULL
        CHECK (severity IN ('info', 'warning', 'critical')),
    dedupe_key TEXT
        CHECK (dedupe_key IS NULL OR length(dedupe_key) BETWEEN 1 AND 100),
    outcome TEXT NOT NULL
        CHECK (outcome IN ('pending', 'delivered', 'suppressed', 'failed', 'dropped', 'duplicate')),
    assessment TEXT
        CHECK (assessment IS NULL OR assessment IN ('notify', 'quiet', 'failed')),
    assessed_message TEXT
        CHECK (assessed_message IS NULL OR length(assessed_message) BETWEEN 1 AND 1000),
    FOREIGN KEY (source_id, principal_id) REFERENCES event_sources (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX inbound_events_source_idx
    ON inbound_events (source_id, received_at);

CREATE INDEX inbound_events_principal_idx
    ON inbound_events (principal_id, received_at);

CREATE INDEX inbound_events_pending_idx
    ON inbound_events (received_at)
    WHERE outcome = 'pending';

-- A message that showed a source's token, deleted once it is due, whatever restarts come between.
-- It has no key to its source or principal on purpose: deleting either must not cancel the
-- deletion of a message that shows a token.
CREATE TABLE event_token_messages (
    principal_id TEXT NOT NULL
        CHECK (length(principal_id) BETWEEN 1 AND 128),
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL
        CHECK (message_id > 0),
    delete_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, chat_id, message_id)
) STRICT;

CREATE INDEX event_token_messages_due_idx
    ON event_token_messages (delete_at);
