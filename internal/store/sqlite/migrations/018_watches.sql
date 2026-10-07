-- A watch checks one departure, route, or set of stations during a daily window and tells its owner
-- only when something changes. The definition is checked JSON so the kinds can differ; the per-user
-- cap is enforced by the watch service.
CREATE TABLE watches (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('departure', 'disruptions', 'route')),
    config TEXT NOT NULL
        CHECK (json_valid(config)),
    enabled INTEGER NOT NULL DEFAULT 1
        CHECK (enabled IN (0, 1)),
    muted_on TEXT
        CHECK (muted_on IS NULL OR length(muted_on) = 10),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX watches_principal_idx
    ON watches (principal_id, created_at);

-- The last state seen for one day's window of a watch, so a restart neither forgets an alert nor
-- sends it again.
CREATE TABLE watch_states (
    watch_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    occurrence TEXT NOT NULL
        CHECK (length(occurrence) = 10),
    state TEXT,
    alerted INTEGER NOT NULL DEFAULT 0
        CHECK (alerted IN (0, 1)),
    messages INTEGER NOT NULL DEFAULT 0
        CHECK (messages BETWEEN 0 AND 5),
    failures INTEGER NOT NULL DEFAULT 0
        CHECK (failures >= 0),
    failure_noticed INTEGER NOT NULL DEFAULT 0
        CHECK (failure_noticed IN (0, 1)),
    checked_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (watch_id, occurrence),
    FOREIGN KEY (watch_id, principal_id) REFERENCES watches (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- A message a watch owes its owner. It is sent while its window lasts and dropped after it, never
-- delivered late; the sequence caps one window at five messages.
CREATE TABLE watch_alerts (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    watch_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    occurrence TEXT NOT NULL
        CHECK (length(occurrence) = 10),
    sequence INTEGER NOT NULL
        CHECK (sequence BETWEEN 1 AND 5),
    type TEXT NOT NULL
        CHECK (type IN ('alert', 'recovered', 'failure')),
    text TEXT
        CHECK (text IS NULL OR length(text) >= 1),
    status TEXT NOT NULL DEFAULT 'ready'
        CHECK (status IN ('ready', 'delivered', 'dropped', 'failed')),
    reason TEXT
        CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 64),
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE (watch_id, occurrence, sequence),
    FOREIGN KEY (watch_id, principal_id) REFERENCES watches (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((status = 'ready') = (text IS NOT NULL)),
    CHECK ((status = 'ready') = (finished_at IS NULL))
) STRICT;

CREATE INDEX watch_alerts_ready_idx
    ON watch_alerts (status, created_at)
    WHERE status = 'ready';
