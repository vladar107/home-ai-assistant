-- Only each principal's latest position is kept; a new share or update overwrites it.
CREATE TABLE user_locations (
    principal_id TEXT PRIMARY KEY NOT NULL,
    latitude REAL NOT NULL
        CHECK (latitude BETWEEN -90 AND 90),
    longitude REAL NOT NULL
        CHECK (longitude BETWEEN -180 AND 180),
    accuracy REAL NOT NULL
        CHECK (accuracy BETWEEN 0 AND 1500),
    source TEXT NOT NULL
        CHECK (source IN ('pin', 'venue', 'live')),
    venue_title TEXT NOT NULL DEFAULT ''
        CHECK (length(venue_title) <= 200),
    venue_address TEXT NOT NULL DEFAULT ''
        CHECK (length(venue_address) <= 300),
    live_message_id INTEGER
        CHECK (live_message_id IS NULL OR live_message_id > 0),
    live_until TEXT,
    ended_at TEXT,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((source = 'live') = (live_message_id IS NOT NULL)),
    CHECK (source = 'live' OR (live_until IS NULL AND ended_at IS NULL))
) STRICT;

-- The live message whose position the principal last forgot while it was still running; its
-- later edits are ignored. It holds no position, so it is kept rather than pruned.
CREATE TABLE ignored_live_locations (
    principal_id TEXT PRIMARY KEY NOT NULL,
    live_message_id INTEGER NOT NULL
        CHECK (live_message_id > 0),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
