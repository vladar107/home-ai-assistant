CREATE TABLE telegram_polling_cursors (
    bot_user_id INTEGER PRIMARY KEY
        CHECK (bot_user_id > 0),
    last_committed_update_id INTEGER NOT NULL
        CHECK (last_committed_update_id >= 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (updated_at >= created_at)
) STRICT;

CREATE TRIGGER telegram_polling_cursors_no_regression
BEFORE UPDATE OF last_committed_update_id ON telegram_polling_cursors
WHEN NEW.last_committed_update_id < OLD.last_committed_update_id
BEGIN
    SELECT RAISE(ABORT, 'Telegram polling cursor cannot move backwards');
END;
