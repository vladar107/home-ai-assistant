ALTER TABLE conversation_requests ADD COLUMN telegram_message_id INTEGER
    CHECK (telegram_message_id IS NULL OR telegram_message_id > 0);
