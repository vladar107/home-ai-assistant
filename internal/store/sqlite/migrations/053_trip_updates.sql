-- What was already sent to a trip's tab as news about the trip, so the same item is not sent
-- twice. The reference identifies the item: a mail message ID, a page address, or a calendar
-- event ID with what changed. Deleted with the trip.
CREATE TABLE trip_updates_sent (
    trip_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    source TEXT NOT NULL
        CHECK (source IN ('mail', 'web', 'calendar')),
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

CREATE INDEX trip_updates_sent_recent_idx
    ON trip_updates_sent (trip_id, sent_at);

-- One row per trip. The calendar snapshot is the events inside the trip's dates from the last
-- complete list, and missing the IDs of those absent from it. The mail horizon is how far back
-- mail was last read. Deleted with the trip.
CREATE TABLE trip_update_states (
    trip_id TEXT PRIMARY KEY NOT NULL,
    principal_id TEXT NOT NULL,
    calendar_snapshot TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(calendar_snapshot)),
    missing TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(missing)),
    last_attempt_at TEXT,
    last_success_at TEXT,
    mail_read_at TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (trip_id) REFERENCES trips (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
