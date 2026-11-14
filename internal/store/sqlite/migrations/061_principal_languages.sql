-- The interface language each principal chose. A principal without a row has English, so a deploy
-- changes nothing for anyone until they choose.
CREATE TABLE principal_languages (
    principal_id TEXT NOT NULL PRIMARY KEY,
    language TEXT NOT NULL
        CHECK (language IN ('en', 'ru')),
    changed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
