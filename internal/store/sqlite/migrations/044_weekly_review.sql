-- How each principal's tool calls ended, counted per UTC day, for the weekly review. Only the tool's
-- name and the outcome word are kept, never arguments or results; rows older than 30 days are
-- pruned.
CREATE TABLE tool_outcome_counts (
    principal_id TEXT NOT NULL,
    day TEXT NOT NULL
        CHECK (length(day) = 10),
    tool TEXT NOT NULL
        CHECK (length(tool) BETWEEN 1 AND 64),
    outcome TEXT NOT NULL
        CHECK (length(outcome) BETWEEN 1 AND 32),
    count INTEGER NOT NULL
        CHECK (count > 0),
    PRIMARY KEY (principal_id, day, tool, outcome),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX tool_outcome_counts_day_idx
    ON tool_outcome_counts (day);

-- A bundle offered outside a chat turn, such as the weekly review, carries the heading it is shown
-- under. Confirming one continues no conversation, because no chat turn made it.
ALTER TABLE approvals ADD COLUMN heading TEXT
    CHECK (heading IS NULL OR length(heading) BETWEEN 1 AND 64);
