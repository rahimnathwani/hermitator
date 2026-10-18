-- Scheduled tasks: a prompt an agent runs at a time, or on a cron
-- schedule, in the chat it was set up from.
CREATE TABLE tasks (
    id         TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    account_id TEXT NOT NULL DEFAULT '',
    chat_jid   TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL,
    prompt     TEXT NOT NULL,
    cron       TEXT NOT NULL DEFAULT '', -- empty for a one-off
    next_run   TEXT,                     -- NULL once a one-off has run
    paused     INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL,            -- owner or agent
    last_run   TEXT,
    runs       INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX tasks_due ON tasks (next_run) WHERE paused = 0;

-- Schedules are in the owner's time zone; empty means UTC.
ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
-- Spending caps in API-equivalent dollars; 0 means none.
ALTER TABLE users ADD COLUMN daily_cap REAL NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN monthly_cap REAL NOT NULL DEFAULT 0;
