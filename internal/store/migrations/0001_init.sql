-- Timestamps are TEXT in a fixed-width UTC format (timeFormat in store.go),
-- which sorts correctly and keeps the schema portable to Postgres.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    max_agents    INTEGER NOT NULL DEFAULT 5,
    suspended     INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL
);

-- actor_id is the human who logged in. effective_id is whose resources the
-- session acts on; it differs from actor_id only while an admin is
-- impersonating someone.
CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    actor_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    effective_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL
);

CREATE TABLE user_secrets (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, name)
);

CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    user_id          TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    image            TEXT NOT NULL,
    harness          TEXT NOT NULL CHECK (harness IN ('claude', 'pi')),
    model            TEXT NOT NULL DEFAULT '',
    pi_provider      TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'stopped',
    session_id       TEXT NOT NULL DEFAULT '',
    idle_timeout_sec INTEGER NOT NULL DEFAULT 300,
    memory_mb        INTEGER NOT NULL DEFAULT 4096,
    ip               TEXT NOT NULL UNIQUE,
    last_active_at   TEXT,
    created_at       TEXT NOT NULL,
    UNIQUE (user_id, name)
);

CREATE TABLE whatsapp_accounts (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    jid               TEXT UNIQUE,
    phone             TEXT NOT NULL DEFAULT '',
    push_name         TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,
    fallback_agent_id TEXT REFERENCES agents (id) ON DELETE SET NULL,
    created_at        TEXT NOT NULL
);

CREATE TABLE routes (
    account_id      TEXT NOT NULL REFERENCES whatsapp_accounts (id) ON DELETE CASCADE,
    chat_jid        TEXT NOT NULL,
    chat_name       TEXT NOT NULL DEFAULT '',
    agent_id        TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    require_mention INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    PRIMARY KEY (account_id, chat_jid)
);

CREATE INDEX routes_agent ON routes (agent_id);

CREATE TABLE link_codes (
    code       TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);

CREATE TABLE turns (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    account_id  TEXT NOT NULL DEFAULT '',
    chat_jid    TEXT NOT NULL DEFAULT '',
    harness     TEXT NOT NULL,
    status      TEXT NOT NULL,
    input       TEXT NOT NULL,
    output      TEXT NOT NULL DEFAULT '',
    tools       INTEGER NOT NULL DEFAULT 0,
    wake        TEXT NOT NULL DEFAULT '',
    wake_ms     INTEGER NOT NULL DEFAULT 0,
    cost_usd    REAL NOT NULL DEFAULT 0,
    started_at  TEXT NOT NULL,
    finished_at TEXT
);

CREATE INDEX turns_agent ON turns (agent_id, started_at);

CREATE TABLE audit_log (
    id           TEXT PRIMARY KEY,
    actor_id     TEXT NOT NULL,
    effective_id TEXT NOT NULL,
    action       TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);

CREATE INDEX audit_log_created ON audit_log (created_at);
