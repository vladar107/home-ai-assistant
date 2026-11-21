-- Each principal's vault of the personal values the model receives as placeholders (plan 0170).
-- A value keeps one random suffix for as long as its row lives, so a token in a resumed session
-- still resolves; rows unseen for longer than conversations are kept are pruned by last_seen.
CREATE TABLE placeholder_values (
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('email', 'phone', 'address')),
    value TEXT NOT NULL
        CHECK (length(value) BETWEEN 1 AND 2000),
    suffix TEXT NOT NULL
        CHECK (length(suffix) = 6 AND suffix NOT GLOB '*[^a-z2-7]*'),
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX placeholder_values_principal_value ON placeholder_values (principal_id, kind, value);
CREATE UNIQUE INDEX placeholder_values_principal_suffix ON placeholder_values (principal_id, suffix);
CREATE INDEX placeholder_values_last_seen ON placeholder_values (last_seen);
