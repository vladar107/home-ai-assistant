-- A bundle offered silently, such as the parcel mail offer, is sent without a notification, and so
-- is the sweep's resend of it. A record made before the flag sounds, as it did.
ALTER TABLE approvals ADD COLUMN silent INTEGER NOT NULL DEFAULT 0
    CHECK (silent IN (0, 1));
