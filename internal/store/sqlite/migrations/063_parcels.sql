-- The parcels a principal is waiting for. A parcel is found by its carrier and number, or, before a
-- number is known, by its sender and order reference. The card message belongs to one chat, so the
-- export leaves it out. A stopped parcel keeps its row until retention deletes it; a parcel no
-- source has updated for 30 days before it was delivered is deleted by the tracker loop, without a
-- tombstone, so news of it follows it afresh.
CREATE TABLE parcels (
    id TEXT NOT NULL PRIMARY KEY
        CHECK (length(id) BETWEEN 8 AND 64),
    principal_id TEXT NOT NULL,
    carrier TEXT NOT NULL
        CHECK (length(carrier) BETWEEN 1 AND 32),
    number TEXT NOT NULL DEFAULT ''
        CHECK (number = '' OR length(number) BETWEEN 8 AND 35),
    order_ref TEXT NOT NULL DEFAULT ''
        CHECK (length(order_ref) <= 64),
    title TEXT NOT NULL DEFAULT ''
        CHECK (length(title) <= 200),
    sender TEXT NOT NULL DEFAULT ''
        CHECK (length(sender) <= 100),
    source TEXT NOT NULL
        CHECK (source IN ('ha', 'mail')),
    source_key TEXT NOT NULL DEFAULT ''
        CHECK (length(source_key) <= 200),
    status TEXT NOT NULL
        CHECK (status IN ('registered', 'in_transit', 'out_for_delivery', 'at_pickup_point', 'delivered', 'problem', 'unknown')),
    window_from TEXT,
    window_to TEXT,
    delivered_at TEXT,
    pickup_point TEXT NOT NULL DEFAULT ''
        CHECK (length(pickup_point) <= 200),
    link TEXT NOT NULL DEFAULT ''
        CHECK (length(link) <= 500),
    card_message_id INTEGER,
    card_kind TEXT
        CHECK (card_kind IS NULL OR card_kind IN ('widget', 'text')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    stopped_at TEXT,
    CHECK (number != '' OR order_ref != '' OR source_key != ''),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE UNIQUE INDEX parcels_number_idx
    ON parcels (principal_id, carrier, number)
    WHERE number != '';

CREATE UNIQUE INDEX parcels_order_idx
    ON parcels (principal_id, sender, order_ref)
    WHERE number = '' AND order_ref != '';

CREATE INDEX parcels_principal_idx
    ON parcels (principal_id, created_at);

-- The status notices already sent, once per parcel and status, so a restart never repeats one.
-- Deleted with the parcel; the parcel's tombstone keeps a source from bringing it back.
CREATE TABLE parcel_notices_sent (
    parcel_id TEXT NOT NULL,
    status TEXT NOT NULL
        CHECK (status IN ('out_for_delivery', 'at_pickup_point', 'delivered', 'problem')),
    principal_id TEXT NOT NULL,
    sent_at TEXT NOT NULL,
    PRIMARY KEY (parcel_id, status),
    FOREIGN KEY (parcel_id) REFERENCES parcels (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- The numbers a principal added to a carrier integration through the assistant, with the
-- integration's domain and config entry, which decide who sees the parcel and where Undo removes it.
CREATE TABLE parcel_added_numbers (
    principal_id TEXT NOT NULL,
    carrier TEXT NOT NULL
        CHECK (length(carrier) BETWEEN 1 AND 32),
    number TEXT NOT NULL
        CHECK (length(number) BETWEEN 8 AND 35),
    domain TEXT NOT NULL
        CHECK (length(domain) BETWEEN 1 AND 64),
    entry_id TEXT NOT NULL
        CHECK (length(entry_id) BETWEEN 1 AND 128),
    added_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, carrier, number),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX parcel_added_numbers_number_idx
    ON parcel_added_numbers (carrier, number);

-- The numbers the assistant proposed to add and what became of each: an explicit reject is
-- declined, and a proposal that expired or was replaced stays proposed and may be offered again.
-- The approval of the latest offer tells a proposal still pending from one that is not; it is not
-- exported.
CREATE TABLE parcel_proposed_numbers (
    principal_id TEXT NOT NULL,
    number TEXT NOT NULL
        CHECK (length(number) BETWEEN 8 AND 35),
    carrier TEXT NOT NULL DEFAULT ''
        CHECK (length(carrier) <= 32),
    outcome TEXT NOT NULL
        CHECK (outcome IN ('proposed', 'added', 'declined')),
    approval_id TEXT NOT NULL DEFAULT ''
        CHECK (length(approval_id) <= 64),
    offers INTEGER NOT NULL DEFAULT 1
        CHECK (offers >= 0),
    proposed_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, number),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- What outlives a deleted or stopped parcel, so a source that still lists it never brings it back
-- or repeats a notice. The key is the carrier and number, or the source's own key. A tombstone is
-- dropped once no source has listed the parcel for a while.
CREATE TABLE parcel_tombstones (
    principal_id TEXT NOT NULL,
    key TEXT NOT NULL
        CHECK (length(key) BETWEEN 1 AND 300),
    created_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, key),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX parcel_tombstones_seen_idx
    ON parcel_tombstones (last_seen_at);

-- How far the mail job has read: epoch seconds of the newest message, and the IDs already read at
-- that second, as the inbox check's overlap does.
CREATE TABLE parcel_mail_cursors (
    principal_id TEXT PRIMARY KEY NOT NULL,
    after_epoch INTEGER NOT NULL
        CHECK (after_epoch >= 0),
    processed TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(processed) AND length(processed) <= 8192),
    updated_at TEXT NOT NULL,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- What the tracker loop has settled for a principal: when its silent baseline last finished, the
-- fingerprint of the sources assigned then, whether the cards were marked as tracking off, and
-- whether the baseline finished before every assigned Home Assistant source had read well, whose
-- reads are then silent too until they have.
CREATE TABLE parcel_tracking (
    principal_id TEXT PRIMARY KEY NOT NULL,
    baselined_at TEXT,
    sources_fingerprint TEXT NOT NULL DEFAULT ''
        CHECK (length(sources_fingerprint) <= 64),
    cards_off INTEGER NOT NULL DEFAULT 0
        CHECK (cards_off IN (0, 1)),
    sources_owed INTEGER NOT NULL DEFAULT 0
        CHECK (sources_owed IN (0, 1)),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- Each principal's own parcel settings.
CREATE TABLE parcel_settings (
    principal_id TEXT PRIMARY KEY NOT NULL,
    postcode TEXT NOT NULL DEFAULT ''
        CHECK (length(postcode) <= 16),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX parcels_source_idx
    ON parcels (principal_id, source, source_key)
    WHERE source_key != '';
