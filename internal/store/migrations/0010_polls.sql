-- Polls agents sent, so votes, which carry only hashes of the chosen
-- options, can be read back as the options' names.
CREATE TABLE polls (
    message_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES whatsapp_accounts (id) ON DELETE CASCADE,
    chat_jid   TEXT NOT NULL,
    question   TEXT NOT NULL,
    options    TEXT NOT NULL, -- JSON array
    created_at TEXT NOT NULL
);
