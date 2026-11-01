-- A turn trace is what one turn did, so the user can ask what a message it delivered was based on:
-- the model and effort that answered, whether outside or private content reached it, and its tool
-- calls in order. It holds no arguments and no results. detail says what code checked for a message
-- no model turn wrote, such as a watch alert raised in code. Traces are kept 30 days.
CREATE TABLE turn_traces (
    principal_id TEXT NOT NULL,
    trace_key TEXT NOT NULL
        CHECK (length(trace_key) BETWEEN 1 AND 200),
    kind TEXT NOT NULL
        CHECK (kind IN ('chat', 'scheduled', 'task', 'check', 'assess', 'code')),
    model TEXT NOT NULL DEFAULT ''
        CHECK (length(model) <= 200),
    effort TEXT NOT NULL DEFAULT ''
        CHECK (length(effort) <= 40),
    detail TEXT NOT NULL DEFAULT ''
        CHECK (length(detail) <= 4000),
    external INTEGER NOT NULL DEFAULT 0
        CHECK (external IN (0, 1)),
    private INTEGER NOT NULL DEFAULT 0
        CHECK (private IN (0, 1)),
    started_at TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, trace_key),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX turn_traces_finished_idx
    ON turn_traces (finished_at);

-- One tool call of a traced turn. label names what the call looked at, such as a page's address or
-- a search's query; it can be private, so it is never logged or exported.
CREATE TABLE turn_trace_calls (
    principal_id TEXT NOT NULL,
    trace_key TEXT NOT NULL,
    sequence INTEGER NOT NULL
        CHECK (sequence >= 1),
    call_id TEXT NOT NULL DEFAULT ''
        CHECK (length(call_id) <= 1000),
    tool TEXT NOT NULL
        CHECK (length(tool) BETWEEN 1 AND 64),
    outcome TEXT NOT NULL
        CHECK (length(outcome) BETWEEN 1 AND 40),
    duration_ms INTEGER NOT NULL DEFAULT 0
        CHECK (duration_ms >= 0),
    result_bytes INTEGER NOT NULL DEFAULT 0
        CHECK (result_bytes >= 0),
    label TEXT NOT NULL DEFAULT ''
        CHECK (length(label) <= 120),
    PRIMARY KEY (principal_id, trace_key, sequence),
    FOREIGN KEY (principal_id, trace_key) REFERENCES turn_traces (principal_id, trace_key)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

-- Every Telegram message a traced turn delivered, so a reply to it finds the trace. It goes with its
-- trace.
CREATE TABLE turn_messages (
    principal_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL,
    telegram_message_id INTEGER NOT NULL
        CHECK (telegram_message_id > 0),
    trace_key TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (principal_id, chat_id, telegram_message_id),
    FOREIGN KEY (principal_id, trace_key) REFERENCES turn_traces (principal_id, trace_key)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;

CREATE INDEX turn_messages_trace_idx
    ON turn_messages (principal_id, trace_key);

-- The check turn of an agent watch keeps a trace, which the alert it raises is linked to on
-- delivery.
ALTER TABLE watch_alerts ADD COLUMN trace_key TEXT NOT NULL DEFAULT '';
