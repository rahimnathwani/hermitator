-- Each chat an agent is in has a conversation of its own; the web console
-- is one more, with no account or chat. What an agent kept per
-- conversation moves here from the agents table.
CREATE TABLE chat_threads (
    agent_id       TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    account_id     TEXT NOT NULL,
    chat_jid       TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    session_cost   REAL NOT NULL DEFAULT 0, -- Claude Code's running total
    context_tokens INTEGER NOT NULL DEFAULT 0,
    review_turns   INTEGER NOT NULL DEFAULT 0,
    last_at        TEXT,
    PRIMARY KEY (agent_id, account_id, chat_jid)
);

-- An agent's conversation carries on in the chat whose message it last
-- answered; its other chats start afresh. Compactions and reviews run
-- without a chat, so they do not count.
INSERT INTO chat_threads (agent_id, account_id, chat_jid, session_id, session_cost, context_tokens, review_turns, last_at)
SELECT a.id,
       COALESCE((SELECT t.account_id FROM turns t WHERE t.agent_id = a.id AND t.session_id = a.session_id
                 AND t.id NOT LIKE 'compact-%' AND t.id NOT LIKE 'review-%' ORDER BY t.started_at DESC LIMIT 1), ''),
       COALESCE((SELECT t.chat_jid FROM turns t WHERE t.agent_id = a.id AND t.session_id = a.session_id
                 AND t.id NOT LIKE 'compact-%' AND t.id NOT LIKE 'review-%' ORDER BY t.started_at DESC LIMIT 1), ''),
       a.session_id, a.session_cost, a.context_tokens, a.review_turns, a.last_active_at
FROM agents a WHERE a.session_id <> '';

ALTER TABLE agents DROP COLUMN session_id;
ALTER TABLE agents DROP COLUMN session_cost;
ALTER TABLE agents DROP COLUMN context_tokens;
ALTER TABLE agents DROP COLUMN review_turns;

-- /sessions lists one chat's conversations, so each remembers its chat:
-- the one its first turn answered.
ALTER TABLE agent_sessions ADD COLUMN account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN chat_jid TEXT NOT NULL DEFAULT '';
UPDATE agent_sessions SET
    account_id = COALESCE((SELECT t.account_id FROM turns t WHERE t.agent_id = agent_sessions.agent_id AND t.session_id = agent_sessions.session_id
                           AND t.id NOT LIKE 'compact-%' AND t.id NOT LIKE 'review-%' ORDER BY t.started_at LIMIT 1), ''),
    chat_jid = COALESCE((SELECT t.chat_jid FROM turns t WHERE t.agent_id = agent_sessions.agent_id AND t.session_id = agent_sessions.session_id
                         AND t.id NOT LIKE 'compact-%' AND t.id NOT LIKE 'review-%' ORDER BY t.started_at LIMIT 1), '');
-- The current conversation belongs to the chat it carries on in, even if
-- it began in another.
UPDATE agent_sessions SET
    account_id = (SELECT c.account_id FROM chat_threads c WHERE c.agent_id = agent_sessions.agent_id AND c.session_id = agent_sessions.session_id),
    chat_jid = (SELECT c.chat_jid FROM chat_threads c WHERE c.agent_id = agent_sessions.agent_id AND c.session_id = agent_sessions.session_id)
WHERE EXISTS (SELECT 1 FROM chat_threads c WHERE c.agent_id = agent_sessions.agent_id AND c.session_id = agent_sessions.session_id);
CREATE INDEX agent_sessions_chat ON agent_sessions (agent_id, account_id, chat_jid, last_at);
