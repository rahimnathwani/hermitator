-- Subscription usage windows, as the harness last reported them.
CREATE TABLE usage_windows (
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source      TEXT NOT NULL, -- claude
    name        TEXT NOT NULL, -- five_hour, seven_day, ...
    utilization REAL NOT NULL,
    resets_at   TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (user_id, source, name)
);

-- Every conversation an agent has had, so /resume can go back to one.
CREATE TABLE agent_sessions (
    agent_id     TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL,
    harness      TEXT NOT NULL,
    first_prompt TEXT NOT NULL,
    turns        INTEGER NOT NULL DEFAULT 0,
    cost_total   REAL NOT NULL DEFAULT 0, -- Claude Code's running total
    started_at   TEXT NOT NULL,
    last_at      TEXT NOT NULL,
    PRIMARY KEY (agent_id, session_id)
);

INSERT INTO agent_sessions (agent_id, session_id, harness, first_prompt, turns, started_at, last_at)
SELECT t.agent_id, t.session_id, MIN(t.harness),
       (SELECT f.input FROM turns f WHERE f.agent_id = t.agent_id AND f.session_id = t.session_id ORDER BY f.started_at LIMIT 1),
       COUNT(*), MIN(t.started_at), MAX(t.started_at)
FROM turns t WHERE t.session_id <> '' GROUP BY t.agent_id, t.session_id;

UPDATE agent_sessions SET cost_total = (SELECT a.session_cost FROM agents a WHERE a.id = agent_sessions.agent_id)
WHERE session_id = (SELECT a.session_id FROM agents a WHERE a.id = agent_sessions.agent_id);
