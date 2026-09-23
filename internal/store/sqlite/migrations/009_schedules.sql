CREATE TABLE scheduled_jobs (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL
        CHECK (length(name) BETWEEN 1 AND 60),
    job_type TEXT NOT NULL
        CHECK (length(job_type) BETWEEN 1 AND 64),
    cron TEXT NOT NULL
        CHECK (length(cron) BETWEEN 9 AND 100),
    zone TEXT NOT NULL
        CHECK (length(zone) BETWEEN 1 AND 64),
    config TEXT NOT NULL
        CHECK (json_valid(config)),
    enabled INTEGER NOT NULL DEFAULT 1
        CHECK (enabled IN (0, 1)),
    next_run_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX scheduled_jobs_due_idx
    ON scheduled_jobs (next_run_at)
    WHERE enabled = 1;

CREATE INDEX scheduled_jobs_principal_idx
    ON scheduled_jobs (principal_id, created_at);

CREATE TABLE schedule_runs (
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
    FOREIGN KEY (job_id, principal_id) REFERENCES scheduled_jobs (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK ((status = 'ready') = (reply IS NOT NULL)),
    CHECK ((status IN ('delivered', 'skipped', 'failed')) = (finished_at IS NOT NULL))
) STRICT;

CREATE INDEX schedule_runs_open_idx
    ON schedule_runs (status, scheduled_for)
    WHERE status IN ('queued', 'running', 'ready');
