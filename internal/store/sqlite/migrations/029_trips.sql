-- One row per trip the user is taking or may take, and one per booking that carries a check-in
-- time. The caps on unfinished trips and on bookings per trip are enforced by the trip service.
-- A booking's fingerprint identifies it across mail, calendar, and chat and is empty when it has
-- no reference to identify it by.
CREATE TABLE trips (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 320),
    destination TEXT NOT NULL
        CHECK (length(destination) BETWEEN 1 AND 320),
    starts_at TEXT NOT NULL,
    ends_at TEXT NOT NULL,
    zone TEXT NOT NULL
        CHECK (length(zone) BETWEEN 1 AND 64),
    status TEXT NOT NULL
        CHECK (status IN ('suggested', 'planned', 'dismissed', 'cancelled')),
    source TEXT NOT NULL
        CHECK (source IN ('chat', 'mail', 'calendar')),
    started_notified_at TEXT,
    ended_notified_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX trips_principal_idx
    ON trips (principal_id, status, starts_at);

CREATE INDEX trips_active_idx
    ON trips (principal_id, starts_at, ends_at)
    WHERE status = 'planned';

CREATE TABLE trip_bookings (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 8),
    principal_id TEXT NOT NULL,
    trip_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('flight', 'hotel', 'train', 'car', 'other')),
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 320),
    reference TEXT NOT NULL
        CHECK (length(reference) <= 160),
    starts_at TEXT NOT NULL,
    check_in_at TEXT,
    check_in_source TEXT
        CHECK (check_in_source IN ('stated', 'looked_up', 'default')),
    fingerprint TEXT NOT NULL
        CHECK (length(fingerprint) IN (0, 64)),
    position INTEGER NOT NULL,
    CHECK ((check_in_at IS NULL) = (check_in_source IS NULL)),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (trip_id) REFERENCES trips (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX trip_bookings_fingerprint_idx
    ON trip_bookings (principal_id, fingerprint)
    WHERE fingerprint <> '';

CREATE INDEX trip_bookings_trip_idx
    ON trip_bookings (trip_id, position);
