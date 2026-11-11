-- A proposal's message may be a Telegram widget, which has no text to edit: the flag lets its edits go
-- the widget's way from the start. A record made before the flag reads as a text message.
ALTER TABLE approvals ADD COLUMN widget INTEGER NOT NULL DEFAULT 0
    CHECK (widget IN (0, 1));
