-- Tabs the bot knew of before it named tabs by topic keep the names they have; only tabs opened
-- from now on are named after their conversation.
UPDATE telegram_tabs SET named_by = 'user' WHERE named_by = 'bot';
