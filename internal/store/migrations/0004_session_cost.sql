-- Claude Code reports a running total for the whole conversation, which
-- turns used to store as their own cost. Agents now remember the last
-- total so each turn can record only its own share.
ALTER TABLE agents ADD COLUMN session_cost REAL NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN session_id TEXT NOT NULL DEFAULT '';

-- Seed each agent's running total from its latest turn, before the turns
-- themselves are converted.
UPDATE agents SET session_cost = COALESCE((
    SELECT t.cost_usd FROM turns t
    WHERE t.agent_id = agents.id AND t.harness = 'claude' AND t.status = 'ok'
    ORDER BY t.started_at DESC LIMIT 1), 0)
WHERE harness = 'claude' AND session_id <> '';

-- Turn stored totals into per-turn amounts. A total that went down marks a
-- new conversation, whose first turn already holds its own cost.
WITH c AS (
    SELECT id, cost_usd - LAG(cost_usd, 1, 0) OVER (PARTITION BY agent_id ORDER BY started_at) AS delta
    FROM turns WHERE harness = 'claude' AND status = 'ok'
)
UPDATE turns SET cost_usd = (SELECT delta FROM c WHERE c.id = turns.id)
WHERE id IN (SELECT id FROM c WHERE delta >= 0);
