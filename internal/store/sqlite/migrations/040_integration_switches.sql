-- Each principal's own choice to turn an integration on or off. A principal without a row has the
-- integration's default, which the integration declares.
CREATE TABLE integration_switches (
    principal_id TEXT NOT NULL,
    integration TEXT NOT NULL
        CHECK (integration = trim(integration) AND length(integration) BETWEEN 1 AND 64),
    enabled INTEGER NOT NULL
        CHECK (enabled IN (0, 1)),
    changed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, integration),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- The switches used to be connection rows read only for whether one existed: for the integrations
-- on by default a row meant the principal turned it off, and for the TypeSafe opt-in that they
-- turned it on.
INSERT INTO integration_switches (principal_id, integration, enabled, changed_at)
SELECT principal_id, integration, CASE integration WHEN 'typesafe' THEN 1 ELSE 0 END, connected_at
FROM integration_connections
WHERE integration IN ('ns', 'bus', 'web', 'grafana', 'homeassistant', 'typesafe');

DELETE FROM integration_connections
WHERE integration IN ('ns', 'bus', 'web', 'grafana', 'homeassistant', 'typesafe');
