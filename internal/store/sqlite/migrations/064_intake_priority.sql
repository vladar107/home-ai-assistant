-- How much a record asks of the user: an action to take, news worth knowing, or a parcel update.
-- Existing rows and records made in chat are actions. update_text is the news line of an update or
-- parcel record. The previous image ignores both columns, so a rollback stays possible.
ALTER TABLE intake_records ADD COLUMN priority TEXT NOT NULL DEFAULT 'action'
    CHECK (priority IN ('action', 'update', 'parcel'));
ALTER TABLE intake_records ADD COLUMN update_text TEXT NOT NULL DEFAULT ''
    CHECK (length(update_text) <= 300);
