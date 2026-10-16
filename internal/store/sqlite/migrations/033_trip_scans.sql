-- When each principal's last successful Trips scan started, which sets how far back the next scan
-- searches mail.
CREATE TABLE trip_scans (
    principal_id TEXT PRIMARY KEY NOT NULL,
    started_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
