-- hermitator: foreign keys off
--
-- Agents may run Antigravity (agy) as well as Claude Code and pi. SQLite
-- cannot change a CHECK constraint, so the table is rebuilt the way
-- SQLite's own documentation does it (lang_altertable.html, "Making Other
-- Kinds Of Table Schema Changes"): with foreign keys off, which the line
-- above asks the migrator for, since they cannot be turned off inside the
-- transaction. With them on, dropping the old table would delete every
-- route, turn, task and conversation through ON DELETE CASCADE, and a
-- rename would point those tables' references at the renamed table. The
-- migrator checks the foreign keys before it commits.

CREATE TABLE agents_new (
    id               TEXT PRIMARY KEY,
    user_id          TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    image            TEXT NOT NULL,
    harness          TEXT NOT NULL CHECK (harness IN ('claude', 'pi', 'agy')),
    model            TEXT NOT NULL DEFAULT '',
    pi_provider      TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'stopped',
    idle_timeout_sec INTEGER NOT NULL DEFAULT 300,
    memory_mb        INTEGER NOT NULL DEFAULT 4096,
    ip               TEXT NOT NULL UNIQUE,
    last_active_at   TEXT,
    created_at       TEXT NOT NULL,
    effort           TEXT NOT NULL DEFAULT '',
    vocabulary       TEXT NOT NULL DEFAULT '',
    UNIQUE (user_id, name)
);

INSERT INTO agents_new (id, user_id, name, image, harness, model, pi_provider, state, idle_timeout_sec, memory_mb, ip, last_active_at, created_at, effort, vocabulary)
SELECT id, user_id, name, image, harness, model, pi_provider, state, idle_timeout_sec, memory_mb, ip, last_active_at, created_at, effort, vocabulary
FROM agents;

DROP TABLE agents;

ALTER TABLE agents_new RENAME TO agents;
