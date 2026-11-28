-- A loose end is a deadline nobody asked the user about, such as a free-cancellation window or a
-- renewal, or a promise the user made, tracked until it is done, dropped, closed, or past. due_at
-- is an instant; nudge_on and nudged_on are local dates, YYYY-MM-DD, with nudged_on empty until
-- the first nudge. The unique key makes a second detection of the same message, event, or chat
-- item a no-op; a chat item's source_ref is its own ID. A row no longer open is kept as a
-- tombstone, so a later scan never brings it back, until the loose ends service prunes it. The
-- caps on new and open items are enforced by the service.
CREATE TABLE loose_ends (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('cancel_window', 'return_window', 'renewal', 'trial_end', 'expiry', 'promise')),
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 120),
    counterpart TEXT NOT NULL DEFAULT ''
        CHECK (length(counterpart) <= 120),
    due_at TEXT NOT NULL,
    due_all_day INTEGER NOT NULL DEFAULT 0
        CHECK (due_all_day IN (0, 1)),
    nudge_on TEXT NOT NULL
        CHECK (length(nudge_on) = 10),
    nudged_on TEXT NOT NULL DEFAULT ''
        CHECK (length(nudged_on) IN (0, 10)),
    source TEXT NOT NULL
        CHECK (source IN ('mail', 'sent', 'calendar', 'chat')),
    account TEXT NOT NULL DEFAULT ''
        CHECK (length(account) <= 320),
    source_ref TEXT NOT NULL DEFAULT ''
        CHECK (length(source_ref) <= 300),
    thread_ref TEXT NOT NULL DEFAULT ''
        CHECK (length(thread_ref) <= 300),
    outside INTEGER NOT NULL DEFAULT 0
        CHECK (outside IN (0, 1)),
    status TEXT NOT NULL
        CHECK (status IN ('open', 'done', 'dropped', 'expired', 'closed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (principal_id, source, account, source_ref, kind),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX loose_ends_principal_idx
    ON loose_ends (principal_id, status, due_at);

CREATE INDEX loose_ends_prune_idx
    ON loose_ends (due_at)
    WHERE status <> 'open';

-- How far each principal's loose ends scan has read: mail up to read_until, the backfill window
-- still to read, from backfill_before back to backfill_until (both empty once it is drained), how
-- many scans in a row made no progress on new mail and on the backfill, a digest of the calendar
-- events last seen, while the events of a changed calendar are offered over several scans, the
-- key of the last one offered, and a JSON array of keys of the events the model was offered and
-- reported nothing for, each a hash of the event and its update time.
CREATE TABLE loose_end_scans (
    principal_id TEXT PRIMARY KEY NOT NULL,
    read_until TEXT NOT NULL,
    backfill_until TEXT,
    backfill_before TEXT,
    stalls INTEGER NOT NULL DEFAULT 0
        CHECK (stalls >= 0),
    backfill_stalls INTEGER NOT NULL DEFAULT 0
        CHECK (backfill_stalls >= 0),
    calendar_digest TEXT NOT NULL DEFAULT ''
        CHECK (length(calendar_digest) <= 128),
    calendar_cursor TEXT NOT NULL DEFAULT ''
        CHECK (length(calendar_cursor) <= 2048),
    calendar_declined TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(calendar_declined) AND json_type(calendar_declined) = 'array' AND length(calendar_declined) <= 16384),
    CHECK ((backfill_until IS NULL) = (backfill_before IS NULL)),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
