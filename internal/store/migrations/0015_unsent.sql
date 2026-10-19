-- Texts on their way to a chat. Each is written down before it is sent
-- and deleted once it has gone, so one whose send failed, or that a
-- restart cut off, is sent again.
CREATE TABLE unsent (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    chat_jid   TEXT NOT NULL,
    text       TEXT NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    next_try   TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX unsent_next_try ON unsent (next_try);
