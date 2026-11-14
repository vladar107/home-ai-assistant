-- A record says whether the intent check warned about it, so the warning is no longer found by its
-- English prefix and a proposal made in one language reads correctly after the user switches to the
-- other. The outcome the user reads is kept beside the English one the model reads. Records stored
-- before this have no user outcome and show the English one, as they were shown.
ALTER TABLE approvals ADD COLUMN warned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE approvals ADD COLUMN user_outcome TEXT NOT NULL DEFAULT '';
UPDATE approvals SET warned = 1
WHERE instr(summary, '⚠️ This does not look like something you asked for. Check it carefully before confirming.') = 1
   OR instr(summary, '⚠️ This does not look like something you asked for. Check it, and press Undo if it is wrong.') = 1;
