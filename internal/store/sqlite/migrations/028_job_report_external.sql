-- Whether the run behind a report read outside content, so a chat turn told of the report is
-- marked as having read it only then. Existing reports stay external, since their runs are unknown.
ALTER TABLE job_reports ADD COLUMN external INTEGER NOT NULL DEFAULT 1
    CHECK (external IN (0, 1));
