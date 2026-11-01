-- A project is a goal a user works toward through the assistant, such as finding a dentist, with a
-- checklist of items and links to related things. Its tab is the telegram_tabs row whose subject is
-- project:<id>, and summary_message_id is the message pinned there. The caps on active projects,
-- items, and links are enforced by the project service.
CREATE TABLE projects (
    id TEXT PRIMARY KEY NOT NULL
        CHECK (length(id) = 16),
    principal_id TEXT NOT NULL,
    title TEXT NOT NULL
        CHECK (length(title) BETWEEN 1 AND 100),
    goal TEXT NOT NULL
        CHECK (length(goal) <= 500),
    status TEXT NOT NULL
        CHECK (status IN ('active', 'paused', 'done', 'abandoned')),
    summary_message_id INTEGER
        CHECK (summary_message_id IS NULL OR summary_message_id > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (id, principal_id),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX projects_principal_idx
    ON projects (principal_id, status, created_at);

-- An item's due date is a local date, YYYY-MM-DD.
CREATE TABLE project_items (
    principal_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    id TEXT NOT NULL
        CHECK (length(id) = 8),
    position INTEGER NOT NULL,
    text TEXT NOT NULL
        CHECK (length(text) BETWEEN 1 AND 300),
    done_at TEXT,
    due_on TEXT
        CHECK (due_on IS NULL OR length(due_on) = 10),
    note TEXT NOT NULL
        CHECK (length(note) <= 500),
    PRIMARY KEY (principal_id, project_id, id),
    FOREIGN KEY (project_id, principal_id) REFERENCES projects (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX project_items_position_idx
    ON project_items (principal_id, project_id, position);

CREATE TABLE project_links (
    principal_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    kind TEXT NOT NULL
        CHECK (kind IN ('notion', 'nas', 'collection', 'intake', 'url')),
    reference TEXT NOT NULL
        CHECK (length(reference) BETWEEN 1 AND 500),
    label TEXT NOT NULL
        CHECK (length(label) <= 100),
    PRIMARY KEY (principal_id, project_id, position),
    UNIQUE (principal_id, project_id, kind, reference),
    FOREIGN KEY (project_id, principal_id) REFERENCES projects (id, principal_id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
