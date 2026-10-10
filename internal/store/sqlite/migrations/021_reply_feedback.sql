-- The request each delivered answer came from, so a reaction to it can reach that request. Empty for
-- answers delivered before this column existed.
ALTER TABLE conversation_reply_messages ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

-- The latest reaction of the user to a delivered answer. Only its class and emoji are kept, never
-- the answer text.
CREATE TABLE reply_feedback (
    principal_id TEXT NOT NULL,
    telegram_message_id INTEGER NOT NULL,
    class TEXT NOT NULL
        CHECK (class IN ('positive', 'negative', 'other')),
    emoji TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, telegram_message_id),
    FOREIGN KEY (principal_id, telegram_message_id)
        REFERENCES conversation_reply_messages (principal_id, telegram_message_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- One row for each reconsider turn started by a negative reaction. The key is the request that
-- produced the answer, or message:<id> for an answer without one.
CREATE TABLE reply_reconsiders (
    principal_id TEXT NOT NULL,
    reconsider_key TEXT NOT NULL
        CHECK (reconsider_key <> ''),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, reconsider_key),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
