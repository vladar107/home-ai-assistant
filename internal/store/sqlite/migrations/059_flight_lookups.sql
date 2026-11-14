-- The flight status units spent in each quota period, household-wide: one counter for every
-- lookup, one for the lookups users asked for through the tool, and one per user for theirs. A
-- lookup reserves its units here before it is sent, so concurrent turns and the tracking of trips
-- cannot spend past the budget, and the count survives a restart. Left out of the export.
CREATE TABLE flight_lookup_units (
    period TEXT NOT NULL
        CHECK (length(period) = 10),
    scope TEXT NOT NULL
        CHECK (scope IN ('all', 'tool') OR (scope GLOB 'user:?*' AND length(scope) <= 80)),
    units INTEGER NOT NULL
        CHECK (units >= 0),
    PRIMARY KEY (period, scope)
) STRICT;

-- A period the provider's marketplace said was used up, by refusing a request for quota or by
-- reporting fewer units left than one lookup costs, whatever the counters above say.
CREATE TABLE flight_lookup_periods (
    period TEXT PRIMARY KEY NOT NULL
        CHECK (length(period) = 10),
    spent_at TEXT NOT NULL
) STRICT;
