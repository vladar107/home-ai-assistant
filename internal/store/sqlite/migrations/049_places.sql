-- A place is a point a user saved by name from a position they shared, with the radius that counts
-- as being there. Names are unique per user regardless of case, by name_key, the folded name, among
-- the places not deleted; the per-user cap is enforced by the location service. A deleted place
-- keeps deleted_at while it can be undone, and the location prune removes it after that, so its
-- point is never copied anywhere else to bring it back.
CREATE TABLE places (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) BETWEEN 1 AND 64),
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (length(name) BETWEEN 1 AND 40),
    name_key TEXT NOT NULL
        CHECK (length(name_key) >= 1),
    latitude REAL NOT NULL
        CHECK (latitude BETWEEN -90 AND 90),
    longitude REAL NOT NULL
        CHECK (longitude BETWEEN -180 AND 180),
    radius_m INTEGER NOT NULL
        CHECK (radius_m BETWEEN 50 AND 2000),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX places_principal_name_idx
    ON places (principal_id, name_key) WHERE deleted_at IS NULL;

-- Whether a user is inside or outside each of their places, as the live share that live_message_id
-- names last showed it, with a change seen but not yet lasting enough to count. A place without a
-- row is unknown.
CREATE TABLE place_presence (
    place_id TEXT PRIMARY KEY NOT NULL,
    principal_id TEXT NOT NULL,
    state TEXT NOT NULL
        CHECK (state IN ('inside', 'outside')),
    candidate TEXT
        CHECK (candidate IS NULL OR candidate IN ('inside', 'outside')),
    candidate_since TEXT,
    candidate_fixes INTEGER NOT NULL DEFAULT 0
        CHECK (candidate_fixes >= 0),
    live_message_id INTEGER NOT NULL
        CHECK (live_message_id > 0),
    updated_at TEXT NOT NULL,
    CHECK ((candidate IS NULL) = (candidate_since IS NULL)),
    FOREIGN KEY (place_id, principal_id) REFERENCES places (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX place_presence_principal_idx
    ON place_presence (principal_id);
