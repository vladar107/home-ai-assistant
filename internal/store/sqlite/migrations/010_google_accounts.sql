CREATE TABLE google_accounts (
    principal_id TEXT NOT NULL,
    account_id TEXT NOT NULL UNIQUE
        CHECK (account_id GLOB '[0-9]*' AND length(account_id) BETWEEN 1 AND 40),
    address TEXT NOT NULL
        CHECK (length(address) BETWEEN 1 AND 128),
    mail_granted INTEGER NOT NULL CHECK (mail_granted IN (0, 1)),
    calendar_granted INTEGER NOT NULL CHECK (calendar_granted IN (0, 1)),
    mail_enabled INTEGER NOT NULL CHECK (mail_enabled IN (0, 1)),
    calendar_enabled INTEGER NOT NULL CHECK (calendar_enabled IN (0, 1)),
    connected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, account_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

DELETE FROM integration_connections WHERE integration = 'gmail';
