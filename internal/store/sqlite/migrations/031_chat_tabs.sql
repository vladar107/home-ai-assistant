-- The Telegram tab each conversation, request, proposal, and task belongs to. Zero is the chat's
-- main view, and the only tab while the bot has tabs off. Each tab holds its own active
-- conversation, and a session keeps the tab it was last active in.
ALTER TABLE conversation_sessions ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0
    CHECK (thread_id >= 0);
ALTER TABLE conversation_requests ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0
    CHECK (thread_id >= 0);
ALTER TABLE approvals ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0
    CHECK (thread_id >= 0);
ALTER TABLE tasks ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0
    CHECK (thread_id >= 0);

DROP INDEX conversation_sessions_one_active_principal_idx;
CREATE UNIQUE INDEX conversation_sessions_one_active_lane_idx
    ON conversation_sessions (principal_id, thread_id)
    WHERE status = 'active';

DROP INDEX conversation_requests_principal_state_idx;
CREATE INDEX conversation_requests_lane_state_idx
    ON conversation_requests (principal_id, thread_id, state, created_at);
