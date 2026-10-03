-- SQLite cannot relax the NOT NULL and length checks on cron in place, so both tables are rebuilt.
-- The runs are rebuilt too: dropping scheduled_jobs while schedule_runs still references it would
-- cascade-delete every run.
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
    UNIQUE (id, principal_id),
    CHECK ((cron IS NULL) <> (run_at IS NULL)),
    CHECK (finished_at IS NULL OR run_at IS NOT NULL),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

INSERT INTO scheduled_jobs_next (id, principal_id, name, job_type, cron, zone, config, enabled, next_run_at, created_at, updated_at)
SELECT id, principal_id, name, job_type, cron, zone, config, enabled, next_run_at, created_at, updated_at
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
    UNIQUE (job_id, scheduled_for),
    FOREIGN KEY (job_id, principal_id) REFERENCES scheduled_jobs_next (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((status = 'ready') = (reply IS NOT NULL)),
    CHECK ((status IN ('delivered', 'skipped', 'failed')) = (finished_at IS NOT NULL))
) STRICT;

INSERT INTO schedule_runs_next SELECT * FROM schedule_runs;

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
