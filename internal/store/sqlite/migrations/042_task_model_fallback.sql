-- Why a task runs on the chat default instead of the task default: the principal's catalog lacked
-- the model, or the cached usage read marked it unavailable. NULL when it did not fall back.
ALTER TABLE tasks ADD COLUMN model_fallback TEXT
    CHECK (model_fallback IS NULL OR model_fallback IN ('catalog', 'usage'));
