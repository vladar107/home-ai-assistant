-- How many of a reply's chunks are already in the chat, so a delivery retried after a failure or a
-- restart goes on with the next chunk instead of sending the earlier ones again.
ALTER TABLE conversation_requests ADD COLUMN delivered_chunks INTEGER NOT NULL DEFAULT 0
    CHECK (delivered_chunks >= 0);

ALTER TABLE tasks ADD COLUMN delivered_chunks INTEGER NOT NULL DEFAULT 0
    CHECK (delivered_chunks >= 0);

ALTER TABLE schedule_runs ADD COLUMN delivered_chunks INTEGER NOT NULL DEFAULT 0
    CHECK (delivered_chunks >= 0);

-- The messages a run's delivered chunks were sent as, so its report names all of them once the run
-- is delivered, however many attempts that took.
ALTER TABLE schedule_runs ADD COLUMN delivered_messages TEXT
    CHECK (delivered_messages IS NULL OR (json_valid(delivered_messages) AND json_type(delivered_messages) = 'array'));
