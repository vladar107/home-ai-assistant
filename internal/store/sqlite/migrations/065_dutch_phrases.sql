-- Every Dutch phrase of the day a principal was sent, one per local date, so no phrase is ever sent
-- to them twice. normalized is the phrase in lowercase without punctuation, which the second key
-- holds unique. Turning the phrase off keeps the rows.
CREATE TABLE dutch_phrases (
    principal_id TEXT NOT NULL,
    local_date TEXT NOT NULL
        CHECK (length(local_date) = 10),
    phrase TEXT NOT NULL
        CHECK (length(phrase) BETWEEN 1 AND 120),
    normalized TEXT NOT NULL
        CHECK (length(normalized) BETWEEN 1 AND 240),
    usage TEXT NOT NULL
        CHECK (length(usage) BETWEEN 1 AND 200),
    translation TEXT NOT NULL
        CHECK (length(translation) BETWEEN 1 AND 200),
    usage_translation TEXT NOT NULL
        CHECK (length(usage_translation) BETWEEN 1 AND 200),
    note TEXT NOT NULL DEFAULT ''
        CHECK (length(note) <= 200),
    created_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, local_date),
    UNIQUE (principal_id, normalized),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
