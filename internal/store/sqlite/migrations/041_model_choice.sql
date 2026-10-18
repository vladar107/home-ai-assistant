-- A lane's own choice of model and effort for chat turns: the main view (thread 0) or one tab.
-- A lane without a row runs on the configured chat default. /new and closing the tab delete the row.
CREATE TABLE conversation_lane_models (
    principal_id TEXT NOT NULL,
    thread_id INTEGER NOT NULL
        CHECK (thread_id >= 0),
    model TEXT NOT NULL
        CHECK (model = trim(model) AND length(model) BETWEEN 1 AND 200),
    effort TEXT NOT NULL
        CHECK (effort IN ('off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, thread_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- The model and effort a background task copies when it is created. Rows from before this
-- migration have none.
ALTER TABLE tasks ADD COLUMN model TEXT
    CHECK (model IS NULL OR (model = trim(model) AND length(model) BETWEEN 1 AND 200));
ALTER TABLE tasks ADD COLUMN effort TEXT
    CHECK (effort IS NULL OR effort IN ('off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'));
