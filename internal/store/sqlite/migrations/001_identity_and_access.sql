CREATE TABLE principals (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (id = trim(id) AND length(id) BETWEEN 1 AND 128),
    display_name TEXT NOT NULL
        CHECK (length(trim(display_name)) BETWEEN 1 AND 200),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (updated_at >= created_at)
) STRICT;

CREATE TABLE telegram_identities (
    telegram_user_id INTEGER PRIMARY KEY
        CHECK (telegram_user_id > 0),
    username TEXT
        CHECK (username IS NULL OR (username = trim(username) AND length(username) BETWEEN 1 AND 64)),
    display_name TEXT
        CHECK (display_name IS NULL OR length(trim(display_name)) BETWEEN 1 AND 200),
    first_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (last_seen_at >= first_seen_at)
) STRICT;

CREATE TABLE telegram_allowlist (
    telegram_user_id INTEGER PRIMARY KEY,
    principal_id TEXT NOT NULL,
    allowed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    revoked_at TEXT,
    FOREIGN KEY (telegram_user_id) REFERENCES telegram_identities (telegram_user_id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (revoked_at IS NULL OR revoked_at >= allowed_at)
) STRICT;

CREATE INDEX telegram_allowlist_principal_idx
    ON telegram_allowlist (principal_id);

CREATE TABLE capability_grants (
    principal_id TEXT NOT NULL,
    capability_name TEXT NOT NULL
        CHECK (capability_name = trim(capability_name) AND length(capability_name) BETWEEN 1 AND 200),
    capability_version INTEGER NOT NULL
        CHECK (capability_version > 0),
    resource_scope TEXT NOT NULL
        CHECK (resource_scope = trim(resource_scope) AND length(resource_scope) BETWEEN 1 AND 500),
    credential_ref TEXT
        CHECK (
            credential_ref IS NULL OR
            (credential_ref = trim(credential_ref) AND length(credential_ref) BETWEEN 1 AND 500)
        ),
    automation_risk_ceiling INTEGER NOT NULL
        CHECK (automation_risk_ceiling BETWEEN 1 AND 4),
    conditions TEXT NOT NULL DEFAULT '{}'
        CHECK (json_valid(conditions) AND json_type(conditions) = 'object'),
    granted_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at TEXT,
    revoked_at TEXT,
    PRIMARY KEY (principal_id, capability_name, capability_version, resource_scope),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE,
    CHECK (expires_at IS NULL OR expires_at > granted_at),
    CHECK (revoked_at IS NULL OR revoked_at >= granted_at)
) STRICT;

CREATE INDEX capability_grants_active_idx
    ON capability_grants (
        principal_id,
        capability_name,
        capability_version,
        resource_scope,
        expires_at
    )
    WHERE revoked_at IS NULL;
