-- A request whose message the user forwarded from someone else, so its turn treats the text as
-- outside content rather than the user's instruction.
ALTER TABLE conversation_requests ADD COLUMN forwarded INTEGER NOT NULL DEFAULT 0
    CHECK (forwarded IN (0, 1));
