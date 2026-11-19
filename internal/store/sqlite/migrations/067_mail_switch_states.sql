-- The Assistant mailbox switch of each principal as the mailbox poller last saw it: whether it was
-- on, since when, and the switch's own change time then. Comparing each poll with this row finds a
-- switch turned on while the poller was stopped or paused, so mail from before is never announced.
CREATE TABLE mail_switch_states (
    principal_id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL
        CHECK (enabled IN (0, 1)),
    on_since TEXT,
    switch_changed_at TEXT,
    observed_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
