-- The replies the assistant sent by itself, without asking, in a thread it started for a principal.
-- thread_id is the key, so a thread gets one routine reply ever; the table is kept apart from
-- mail_routes, so pruning routes can never reset it. started_at counts each principal's replies of
-- a day. send_id is the approval record that holds the whole reply, and also its row in mail_sends;
-- there is no foreign key, so removing that record would never free the thread. reported_at is null
-- until a delivered notice showed the reply.
CREATE TABLE mail_routine_replies (
    thread_id TEXT PRIMARY KEY
        CHECK (length(thread_id) BETWEEN 1 AND 200),
    principal_id TEXT NOT NULL,
    send_id TEXT NOT NULL UNIQUE
        CHECK (length(send_id) BETWEEN 1 AND 200),
    started_at TEXT NOT NULL,
    reported_at TEXT,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX mail_routine_replies_principal_started ON mail_routine_replies (principal_id, started_at);
CREATE INDEX mail_routine_replies_unreported ON mail_routine_replies (principal_id, started_at)
    WHERE reported_at IS NULL;
