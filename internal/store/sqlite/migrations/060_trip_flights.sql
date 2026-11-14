-- The sent log of trip news gains flight status changes. SQLite cannot change a CHECK constraint,
-- so the table is rebuilt with every row it holds.
CREATE TABLE trip_updates_sent_rebuilt (
    trip_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    source TEXT NOT NULL
        CHECK (source IN ('mail', 'web', 'calendar', 'flight')),
    reference TEXT NOT NULL
        CHECK (length(reference) BETWEEN 1 AND 500),
    origin TEXT NOT NULL
        CHECK (length(origin) <= 100),
    title TEXT NOT NULL
        CHECK (length(title) <= 200),
    text TEXT NOT NULL
        CHECK (length(text) <= 600),
    sent_at TEXT NOT NULL,
    PRIMARY KEY (trip_id, source, reference),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (trip_id) REFERENCES trips (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO trip_updates_sent_rebuilt (trip_id, principal_id, source, reference, origin, title, text, sent_at)
SELECT trip_id, principal_id, source, reference, origin, title, text, sent_at FROM trip_updates_sent;

DROP TABLE trip_updates_sent;

ALTER TABLE trip_updates_sent_rebuilt RENAME TO trip_updates_sent;

CREATE INDEX trip_updates_sent_recent_idx
    ON trip_updates_sent (trip_id, sent_at);

-- One row per flight booking of a trip the tracking has looked at: the designator and the
-- booking's stored departure it was made for, and once its leg is found, the leg's origin-local
-- day and airport codes. The lookup is the last stored lookup and what was reported from it, kept
-- at most a week after the leg ends; the progress is which slots were served and which once-only
-- lines were sent. Deleted with the trip; left out of the export.
CREATE TABLE trip_flights (
    trip_id TEXT NOT NULL,
    booking_id TEXT NOT NULL
        CHECK (length(booking_id) BETWEEN 1 AND 64),
    principal_id TEXT NOT NULL,
    designator TEXT NOT NULL
        CHECK (length(designator) BETWEEN 3 AND 8),
    booked_at TEXT NOT NULL,
    day TEXT NOT NULL DEFAULT ''
        CHECK (day = '' OR length(day) = 10),
    origin TEXT NOT NULL DEFAULT ''
        CHECK (length(origin) <= 4),
    destination TEXT NOT NULL DEFAULT ''
        CHECK (length(destination) <= 4),
    lookup TEXT NOT NULL DEFAULT '{}'
        CHECK (json_valid(lookup) AND length(lookup) <= 8192),
    progress TEXT NOT NULL DEFAULT '{}'
        CHECK (json_valid(progress) AND length(progress) <= 4096),
    last_attempt_at TEXT,
    last_success_at TEXT,
    ended_at TEXT,
    PRIMARY KEY (trip_id, booking_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (trip_id) REFERENCES trips (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX trip_flights_ended_idx
    ON trip_flights (ended_at);

-- The end of the last flight tracking pause each principal was told of, so a spent allowance is
-- said once per quota period.
CREATE TABLE trip_flight_pauses (
    principal_id TEXT PRIMARY KEY NOT NULL,
    until TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
