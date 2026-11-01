-- A job anchored to an event that repeats, such as each matching calendar event or each arrival at
-- a place, has neither a cron expression nor a single instant, so anchor = 'repeating' admits it,
-- occurrence_key names the event occurrence next_run_at points at, and last_occurrence_key the one
-- a repeating job last ran for, so a moved event is never run twice. SQLite cannot change the
-- checks in place, so both tables are rebuilt as in migration 011, the runs too, since dropping
-- scheduled_jobs while schedule_runs still references it would cascade-delete every run.
CREATE TABLE scheduled_jobs_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (length(name) BETWEEN 1 AND 60),
    job_type TEXT NOT NULL
        CHECK (length(job_type) BETWEEN 1 AND 64),
    cron TEXT
        CHECK (cron IS NULL OR length(cron) BETWEEN 9 AND 100),
    run_at TEXT,
    anchor TEXT
        CHECK (anchor IS NULL OR anchor = 'repeating'),
    occurrence_key TEXT
        CHECK (occurrence_key IS NULL OR length(occurrence_key) BETWEEN 1 AND 1500),
    last_occurrence_key TEXT
        CHECK (last_occurrence_key IS NULL OR length(last_occurrence_key) BETWEEN 1 AND 1500),
    zone TEXT NOT NULL
        CHECK (length(zone) BETWEEN 1 AND 64),
    config TEXT NOT NULL
        CHECK (json_valid(config)),
    enabled INTEGER NOT NULL DEFAULT 1
        CHECK (enabled IN (0, 1)),
    next_run_at TEXT NOT NULL,
    finished_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    travel_pause INTEGER NOT NULL DEFAULT 0
        CHECK (travel_pause IN (0, 1)),
    destination TEXT
        CHECK (destination IS NULL OR length(destination) BETWEEN 1 AND 100),
    UNIQUE (id, principal_id),
    CHECK ((cron IS NOT NULL) + (run_at IS NOT NULL) + (anchor IS NOT NULL) = 1),
    CHECK (finished_at IS NULL OR run_at IS NOT NULL OR anchor IS NOT NULL),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO scheduled_jobs_next (id, principal_id, name, job_type, cron, run_at, zone, config, enabled, next_run_at, finished_at, created_at, updated_at, travel_pause, destination)
SELECT id, principal_id, name, job_type, cron, run_at, zone, config, enabled, next_run_at, finished_at, created_at, updated_at, travel_pause, destination
FROM scheduled_jobs;

CREATE TABLE schedule_runs_next (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    job_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    scheduled_for TEXT NOT NULL,
    manual INTEGER NOT NULL DEFAULT 0
        CHECK (manual IN (0, 1)),
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'ready', 'delivered', 'skipped', 'failed')),
    error_kind TEXT
        CHECK (error_kind IS NULL OR length(error_kind) BETWEEN 1 AND 64),
    reply TEXT
        CHECK (reply IS NULL OR length(reply) >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    finished_at TEXT,
    delivered_chunks INTEGER NOT NULL DEFAULT 0
        CHECK (delivered_chunks >= 0),
    delivered_messages TEXT
        CHECK (delivered_messages IS NULL OR (json_valid(delivered_messages) AND json_type(delivered_messages) = 'array')),
    UNIQUE (job_id, scheduled_for),
    FOREIGN KEY (job_id, principal_id) REFERENCES scheduled_jobs_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((status = 'ready') = (reply IS NOT NULL)),
    CHECK ((status IN ('delivered', 'skipped', 'failed')) = (finished_at IS NOT NULL))
) STRICT;

INSERT INTO schedule_runs_next (id, job_id, principal_id, scheduled_for, manual, status, error_kind, reply, created_at, updated_at, finished_at, delivered_chunks, delivered_messages)
SELECT id, job_id, principal_id, scheduled_for, manual, status, error_kind, reply, created_at, updated_at, finished_at, delivered_chunks, delivered_messages
FROM schedule_runs;

DROP TABLE schedule_runs;
DROP TABLE scheduled_jobs;
ALTER TABLE scheduled_jobs_next RENAME TO scheduled_jobs;
ALTER TABLE schedule_runs_next RENAME TO schedule_runs;

CREATE INDEX scheduled_jobs_due_idx
    ON scheduled_jobs (next_run_at)
    WHERE enabled = 1 AND finished_at IS NULL;

CREATE INDEX scheduled_jobs_principal_idx
    ON scheduled_jobs (principal_id, created_at);

CREATE INDEX schedule_runs_open_idx
    ON schedule_runs (status, scheduled_for)
    WHERE status IN ('queued', 'running', 'ready');
