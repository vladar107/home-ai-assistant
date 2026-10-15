-- Whether a recurring job or a watch stays silent while its owner is on a trip. The user's own
-- pause (enabled) is a separate switch and is never changed by a trip.
ALTER TABLE scheduled_jobs ADD COLUMN travel_pause INTEGER NOT NULL DEFAULT 0
    CHECK (travel_pause IN (0, 1));
ALTER TABLE watches ADD COLUMN travel_pause INTEGER NOT NULL DEFAULT 0
    CHECK (travel_pause IN (0, 1));
