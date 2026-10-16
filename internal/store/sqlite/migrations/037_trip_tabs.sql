-- The message in a trip's tab that sums the trip up and is pinned there. The tab itself is the
-- telegram_tabs row whose subject is trip:<id>, so a trip without that row has no tab.
ALTER TABLE trips ADD COLUMN summary_message_id INTEGER
    CHECK (summary_message_id IS NULL OR summary_message_id > 0);

-- Where a job's runs go instead of the feed of their kind, such as trip:<id> for a check-in
-- reminder, which then lands in that trip's tab.
ALTER TABLE scheduled_jobs ADD COLUMN destination TEXT
    CHECK (destination IS NULL OR length(destination) BETWEEN 1 AND 100);

-- The destination of the job a report came from, as it was when the report was sent, so a reply to
-- the report still finds what it was about once the job is gone.
ALTER TABLE job_reports ADD COLUMN destination TEXT
    CHECK (destination IS NULL OR length(destination) BETWEEN 1 AND 100);
