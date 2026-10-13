-- An agent watch is checked by a model turn, and its alerts may retell private content the turn
-- read, so the kind check admits agent and each alert remembers whether it is private. SQLite
-- cannot widen a check in place, so the three watch tables are rebuilt as in migration 022, the
-- children too, since dropping watches while they still reference it would cascade-delete them.
CREATE TABLE watches_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('departure', 'disruptions', 'route', 'home', 'agent')),
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

INSERT INTO watches_next (id, principal_id, kind, config, enabled, muted_on, created_at, updated_at)
SELECT id, principal_id, kind, config, enabled, muted_on, created_at, updated_at
FROM watches;

CREATE TABLE watch_states_next (
    watch_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    occurrence TEXT NOT NULL
        CHECK (length(occurrence) = 10),
    state TEXT,
    alerted INTEGER NOT NULL DEFAULT 0
        CHECK (alerted IN (0, 1)),
    messages INTEGER NOT NULL DEFAULT 0
        CHECK (messages BETWEEN 0 AND 10),
    failures INTEGER NOT NULL DEFAULT 0
        CHECK (failures >= 0),
    failure_noticed INTEGER NOT NULL DEFAULT 0
        CHECK (failure_noticed IN (0, 1)),
    since TEXT,
    checked_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (watch_id, occurrence),
    FOREIGN KEY (watch_id, principal_id) REFERENCES watches_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO watch_states_next (watch_id, principal_id, occurrence, state, alerted, messages, failures, failure_noticed, since, checked_at, updated_at)
SELECT watch_id, principal_id, occurrence, state, alerted, messages, failures, failure_noticed, since, checked_at, updated_at
FROM watch_states;

CREATE TABLE watch_alerts_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    watch_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    occurrence TEXT NOT NULL
        CHECK (length(occurrence) = 10),
    sequence INTEGER NOT NULL
        CHECK (sequence BETWEEN 1 AND 10),
    type TEXT NOT NULL
        CHECK (type IN ('alert', 'recovered', 'failure')),
    text TEXT
        CHECK (text IS NULL OR length(text) >= 1),
    status TEXT NOT NULL DEFAULT 'ready'
        CHECK (status IN ('ready', 'delivered', 'dropped', 'failed')),
    reason TEXT
        CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 64),
    expires_at TEXT NOT NULL,
    private INTEGER NOT NULL DEFAULT 0
        CHECK (private IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE (watch_id, occurrence, sequence),
    FOREIGN KEY (watch_id, principal_id) REFERENCES watches_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((status = 'ready') = (text IS NOT NULL)),
    CHECK ((status = 'ready') = (finished_at IS NULL))
) STRICT;

INSERT INTO watch_alerts_next (id, watch_id, principal_id, occurrence, sequence, type, text, status, reason, expires_at, created_at, updated_at, finished_at)
SELECT id, watch_id, principal_id, occurrence, sequence, type, text, status, reason, expires_at, created_at, updated_at, finished_at
FROM watch_alerts;

DROP TABLE watch_alerts;
DROP TABLE watch_states;
DROP TABLE watches;
ALTER TABLE watches_next RENAME TO watches;
ALTER TABLE watch_states_next RENAME TO watch_states;
ALTER TABLE watch_alerts_next RENAME TO watch_alerts;

CREATE INDEX watches_principal_idx
    ON watches (principal_id, created_at);

CREATE INDEX watch_alerts_ready_idx
    ON watch_alerts (status, created_at)
    WHERE status = 'ready';
