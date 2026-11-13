-- The proposal widget switch becomes the Widgets switch, which covers every widget. Each choice keeps
-- when it was made; a principal who somehow has both keeps their widgets row, and what is left of
-- the old name goes, so the rename cannot collide with the table's key.
UPDATE OR IGNORE integration_switches
SET integration = 'widgets'
WHERE integration = 'proposal-widget';

DELETE FROM integration_switches
WHERE integration = 'proposal-widget';
