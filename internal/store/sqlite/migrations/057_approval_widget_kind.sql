-- A proposal's message may be a widget of one of several kinds, and an edit must send the data its
-- kind reads. The kind is free text so a later kind needs no table rebuild; a proposal flagged as a
-- widget before this is the proposal widget, and a text message has no kind. A proposal's own title
-- names it in a list when its summary starts with something other than the change.
ALTER TABLE approvals ADD COLUMN widget_kind TEXT NOT NULL DEFAULT '';
UPDATE approvals SET widget_kind = 'proposal' WHERE widget = 1;
ALTER TABLE approvals DROP COLUMN widget;
ALTER TABLE approvals ADD COLUMN title TEXT NOT NULL DEFAULT '';
